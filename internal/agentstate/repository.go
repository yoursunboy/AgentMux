package agentstate

import "context"

// Repository is where projected states are kept.
//
// It is an interface, and the implementation is in internal/storage, for the
// reason every other model in this build does it that way: the projection
// decides what a state *is*, and the storage layer decides how one is written
// down. A service that spoke SQL would be a service whose tests could not run
// without a database.
type Repository interface {
	// Upsert records a projected state, unless the row already holds a newer
	// one.
	//
	// It reports whether it wrote. The condition is the whole of what makes
	// replay idempotent: projecting the same event twice leaves the second call
	// with nothing to do, and two events that arrive at once resolve to the same
	// answer whichever order they land in - the later one wins, and re-applying
	// the earlier one afterwards changes nothing.
	//
	// The comparison is on the event's own time, not on the clock of whoever is
	// writing, so it holds across a restart and across a rebuild.
	Upsert(ctx context.Context, s AgentState) (written bool, err error)

	// Get returns one attempt's state.
	//
	// It returns an error carrying CodeNotFound when no event has produced one,
	// which is the ordinary answer for an attempt nothing has observed.
	Get(ctx context.Context, agentSessionID string) (AgentState, error)

	// ListByProject returns a project's states, most recently updated first.
	ListByProject(ctx context.Context, projectID string, limit int) ([]AgentState, error)

	// ByRuntime returns the state of the attempt currently bound to a runtime.
	//
	// It is how an agent event, which names a runtime and no attempt, finds the
	// row it belongs to. "Currently bound" is the most recently updated row for
	// that runtime: the binding event for a new attempt arrives after the
	// previous attempt's last one, so the newest row is the live one.
	//
	// It returns an error carrying CodeNotFound when no attempt is bound to the
	// runtime, which is what an agent started without a task produces.
	ByRuntime(ctx context.Context, runtimeID string) (AgentState, error)

	// NewestByProjects returns the most recent state of each of the given
	// projects, in one query.
	//
	// It exists because a dashboard reads one state per project, and doing that
	// a project at a time is the N+1 that the controller API is explicitly not
	// allowed to commit. A project with no state is absent from the result
	// rather than present with a zero value, so a caller cannot mistake "no
	// attempt" for "an attempt with empty fields".
	NewestByProjects(ctx context.Context, projectIDs []string) ([]AgentState, error)

	// Count returns how many states the projection holds.
	//
	// It is what decides whether a rebuild is needed at start-up: an empty
	// projection over a non-empty log is the state a first run after this phase
	// leaves behind, and it is the only condition under which the server
	// recomputes by itself.
	Count(ctx context.Context) (int, error)

	// DeleteAll removes every projected state.
	//
	// It is what a rebuild starts from. Nothing else deletes a state: a state is
	// derived, so discarding the lot and recomputing is always safe, and
	// removing one because something looked finished would be this layer
	// deciding what the log means.
	DeleteAll(ctx context.Context) error
}

// list bounds.
const (
	// DefaultListLimit is how many states a listing returns when nothing is
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
