package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kutonlagos/agentmux/internal/agentstate"
)

// agentStateColumns is the column list every state read uses. Listing columns
// explicitly means adding one to the table cannot silently change the meaning
// of an existing read.
const agentStateColumns = `agent_session_id, project_id, runtime_id, status, last_event, last_event_at, updated_at`

// AgentStateStore is the SQLite-backed projection of the event log.
type AgentStateStore struct {
	db *sql.DB
}

// AgentStateStore must satisfy the contract the projection depends on.
var _ agentstate.Repository = (*AgentStateStore)(nil)

// Upsert records a projected state, unless the row already holds a newer one.
//
// # The two conditions, and why each is here
//
// The `WHERE` on the update clause is what makes replay idempotent and what
// makes concurrent projections agree. It compares the event's own time, not the
// wall clock of whoever is writing, so re-projecting an event cannot move the
// state backwards to where the log had already moved it on - and two events
// projected at the same moment land on the same answer whichever order they
// arrive in. Without it, a rebuild replayed alongside live traffic would
// produce a state that depended on timing.
//
// The `COALESCE(NULLIF(...))` on runtime_id is the other one. A
// `session.created` event happens before a runtime is attached and carries no
// runtime, so the plain assignment would erase the binding that
// `session.status_changed` established a moment later. The binding is set once
// and never cleared, which is the same rule `agent_sessions.runtime_id` has.
//
// The statement is a single atomic write, so nothing here needs a lock. See
// docs/AGENT_STATE.md §5.
func (r *AgentStateStore) Upsert(ctx context.Context, s agentstate.AgentState) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_states (
			agent_session_id, project_id, runtime_id, status, last_event, last_event_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_session_id) DO UPDATE SET
			project_id   = excluded.project_id,
			runtime_id   = COALESCE(NULLIF(excluded.runtime_id, ''), agent_states.runtime_id),
			status       = excluded.status,
			last_event   = excluded.last_event,
			last_event_at = excluded.last_event_at,
			updated_at   = excluded.updated_at
		WHERE agent_states.last_event_at <= excluded.last_event_at`,
		s.AgentSessionID,
		s.ProjectID,
		nullableString(s.RuntimeID),
		string(s.Status),
		s.LastEvent,
		formatTime(s.LastEventAt),
		formatTime(s.UpdatedAt),
	)
	if err != nil {
		return false, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not record an agent state",
			Err:     err,
		}
	}

	// Zero rows means the row already held something newer and the update was
	// skipped, which is a correct answer and not a failure.
	affected, err := result.RowsAffected()
	if err != nil {
		return false, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not tell whether an agent state was written",
			Err:     err,
		}
	}
	return affected > 0, nil
}

// Get returns one attempt's state.
func (r *AgentStateStore) Get(ctx context.Context, agentSessionID string) (agentstate.AgentState, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+agentStateColumns+` FROM agent_states WHERE agent_session_id = ?`,
		agentSessionID)

	state, err := scanAgentState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return agentstate.AgentState{}, &agentstate.Error{
			Code:    agentstate.CodeNotFound,
			Message: fmt.Sprintf("no event has recorded an agent state for %s", agentSessionID),
		}
	}
	return state, err
}

// ListByProject returns a project's states, most recently updated first.
func (r *AgentStateStore) ListByProject(ctx context.Context, projectID string, limit int) ([]agentstate.AgentState, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+agentStateColumns+` FROM agent_states
		WHERE project_id = ?
		ORDER BY updated_at DESC, agent_session_id DESC
		LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not list agent states",
			Err:     err,
		}
	}
	defer rows.Close()

	out := make([]agentstate.AgentState, 0, 16)
	for rows.Next() {
		state, err := scanAgentState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		return nil, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not read agent states",
			Err:     err,
		}
	}
	return out, nil
}

// ByRuntime returns the state of the attempt currently bound to a runtime.
func (r *AgentStateStore) ByRuntime(ctx context.Context, runtimeID string) (agentstate.AgentState, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+agentStateColumns+` FROM agent_states
		WHERE runtime_id = ?
		ORDER BY last_event_at DESC, agent_session_id DESC
		LIMIT 1`,
		runtimeID)

	state, err := scanAgentState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return agentstate.AgentState{}, &agentstate.Error{
			Code:    agentstate.CodeNotFound,
			Message: fmt.Sprintf("no attempt is bound to runtime %s", runtimeID),
		}
	}
	return state, err
}

// Count returns how many states the projection holds.
func (r *AgentStateStore) Count(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_states`).Scan(&count); err != nil {
		return 0, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not count the agent states",
			Err:     err,
		}
	}
	return count, nil
}

// DeleteAll removes every projected state.
func (r *AgentStateStore) DeleteAll(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM agent_states`); err != nil {
		return &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not clear the agent states",
			Err:     err,
		}
	}
	return nil
}

// rowScanner is what *sql.Row and *sql.Rows have in common.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAgentState reads one projected state.
func scanAgentState(row rowScanner) (agentstate.AgentState, error) {
	var (
		state     agentstate.AgentState
		runtimeID sql.NullString
		status    string
		lastAt    string
		updatedAt string
	)
	if err := row.Scan(
		&state.AgentSessionID,
		&state.ProjectID,
		&runtimeID,
		&status,
		&state.LastEvent,
		&lastAt,
		&updatedAt,
	); err != nil {
		// sql.ErrNoRows is the caller's to interpret, so it passes through
		// unwrapped rather than becoming a storage failure.
		if errors.Is(err, sql.ErrNoRows) {
			return agentstate.AgentState{}, err
		}
		return agentstate.AgentState{}, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not read an agent state",
			Err:     err,
		}
	}

	state.RuntimeID = runtimeID.String
	state.Status = agentstate.Status(status)

	var err error
	if state.LastEventAt, err = parseTime(lastAt); err != nil {
		return agentstate.AgentState{}, err
	}
	if state.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return agentstate.AgentState{}, err
	}
	return state, nil
}

// NewestByProjects returns the most recent state of each of the given projects.
//
// # Why this is one query
//
// A dashboard reads one state per project, and doing that a project at a time
// would be a query per project - the N+1 the controller API is explicitly not
// allowed to commit. This asks for them together: the `IN` narrows to the
// projects that were asked for, and the correlated subquery picks each one's
// newest row.
//
// "Newest" is by the event's own time, which is what last_event_at holds, so it
// is the same ordering the projection itself is guarded by. A project with no
// state is simply absent from the result, not an error.
func (r *AgentStateStore) NewestByProjects(ctx context.Context, projectIDs []string) ([]agentstate.AgentState, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}

	statement := `
		SELECT ` + agentStateColumns + ` FROM agent_states a
		WHERE a.project_id IN (` + placeholders(len(projectIDs)) + `)
		  AND a.agent_session_id = (
			SELECT b.agent_session_id FROM agent_states b
			WHERE b.project_id = a.project_id
			ORDER BY b.last_event_at DESC, b.agent_session_id DESC
			LIMIT 1
		  )`

	rows, err := r.db.QueryContext(ctx, statement, argsOf(projectIDs)...)
	if err != nil {
		return nil, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not list agent states for several projects",
			Err:     err,
		}
	}
	defer rows.Close()

	out := make([]agentstate.AgentState, 0, len(projectIDs))
	for rows.Next() {
		state, err := scanAgentState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		return nil, &agentstate.Error{
			Code:    agentstate.CodeStorageFailure,
			Message: "could not read agent states for several projects",
			Err:     err,
		}
	}
	return out, nil
}
