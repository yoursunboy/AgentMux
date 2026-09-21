package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/attention"
)

// attentionColumns is the column list every attention read uses. Listing
// columns explicitly means adding one to the table cannot silently change the
// meaning of an existing read.
const attentionColumns = `agent_session_id, project_id, level, reason, updated_at`

// actionColumns is the same for actions.
const actionColumns = `id, agent_session_id, project_id, type, status, reason, created_at, resolved_at`

// AttentionStore is the SQLite-backed projection of attention and actions.
//
// One store for both tables, because they are one subject: the same event
// raises both, the same rebuild recomputes both, and a caller that wants one
// almost always wants the other.
type AttentionStore struct {
	db *sql.DB
}

// AttentionStore must satisfy the contract the projection depends on.
var _ attention.Repository = (*AttentionStore)(nil)

// ---------------------------------------------------------------------------
// Attention
// ---------------------------------------------------------------------------

// UpsertAttention records a level, unless the row already holds one derived
// from a later event.
//
// The `WHERE` is the same guard agent_states uses and it is here for the same
// two reasons: re-projecting an event cannot move a level back to where the log
// had already moved it on, and two events projected at the same moment land on
// the same answer whichever order their writes reach the database in. The
// comparison is on the event's own time, which is what the column holds - see
// the note on Attention.UpdatedAt.
func (r *AttentionStore) UpsertAttention(ctx context.Context, a attention.Attention) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_attention (
			agent_session_id, project_id, level, reason, updated_at
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(agent_session_id) DO UPDATE SET
			project_id = excluded.project_id,
			level      = excluded.level,
			reason     = excluded.reason,
			updated_at = excluded.updated_at
		WHERE agent_attention.updated_at <= excluded.updated_at`,
		a.AgentSessionID,
		a.ProjectID,
		string(a.Level),
		a.Reason,
		formatTime(a.UpdatedAt),
	)
	if err != nil {
		return false, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not record attention",
			Err:     err,
		}
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not tell whether attention was written",
			Err:     err,
		}
	}
	return affected > 0, nil
}

// Attention returns one attempt's level.
func (r *AttentionStore) Attention(ctx context.Context, agentSessionID string) (attention.Attention, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+attentionColumns+` FROM agent_attention WHERE agent_session_id = ?`,
		agentSessionID)

	a, err := scanAttention(row)
	if errors.Is(err, sql.ErrNoRows) {
		return attention.Attention{}, &attention.Error{
			Code:    attention.CodeNotFound,
			Message: fmt.Sprintf("no event has recorded attention for %s", agentSessionID),
		}
	}
	return a, err
}

// ListAttentionByProject returns a project's levels, most recently updated
// first.
func (r *AttentionStore) ListAttentionByProject(ctx context.Context, projectID string, limit int) ([]attention.Attention, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+attentionColumns+` FROM agent_attention
		WHERE project_id = ?
		ORDER BY updated_at DESC, agent_session_id DESC
		LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not list attention",
			Err:     err,
		}
	}
	defer rows.Close()

	out := make([]attention.Attention, 0, 16)
	for rows.Next() {
		a, err := scanAttention(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not read attention",
			Err:     err,
		}
	}
	return out, nil
}

// CountAttention returns how many rows the projection holds.
func (r *AttentionStore) CountAttention(ctx context.Context) (int, error) {
	return r.count(ctx, "agent_attention")
}

// DeleteAllAttention removes every level.
func (r *AttentionStore) DeleteAllAttention(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM agent_attention`)
	if err != nil {
		return &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not clear the attention projection",
			Err:     err,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

// RaiseAction records a pending action, unless one with that identity is
// already there.
//
// The identity is derived from the event that raises it, so `DO NOTHING` is the
// whole of the idempotency: replaying an event, or a rebuild, meets its own row
// and leaves it alone. Nothing is updated on conflict, deliberately - an action
// that already exists has a status something may have changed, and a re-raise
// must not put it back to pending.
func (r *AttentionStore) RaiseAction(ctx context.Context, a attention.Action) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_actions (
			id, agent_session_id, project_id, type, status, reason, created_at, resolved_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		a.ID,
		a.AgentSessionID,
		a.ProjectID,
		string(a.Type),
		string(a.Status),
		a.Reason,
		formatTime(a.CreatedAt),
		nullableTime(a.ResolvedAt),
	)
	if err != nil {
		return false, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not raise an action",
			Err:     err,
		}
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not tell whether an action was raised",
			Err:     err,
		}
	}
	return affected > 0, nil
}

// Actions returns one attempt's actions, newest first.
func (r *AttentionStore) Actions(ctx context.Context, agentSessionID string) ([]attention.Action, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+actionColumns+` FROM agent_actions
		WHERE agent_session_id = ?
		ORDER BY created_at DESC, id DESC`,
		agentSessionID)
	if err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not list actions",
			Err:     err,
		}
	}
	defer rows.Close()
	return scanActions(rows)
}

// ListActionsByProject returns a project's actions, pending first and then
// newest first.
//
// The ordering is the queue's whole value: a list sorted only by time would put
// a settled action above one that is still waiting, and the one thing a reader
// of a queue is looking for is what is still waiting.
func (r *AttentionStore) ListActionsByProject(ctx context.Context, projectID string, limit int) ([]attention.Action, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+actionColumns+` FROM agent_actions
		WHERE project_id = ?
		ORDER BY CASE status WHEN 'PENDING' THEN 0 ELSE 1 END, created_at DESC, id DESC
		LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not list actions",
			Err:     err,
		}
	}
	defer rows.Close()
	return scanActions(rows)
}

// ResolveActions marks every pending action of the given types on one attempt
// as resolved.
func (r *AttentionStore) ResolveActions(ctx context.Context, agentSessionID string, types []attention.ActionType, at time.Time) (int, error) {
	if len(types) == 0 {
		return 0, nil
	}

	placeholders := make([]string, 0, len(types))
	args := make([]any, 0, len(types)+4)
	args = append(args, string(attention.ActionResolved), formatTime(at), agentSessionID)
	for _, t := range types {
		placeholders = append(placeholders, "?")
		args = append(args, string(t))
	}

	result, err := r.db.ExecContext(ctx, `
		UPDATE agent_actions SET status = ?, resolved_at = ?
		WHERE agent_session_id = ? AND status = 'PENDING'
		  AND type IN (`+strings.Join(placeholders, ", ")+`)`,
		args...)
	if err != nil {
		return 0, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not resolve a pending action",
			Err:     err,
		}
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not tell whether an action was resolved",
			Err:     err,
		}
	}
	return int(affected), nil
}

// CountActions returns how many actions the projection holds.
func (r *AttentionStore) CountActions(ctx context.Context) (int, error) {
	return r.count(ctx, "agent_actions")
}

// DeleteAllActions removes every action.
func (r *AttentionStore) DeleteAllActions(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM agent_actions`)
	if err != nil {
		return &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not clear the action queue",
			Err:     err,
		}
	}
	return nil
}

// count returns how many rows a projection table holds. The table name is a
// constant from this file and never a caller's, so it is interpolated.
func (r *AttentionStore) count(ctx context.Context, table string) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return 0, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not count the rows of " + table,
			Err:     err,
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

// scanAttention reads one attention row.
func scanAttention(row rowScanner) (attention.Attention, error) {
	var (
		a         attention.Attention
		level     string
		updatedAt string
	)
	if err := row.Scan(&a.AgentSessionID, &a.ProjectID, &level, &a.Reason, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attention.Attention{}, err
		}
		return attention.Attention{}, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not read attention",
			Err:     err,
		}
	}
	a.Level = attention.Level(level)

	var err error
	if a.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return attention.Attention{}, err
	}
	return a, nil
}

// scanActions reads every row a query returned.
func scanActions(rows *sql.Rows) ([]attention.Action, error) {
	out := make([]attention.Action, 0, 16)
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not read actions",
			Err:     err,
		}
	}
	return out, nil
}

// scanAction reads one action row.
func scanAction(row rowScanner) (attention.Action, error) {
	var (
		a          attention.Action
		actionType string
		status     string
		createdAt  string
		resolvedAt sql.NullString
	)
	if err := row.Scan(
		&a.ID,
		&a.AgentSessionID,
		&a.ProjectID,
		&actionType,
		&status,
		&a.Reason,
		&createdAt,
		&resolvedAt,
	); err != nil {
		return attention.Action{}, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not read an action",
			Err:     err,
		}
	}
	a.Type = attention.ActionType(actionType)
	a.Status = attention.ActionStatus(status)

	var err error
	if a.CreatedAt, err = parseTime(createdAt); err != nil {
		return attention.Action{}, err
	}
	if a.ResolvedAt, err = optionalTime(resolvedAt); err != nil {
		return attention.Action{}, err
	}
	return a, nil
}

// NewestAttentionByProjects returns the most recent level of each of the given
// projects, in one query, for the reason NewestByProjects gives.
func (r *AttentionStore) NewestAttentionByProjects(ctx context.Context, projectIDs []string) ([]attention.Attention, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}

	statement := `
		SELECT ` + attentionColumns + ` FROM agent_attention a
		WHERE a.project_id IN (` + placeholders(len(projectIDs)) + `)
		  AND a.agent_session_id = (
			SELECT b.agent_session_id FROM agent_attention b
			WHERE b.project_id = a.project_id
			ORDER BY b.updated_at DESC, b.agent_session_id DESC
			LIMIT 1
		  )`

	rows, err := r.db.QueryContext(ctx, statement, argsOf(projectIDs)...)
	if err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not list attention for several projects",
			Err:     err,
		}
	}
	defer rows.Close()

	out := make([]attention.Attention, 0, len(projectIDs))
	for rows.Next() {
		a, err := scanAttention(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not read attention for several projects",
			Err:     err,
		}
	}
	return out, nil
}

// PendingActionCounts returns how many actions are waiting, per project.
//
// It is a single grouped count rather than a read per project, and it counts
// only what is pending: a settled action is history, and a dashboard asking
// "how much is waiting here" is not asking about history.
//
// A project with nothing pending is absent from the map rather than present
// with a zero, so a caller reads the same way it reads the other batch
// lookups - see agentstate's NewestByProjects.
func (r *AttentionStore) PendingActionCounts(ctx context.Context, projectIDs []string) (map[string]int, error) {
	if len(projectIDs) == 0 {
		return map[string]int{}, nil
	}

	statement := `
		SELECT project_id, COUNT(*) FROM agent_actions
		WHERE project_id IN (` + placeholders(len(projectIDs)) + `) AND status = 'PENDING'
		GROUP BY project_id`

	rows, err := r.db.QueryContext(ctx, statement, argsOf(projectIDs)...)
	if err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not count pending actions for several projects",
			Err:     err,
		}
	}
	defer rows.Close()

	out := make(map[string]int, len(projectIDs))
	for rows.Next() {
		var (
			projectID string
			count     int
		)
		if err := rows.Scan(&projectID, &count); err != nil {
			return nil, &attention.Error{
				Code:    attention.CodeStorageFailure,
				Message: "could not read a pending action count",
				Err:     err,
			}
		}
		out[projectID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, &attention.Error{
			Code:    attention.CodeStorageFailure,
			Message: "could not read pending action counts",
			Err:     err,
		}
	}
	return out, nil
}
