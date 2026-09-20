package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/task"
)

// taskColumns is the column list every task read uses. Listing columns
// explicitly means adding one to the table cannot silently change the meaning
// of an existing read.
const taskColumns = `id, project_id, title, status, created_at, updated_at, completed_at`

// sessionColumns is the column list every session read uses, for the same
// reason.
const sessionColumns = `id, task_id, runtime_id, status, started_at, ended_at, created_at`

// TaskStore is the SQLite-backed task and agent session repository.
type TaskStore struct {
	db *sql.DB
}

// TaskStore must satisfy the contract the task service depends on.
var _ task.Repository = (*TaskStore)(nil)

// CreateTask stores a new task.
func (r *TaskStore) CreateTask(ctx context.Context, t *task.Task) error {
	if t == nil {
		return errors.New("storage: task must not be nil")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, project_id, title, status, created_at, updated_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID,
		t.ProjectID,
		t.Title,
		t.Status,
		formatTime(t.CreatedAt),
		formatTime(t.UpdatedAt),
		nullableTime(t.CompletedAt),
	)
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not store the task",
			Err:     err,
		}
	}
	return nil
}

// GetTask returns one task, or task.ErrNotFound.
func (r *TaskStore) GetTask(ctx context.Context, id string) (*task.Task, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, task.ErrNotFound
	}
	return t, err
}

// ListTasks returns tasks matching the query, newest first.
//
// The ordering is `created_at DESC, id DESC` rather than `created_at DESC`
// alone, for the reason internal/storage/events.go gives at length: two tasks
// created in the same millisecond are ordinary, and an ordering that is not
// total leaves the rows in an order the query planner is free to change between
// runs.
func (r *TaskStore) ListTasks(ctx context.Context, query task.TaskQuery) ([]*task.Task, error) {
	if strings.TrimSpace(query.ProjectID) == "" {
		// No scope: refuse rather than return every task AgentMux has ever
		// recorded. A read with no project is a bug in the caller, and the one
		// answer that would hide it is the whole table.
		return nil, &task.Error{
			Code:    task.CodeInvalidInput,
			Message: "a task listing must name a project",
		}
	}
	clauses := []string{"project_id = ?"}
	args := []any{query.ProjectID}
	if query.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, query.Status)
	}
	args = append(args, query.LimitOr())

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not list tasks",
			Err:     err,
		}
	}
	defer rows.Close()

	tasks := make([]*task.Task, 0, 16)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not read tasks",
			Err:     err,
		}
	}
	return tasks, nil
}

// UpdateTaskStatus applies a status change, but only while the stored row still
// holds change.From.
//
// # Why the update is conditional
//
// This is the whole of the concurrency answer, and it is deliberately one
// statement rather than a lock, a version column or a transaction the service
// has to hold open.
//
// The service reads a task, checks that the transition is legal from the status
// it read, and then writes. Between the read and the write another request can
// land, and an unconditional UPDATE would then apply a transition that was
// never legal against the state the row is actually in - written by a service
// that had checked. The window is small and it is not zero, and a test drives
// it on purpose.
//
// Requiring the stored status to still be the one the caller validated closes
// it in the database rather than in the service: either the row is still where
// the caller found it, and the change applies, or it has moved, and nothing is
// written and the caller is told it lost.
func (r *TaskStore) UpdateTaskStatus(ctx context.Context, id string, change task.TaskStatusChange) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND status = ?`,
		change.To,
		formatTime(change.At),
		// A nil completion time clears the column, which is what a task that is
		// not completed must carry: nothing was completed.
		nullableTime(change.CompletedAt),
		id,
		change.From,
	)
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not change the task status",
			Err:     err,
		}
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not change the task status",
			Err:     err,
		}
	}
	if affected == 0 {
		// Two different things produce no row: the task is gone, or its status
		// has moved. They are different answers for the caller - one is a
		// missing resource and the other is a race - so they are told apart
		// here rather than both being reported as a conflict.
		return r.taskMissOrConflict(ctx, id)
	}
	return nil
}

// taskMissOrConflict reports why a conditional task update matched no row.
func (r *TaskStore) taskMissOrConflict(ctx context.Context, id string) error {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return task.ErrNotFound
	}
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not read the task",
			Err:     err,
		}
	}
	return task.ErrStatusConflict
}

// UpdateTaskTitle rewrites a task's title and its UpdatedAt.
//
// It is not conditional, and it does not need to be: a title is written whole
// and the last writer wins. There is no transition to be legal against, which
// is exactly the difference between this and UpdateTaskStatus.
func (r *TaskStore) UpdateTaskTitle(ctx context.Context, id, title string, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET title = ?, updated_at = ? WHERE id = ?`,
		title, formatTime(at), id,
	)
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not change the task title",
			Err:     err,
		}
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not change the task title",
			Err:     err,
		}
	}
	if affected == 0 {
		return task.ErrNotFound
	}
	return nil
}

// CreateSession stores a new agent session.
func (r *TaskStore) CreateSession(ctx context.Context, s *task.AgentSession) error {
	if s == nil {
		return errors.New("storage: agent session must not be nil")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_sessions (
			id, task_id, runtime_id, status, started_at, ended_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID,
		s.TaskID,
		nullableString(s.RuntimeID),
		s.Status,
		nullableTime(s.StartedAt),
		nullableTime(s.EndedAt),
		formatTime(s.CreatedAt),
	)
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not store the agent session",
			Err:     err,
		}
	}
	return nil
}

// GetSession returns one agent session, or task.ErrSessionNotFound.
func (r *TaskStore) GetSession(ctx context.Context, id string) (*task.AgentSession, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM agent_sessions WHERE id = ?`, id)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, task.ErrSessionNotFound
	}
	return s, err
}

// ListSessions returns one task's attempts, newest first.
func (r *TaskStore) ListSessions(ctx context.Context, query task.SessionQuery) ([]*task.AgentSession, error) {
	if strings.TrimSpace(query.TaskID) == "" {
		return nil, &task.Error{
			Code:    task.CodeInvalidInput,
			Message: "a session listing must name a task",
		}
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM agent_sessions
		WHERE task_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, query.TaskID, query.LimitOr())
	if err != nil {
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not list agent sessions",
			Err:     err,
		}
	}
	defer rows.Close()

	sessions := make([]*task.AgentSession, 0, 8)
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not read agent sessions",
			Err:     err,
		}
	}
	return sessions, nil
}

// UpdateSessionStatus applies a status change, conditionally, like
// UpdateTaskStatus and for the same reason.
//
// The two timestamp columns are written differently on purpose:
//
//   - `ended_at = ?` takes the value it is given, including nil. Moving into
//     RUNNING clears the end time, which is what a session that is running
//     again must carry.
//   - `started_at = COALESCE(?, started_at)` keeps the stored value when the
//     caller supplies none. A session's start is a fact about the attempt and
//     is not rewritten by a later transition, and COALESCE says that in the
//     statement rather than in a rule the service has to remember.
func (r *TaskStore) UpdateSessionStatus(ctx context.Context, id string, change task.SessionStatusChange) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE agent_sessions
		SET status = ?, started_at = COALESCE(?, started_at), ended_at = ?
		WHERE id = ? AND status = ?`,
		change.To,
		nullableTime(change.StartedAt),
		nullableTime(change.EndedAt),
		id,
		change.From,
	)
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not change the agent session status",
			Err:     err,
		}
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not change the agent session status",
			Err:     err,
		}
	}
	if affected == 0 {
		return r.sessionMissOrConflict(ctx, id)
	}
	return nil
}

// AttachSessionRuntime binds a runtime to a session, but only while the session
// has none.
//
// `runtime_id IS NULL` is the condition, so the binding is one-way: the column
// reads "the runtime this attempt ran in", and a field that can be rewritten
// stops answering that question about the attempt it is on. A second attach
// loses the race and is reported as a conflict rather than overwriting.
func (r *TaskStore) AttachSessionRuntime(ctx context.Context, id, runtimeID string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE agent_sessions SET runtime_id = ?
		WHERE id = ? AND runtime_id IS NULL`,
		runtimeID, id,
	)
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not attach the runtime to the agent session",
			Err:     err,
		}
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not attach the runtime to the agent session",
			Err:     err,
		}
	}
	if affected == 0 {
		return r.sessionMissOrConflict(ctx, id)
	}
	return nil
}

// sessionMissOrConflict reports why a conditional session update matched no
// row.
func (r *TaskStore) sessionMissOrConflict(ctx context.Context, id string) error {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM agent_sessions WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return task.ErrSessionNotFound
	}
	if err != nil {
		return &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not read the agent session",
			Err:     err,
		}
	}
	return task.ErrStatusConflict
}

func scanTask(row scanRow) (*task.Task, error) {
	var (
		t           task.Task
		createdAt   string
		updatedAt   string
		completedAt sql.NullString
	)
	if err := row.Scan(
		&t.ID,
		&t.ProjectID,
		&t.Title,
		&t.Status,
		&createdAt,
		&updatedAt,
		&completedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not read a task row",
			Err:     err,
		}
	}

	created, err := parseTime(createdAt)
	if err != nil {
		return nil, &task.Error{
			Code: task.CodeStorageFailure, Message: "could not read the task's creation time", Err: err,
		}
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return nil, &task.Error{
			Code: task.CodeStorageFailure, Message: "could not read the task's update time", Err: err,
		}
	}
	t.CreatedAt = created
	t.UpdatedAt = updated

	if completedAt.Valid {
		at, err := parseTime(completedAt.String)
		if err != nil {
			return nil, &task.Error{
				Code:    task.CodeStorageFailure,
				Message: fmt.Sprintf("could not read the completion time of task %s", t.ID),
				Err:     err,
			}
		}
		t.CompletedAt = &at
	}
	return &t, nil
}

func scanSession(row scanRow) (*task.AgentSession, error) {
	var (
		s         task.AgentSession
		runtimeID sql.NullString
		startedAt sql.NullString
		endedAt   sql.NullString
		createdAt string
	)
	if err := row.Scan(
		&s.ID,
		&s.TaskID,
		&runtimeID,
		&s.Status,
		&startedAt,
		&endedAt,
		&createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: "could not read an agent session row",
			Err:     err,
		}
	}

	if runtimeID.Valid {
		s.RuntimeID = runtimeID.String
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return nil, &task.Error{
			Code: task.CodeStorageFailure, Message: "could not read the session's creation time", Err: err,
		}
	}
	s.CreatedAt = created

	if s.StartedAt, err = optionalTime(startedAt); err != nil {
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: fmt.Sprintf("could not read the start time of session %s", s.ID),
			Err:     err,
		}
	}
	if s.EndedAt, err = optionalTime(endedAt); err != nil {
		return nil, &task.Error{
			Code:    task.CodeStorageFailure,
			Message: fmt.Sprintf("could not read the end time of session %s", s.ID),
			Err:     err,
		}
	}
	return &s, nil
}

// optionalTime reads a nullable timestamp column.
//
// An absent value is nil rather than the zero time, because the two mean
// different things here: a session that never started has no start time, while
// a session that started in year 1 is a corrupted row.
func optionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	at, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &at, nil
}
