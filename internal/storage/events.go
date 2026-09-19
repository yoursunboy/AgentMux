package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kutonlagos/agentmux/internal/event"
)

// eventColumns is the column list every event read uses. Listing columns
// explicitly means adding one to the table cannot silently change the meaning
// of an existing read.
const eventColumns = `id, project_id, runtime_id, type, source, payload, created_at`

// EventStore is the SQLite-backed event repository.
//
// It is append-only, and that is a property of the layer rather than of this
// implementation: there is no Update and no Delete below, because there is none
// in the contract either.
type EventStore struct {
	db *sql.DB
}

// EventStore must satisfy the contract the event service depends on.
var _ event.Repository = (*EventStore)(nil)

// Create stores a new event.
func (r *EventStore) Create(ctx context.Context, ev *event.AgentEvent) error {
	if ev == nil {
		return errors.New("storage: event must not be nil")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_events (
			id, project_id, runtime_id, type, source, payload, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.ID,
		ev.ProjectID,
		nullableString(ev.RuntimeID),
		ev.Type,
		ev.Source,
		nullablePayload(ev.Payload),
		formatTime(ev.CreatedAt),
	)
	if err != nil {
		return &event.Error{
			Code:    event.CodeStorageFailure,
			Message: "could not store the event",
			Err:     err,
		}
	}
	return nil
}

// List returns events matching the query, newest first.
//
// # The ordering key is a pair
//
// `created_at DESC, id DESC` and not `created_at DESC`. This host's clock
// resolves to a millisecond or coarser, and a runtime that starts writes two
// events in the same instant as a matter of course, so a timestamp alone does
// not order the rows. It has to be a total order, because pagination walks it:
// with ties, "everything before this row" either skips the rows that tie with it
// or returns them again, and which of the two happens depends on an index the
// query planner is free to change. `id` breaks the tie the same way on every
// run.
//
// The comparison in the WHERE clause is the same pair, spelled out rather than
// written as a row value, because a row value comparison is a SQLite feature
// with a version floor and this is not the place to acquire one.
func (r *EventStore) List(ctx context.Context, query event.Query) ([]*event.AgentEvent, error) {
	var (
		clauses []string
		args    []any
	)
	switch {
	case query.ProjectID != "":
		clauses = append(clauses, "project_id = ?")
		args = append(args, query.ProjectID)
	case query.RuntimeID != "":
		clauses = append(clauses, "runtime_id = ?")
		args = append(args, query.RuntimeID)
	default:
		// Neither scope: refuse rather than return the whole table. A read
		// with no scope is a bug in the caller, and the one answer that would
		// hide it is every event AgentMux has ever recorded.
		return nil, &event.Error{
			Code:    event.CodeInvalidEvent,
			Message: "an event query must name a project or a runtime",
		}
	}
	if query.Before != nil {
		clauses = append(clauses,
			"(created_at < ? OR (created_at = ? AND id < ?))")
		at := formatTime(query.Before.CreatedAt)
		args = append(args, at, at, query.Before.ID)
	}

	statement := `SELECT ` + eventColumns + ` FROM agent_events
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY created_at DESC, id DESC
		LIMIT ?`
	args = append(args, query.LimitOr())

	rows, err := r.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, &event.Error{
			Code:    event.CodeStorageFailure,
			Message: "could not list events",
			Err:     err,
		}
	}
	defer rows.Close()

	events := make([]*event.AgentEvent, 0, 16)
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, &event.Error{
			Code:    event.CodeStorageFailure,
			Message: "could not read events",
			Err:     err,
		}
	}
	return events, nil
}

// ResolveCursor returns the position an event identifier names.
//
// It reads the whole row rather than only its timestamp, because the ordering
// key is (created_at, id) and the id is the half the caller already has. A
// lookup that returned a timestamp alone would leave the caller to guess which
// of two events in the same instant it was standing on.
func (r *EventStore) ResolveCursor(ctx context.Context, id string) (event.Cursor, error) {
	var (
		cursor    event.Cursor
		createdAt string
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT id, created_at FROM agent_events WHERE id = ?`, id).
		Scan(&cursor.ID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Cursor{}, event.ErrNotFound
	}
	if err != nil {
		return event.Cursor{}, &event.Error{
			Code:    event.CodeStorageFailure,
			Message: "could not resolve the event cursor",
			Err:     err,
		}
	}
	at, err := parseTime(createdAt)
	if err != nil {
		return event.Cursor{}, &event.Error{
			Code:    event.CodeStorageFailure,
			Message: "could not resolve the event cursor",
			Err:     err,
		}
	}
	cursor.CreatedAt = at
	return cursor, nil
}

func scanEvent(row scanRow) (*event.AgentEvent, error) {
	var (
		ev        event.AgentEvent
		runtimeID sql.NullString
		payload   sql.NullString
		createdAt string
	)
	if err := row.Scan(
		&ev.ID,
		&ev.ProjectID,
		&runtimeID,
		&ev.Type,
		&ev.Source,
		&payload,
		&createdAt,
	); err != nil {
		return nil, &event.Error{
			Code:    event.CodeStorageFailure,
			Message: "could not read an event row",
			Err:     err,
		}
	}

	if runtimeID.Valid {
		ev.RuntimeID = runtimeID.String
	}
	if payload.Valid && payload.String != "" {
		ev.Payload = []byte(payload.String)
	}

	at, err := parseTime(createdAt)
	if err != nil {
		return nil, &event.Error{
			Code:    event.CodeStorageFailure,
			Message: fmt.Sprintf("could not read event %s", ev.ID),
			Err:     err,
		}
	}
	ev.CreatedAt = at
	return &ev, nil
}

// nullableString stores an empty string as NULL.
//
// A runtime id is absent rather than empty - that is the difference between "an
// event about the project" and "an event about a runtime whose name is the
// empty string" - and NULL is how that absence is spelled in a column with no
// foreign key to imply it.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nullablePayload stores an absent payload as NULL rather than as an empty
// string, so that "no payload" reads back as no payload.
func nullablePayload(payload []byte) any {
	if len(payload) == 0 {
		return nil
	}
	return string(payload)
}
