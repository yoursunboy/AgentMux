package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kutonlagos/agentmux/internal/session"
)

// runtimeColumns is the column list every runtime read uses.
const runtimeColumns = `project_id, backend, session_name, state,
	canonical_cols, canonical_rows, created_at, updated_at, last_seen_at`

// RuntimeStore is the SQLite-backed store for runtime metadata.
type RuntimeStore struct {
	db *sql.DB
}

// RuntimeStore must satisfy the contract the runtime manager depends on.
var _ session.RuntimeStore = (*RuntimeStore)(nil)

// Save writes a runtime record, replacing any existing one for the project.
//
// It is an upsert because a runtime's record is rewritten on every settled
// state change and the caller should not have to know whether this is the
// first write. There is exactly one runtime per project, so there is nothing
// an insert-only path would protect.
func (r *RuntimeStore) Save(ctx context.Context, rec session.Record) error {
	if rec.ProjectID == "" {
		return errors.New("storage: runtime record must have a project id")
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = rec.UpdatedAt
	}
	updatedAt := rec.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_runtime (
			project_id, backend, session_name, state,
			canonical_cols, canonical_rows, created_at, updated_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET
			backend        = excluded.backend,
			session_name   = excluded.session_name,
			state          = excluded.state,
			canonical_cols = excluded.canonical_cols,
			canonical_rows = excluded.canonical_rows,
			updated_at     = excluded.updated_at,
			last_seen_at   = excluded.last_seen_at`,
		rec.ProjectID,
		rec.Backend,
		rec.Session,
		string(rec.State),
		rec.Cols,
		rec.Rows,
		formatTime(createdAt),
		formatTime(updatedAt),
		nullableTime(rec.LastSeenAt),
	)
	if err != nil {
		return &session.Error{
			Code:    session.CodeStorageFailure,
			Message: "could not store the runtime record",
			Err:     err,
		}
	}
	return nil
}

// Get returns the runtime record for a project.
//
// A missing record is session.ErrRecordNotFound rather than a storage failure:
// "this project has never had a runtime" is an ordinary state, and the most
// common one.
func (r *RuntimeStore) Get(ctx context.Context, projectID string) (session.Record, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+runtimeColumns+` FROM project_runtime WHERE project_id = ?`, projectID)
	rec, err := scanRuntime(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return session.Record{}, session.ErrRecordNotFound
		}
		return session.Record{}, wrapRuntimeError(err, "project "+projectID)
	}
	return rec, nil
}

// List returns every runtime record.
func (r *RuntimeStore) List(ctx context.Context) ([]session.Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+runtimeColumns+` FROM project_runtime ORDER BY project_id ASC`)
	if err != nil {
		return nil, &session.Error{
			Code:    session.CodeStorageFailure,
			Message: "could not list runtime records",
			Err:     err,
		}
	}
	defer rows.Close()

	records := make([]session.Record, 0, 16)
	for rows.Next() {
		rec, err := scanRuntime(rows)
		if err != nil {
			return nil, wrapRuntimeError(err, "runtime row")
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, &session.Error{
			Code:    session.CodeStorageFailure,
			Message: "could not read runtime records",
			Err:     err,
		}
	}
	return records, nil
}

// Delete removes a project's runtime record.
//
// A record that is not there is not an error: the caller asked for it to be
// gone, and it is.
func (r *RuntimeStore) Delete(ctx context.Context, projectID string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM project_runtime WHERE project_id = ?`, projectID); err != nil {
		return &session.Error{
			Code:    session.CodeStorageFailure,
			Message: "could not remove the runtime record",
			Err:     err,
		}
	}
	return nil
}

// scanRuntime reads one runtime row.
func scanRuntime(row scanRow) (session.Record, error) {
	var (
		rec        session.Record
		state      string
		createdAt  string
		updatedAt  string
		lastSeenAt sql.NullString
	)
	if err := row.Scan(
		&rec.ProjectID,
		&rec.Backend,
		&rec.Session,
		&state,
		&rec.Cols,
		&rec.Rows,
		&createdAt,
		&updatedAt,
		&lastSeenAt,
	); err != nil {
		return session.Record{}, err
	}

	rec.State = session.State(state)

	var err error
	if rec.CreatedAt, err = parseTime(createdAt); err != nil {
		return session.Record{}, err
	}
	if rec.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return session.Record{}, err
	}
	if lastSeenAt.Valid && lastSeenAt.String != "" {
		at, err := parseTime(lastSeenAt.String)
		if err != nil {
			return session.Record{}, err
		}
		rec.LastSeenAt = &at
	}
	return rec, nil
}

// wrapRuntimeError converts a scan failure into a runtime-layer error.
func wrapRuntimeError(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return session.ErrRecordNotFound
	}
	return &session.Error{
		Code:    session.CodeStorageFailure,
		Message: fmt.Sprintf("could not read %s", what),
		Err:     err,
	}
}
