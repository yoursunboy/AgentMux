package storage

import (
	"context"
	"database/sql"

	"github.com/kutonlagos/agentmux/internal/usage"
)

// usageColumns is the column list every usage read uses. Listing columns
// explicitly means adding one to the table cannot silently change the meaning
// of an existing read.
const usageColumns = `id, event_type, created_at`

// UsageStore is the SQLite-backed beta usage event log.
//
// It is append-only. Nothing updates a row and nothing deletes one: the table
// records what happened, and a record that could be edited would be a record
// nothing could count.
//
// Nothing derives anything from it. There is no projection over this table, no
// service that reads it to decide something, and no endpoint that serves it -
// which is what makes deleting every row a loss of a number rather than of
// state. See migrations/0009_usage_events.sql.
type UsageStore struct {
	db *sql.DB
}

// UsageStore must satisfy the contract the recorder depends on.
var _ usage.Repository = (*UsageStore)(nil)

// Insert records one usage event.
//
// There is no upsert and no conflict clause, because there is no case for one:
// the identifier is generated before the row exists, so two inserts of the same
// event are two rows, and the only way to produce the same identifier twice is
// for the random source to fail.
func (r *UsageStore) Insert(ctx context.Context, e usage.Event) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO usage_events (`+usageColumns+`) VALUES (?, ?, ?)`,
		e.ID,
		string(e.Type),
		formatTime(e.CreatedAt),
	)
	if err != nil {
		return &usage.Error{
			Code:    usage.CodeStorageFailure,
			Message: "could not record a usage event",
			Err:     err,
		}
	}
	return nil
}

// Count reports how many usage events are stored.
func (r *UsageStore) Count(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events`).Scan(&count); err != nil {
		return 0, &usage.Error{
			Code:    usage.CodeStorageFailure,
			Message: "could not count usage events",
			Err:     err,
		}
	}
	return count, nil
}

// List returns the most recently recorded events, newest first, up to limit.
//
// No endpoint serves this and none is planned: it exists because a test has to
// assert what actually reached the table rather than how many rows did, and
// because the one index on this table is on created_at, which is the order a
// person reading a beta's usage would ask for.
//
// A limit of zero or less returns an empty list rather than everything. The
// table has no bound of its own, and a caller that forgot the argument should
// not be handed an installation's whole history.
func (r *UsageStore) List(ctx context.Context, limit int) ([]usage.Event, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+usageColumns+` FROM usage_events
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, &usage.Error{
			Code:    usage.CodeStorageFailure,
			Message: "could not list usage events",
			Err:     err,
		}
	}
	defer rows.Close()

	var events []usage.Event
	for rows.Next() {
		var (
			event     usage.Event
			eventType string
			createdAt string
		)
		if err := rows.Scan(&event.ID, &eventType, &createdAt); err != nil {
			return nil, &usage.Error{
				Code:    usage.CodeStorageFailure,
				Message: "could not read a usage event",
				Err:     err,
			}
		}
		event.Type = usage.EventType(eventType)
		if event.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, &usage.Error{
				Code:    usage.CodeStorageFailure,
				Message: "could not read a usage event's time",
				Err:     err,
			}
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, &usage.Error{
			Code:    usage.CodeStorageFailure,
			Message: "reading usage events failed",
			Err:     err,
		}
	}
	return events, nil
}
