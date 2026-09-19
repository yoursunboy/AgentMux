package event

import (
	"context"
	"time"
)

// List sizes. The service clamps a request to these; the constants live here so
// that the default and the ceiling are stated once, next to the query they
// bound.
const (
	// DefaultLimit is how many events a listing returns when the caller does
	// not say. Fifty is a screenful of timeline and one small response.
	DefaultLimit = 50

	// MaxLimit is the most a caller may ask for. A timeline is read a page at a
	// time; a request for everything is a request that grows without bound as
	// the installation ages, and it would be answered from a table that only
	// ever gets longer.
	MaxLimit = 200

	// hardLimit is the most any read may fetch: one more than a caller may ask
	// for.
	//
	// The service reads one past the page it is going to return, which is how
	// it knows whether another page exists without a second query. That extra
	// row is why the ceiling a *repository* applies is one above the ceiling a
	// *request* may name.
	hardLimit = MaxLimit + 1
)

// Cursor names a position in a timeline: one event, by the pair the ordering is
// defined on.
//
// It carries both halves because the ordering key is both halves. Events
// created in the same millisecond are ordinary - a runtime that starts produces
// two in one instant - and a cursor of a timestamp alone would either skip the
// second event or return it twice, depending on which way the comparison fell.
// Neither failure is visible in a test with one event per second in it.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Query narrows an event listing.
//
// Exactly one of ProjectID and RuntimeID is set. They are separate fields
// rather than one "scope" because they are different questions - a project's
// whole timeline, and one runtime's across the restarts it has been through -
// and because a query that could carry both would have to decide which wins.
type Query struct {
	// ProjectID selects one project's events.
	ProjectID string

	// RuntimeID selects one runtime's events.
	RuntimeID string

	// Limit is the maximum number of events to return. Zero means
	// DefaultLimit; a value above MaxLimit is treated as MaxLimit.
	Limit int

	// Before, when set, selects only events older than that position.
	Before *Cursor
}

// LimitOr returns the effective number of rows a read may fetch.
//
// It clamps to hardLimit rather than to MaxLimit because the service asks for
// one row past the page it will return. The row that decides "there is more" is
// never returned to a caller.
func (q Query) LimitOr() int {
	switch {
	case q.Limit <= 0:
		return DefaultLimit
	case q.Limit > hardLimit:
		return hardLimit
	default:
		return q.Limit
	}
}

// Repository is the persistence contract the event service depends on.
//
// It is declared here, in the package that consumes it, rather than in the
// storage package. The service states what it needs; storage satisfies it. That
// keeps the dependency pointing one way: storage imports event, never the
// reverse.
//
// There is no Update and no Delete, and their absence is the design rather than
// an omission: a stored event is never changed.
type Repository interface {
	// Create stores a new event.
	Create(ctx context.Context, e *AgentEvent) error

	// List returns events matching the query, newest first, at most
	// query.LimitOr() of them.
	List(ctx context.Context, query Query) ([]*AgentEvent, error)

	// ResolveCursor returns the position an event identifier names, or
	// ErrNotFound when no stored event carries it.
	//
	// It exists because a pagination cursor is an event id and the ordering is
	// on (created_at, id): turning the one into the other is the storage
	// layer's job, and a service that guessed the timestamp from the id's
	// spelling would be guessing about the one thing the cursor exists to get
	// exactly right.
	ResolveCursor(ctx context.Context, id string) (Cursor, error)
}
