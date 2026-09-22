package attention

import (
	"context"
	"time"
)

// Repository is where projected attention and actions are kept.
//
// One interface for two tables, because they are one subject: the same event
// raises both, the same rebuild recomputes both, and a caller that wants one
// almost always wants the other. Splitting them into two repositories would
// make every reader of the pair hold both.
//
// It is an interface, and the implementation is in internal/storage, for the
// reason every other model in this build does it that way: this package decides
// what a level and an action *are*, and the storage layer decides how they are
// written down.
type Repository interface {
	// --- attention ---

	// UpsertAttention records a level, unless the row already holds one derived
	// from a later event.
	//
	// The condition is the whole of what makes the projection order-independent
	// and replayable: projecting the same event twice leaves the second call
	// with nothing to do, and two events that arrive at once resolve to the
	// same answer whichever order they land in.
	UpsertAttention(ctx context.Context, a Attention) (written bool, err error)

	// Attention returns one attempt's level.
	//
	// It returns an error carrying CodeNotFound when no event has produced one.
	Attention(ctx context.Context, agentSessionID string) (Attention, error)

	// ListAttentionByProject returns a project's levels, most recently updated
	// first.
	ListAttentionByProject(ctx context.Context, projectID string, limit int) ([]Attention, error)

	// NewestAttentionByProjects returns the most recent level of each of the
	// given projects, in one query.
	//
	// It exists for the reason agentstate's NewestByProjects does: a dashboard
	// reads one level per project, and a read per project is the N+1 the
	// controller API is not allowed to commit.
	NewestAttentionByProjects(ctx context.Context, projectIDs []string) ([]Attention, error)

	// PendingActionCounts returns how many actions are waiting, per project, in
	// one grouped query. A project with nothing pending is absent from the map.
	PendingActionCounts(ctx context.Context, projectIDs []string) (map[string]int, error)

	// CountAttention returns how many rows the projection holds.
	CountAttention(ctx context.Context) (int, error)

	// DeleteAllAttention removes every level.
	DeleteAllAttention(ctx context.Context) error

	// --- actions ---

	// RaiseAction records a pending action, unless one with that identity is
	// already there.
	//
	// An action's identity is derived from the event that raises it, so this is
	// idempotent by construction: the same event raises the same row. It
	// reports whether it wrote, which is what a caller tests to tell "raised"
	// from "already raised".
	RaiseAction(ctx context.Context, a Action) (raised bool, err error)

	// Actions returns one attempt's actions, newest first.
	Actions(ctx context.Context, agentSessionID string) ([]Action, error)

	// ListActionsByProject returns a project's actions, pending first and then
	// newest first.
	ListActionsByProject(ctx context.Context, projectID string, limit int) ([]Action, error)

	// ListActions returns actions across every project, pending first and then
	// newest first.
	//
	// The ordering is deliberately the one ListActionsByProject uses. Two
	// orderings of one table is how a console and a project page come to
	// disagree about which action is at the top of the queue.
	ListActions(ctx context.Context, limit int) ([]Action, error)

	// ActionByID returns one action.
	//
	// It returns an error carrying CodeNotFound when the queue holds no such
	// action, so that a caller can tell "no such action" from "the read failed"
	// without decoding a message.
	ActionByID(ctx context.Context, id string) (Action, error)

	// PendingActionCountsByType returns how many actions are waiting, grouped by
	// type, across every project. A type with nothing pending is absent from the
	// map, as it is from PendingActionCounts.
	//
	// It is grouped by type and not by level because this layer knows types;
	// folding types into the two lists a console shows is a product decision and
	// belongs to whoever builds that view.
	PendingActionCountsByType(ctx context.Context) (map[ActionType]int, error)

	// ResolveActions marks every pending action of the given types on one
	// attempt as resolved at a time.
	//
	// It reports how many rows it changed, which is zero in the ordinary case:
	// most events resolve nothing, because most events are not evidence that
	// anything was dealt with.
	ResolveActions(ctx context.Context, agentSessionID string, types []ActionType, at time.Time) (int, error)

	// CountActions returns how many actions the projection holds.
	CountActions(ctx context.Context) (int, error)

	// DeleteAllActions removes every action.
	DeleteAllActions(ctx context.Context) error
}

// list bounds, the same shape the other models use.
const (
	// DefaultListLimit is how many rows a listing returns when nothing is
	// asked for.
	DefaultListLimit = 50

	// MaxListLimit is the ceiling a caller may ask for.
	MaxListLimit = 200
)

// LimitOr clamps a requested limit to the range this package serves.
func LimitOr(limit int) int {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	default:
		return limit
	}
}
