package task

import (
	"context"
	"time"
)

// List sizes.
//
// Tasks and sessions are created by a person, so a project's list is small in a
// way its event timeline is not - but "small" is not a bound, and an endpoint
// that answered with every task a project has ever had would grow without one.
// The cap is high enough that an ordinary project never meets it and the
// response says nothing about being capped, which is the honest design: a
// limit nobody reaches is not a limit, it is a guard.
const (
	// DefaultLimit is how many rows a listing returns when the caller does not
	// say.
	DefaultLimit = 100

	// MaxLimit is the most a caller may ask for.
	MaxLimit = 500
)

// EffectiveLimit clamps a requested page size to what a caller may ask for.
//
// It is exported because the HTTP layer reports the same number it applied, and
// a response that said one thing while the query did another is the kind of
// disagreement that only shows up in a client written against the docs.
func EffectiveLimit(requested int) int {
	switch {
	case requested <= 0:
		return DefaultLimit
	case requested > MaxLimit:
		return MaxLimit
	default:
		return requested
	}
}

// TaskQuery narrows a task listing.
type TaskQuery struct {
	// ProjectID selects one project's tasks. Required.
	ProjectID string

	// Status, when set, selects only tasks in that status.
	Status string

	// Limit is the maximum number of tasks to return. Zero means DefaultLimit;
	// a value above MaxLimit is treated as MaxLimit.
	Limit int
}

// LimitOr returns the effective number of rows a read may fetch.
func (q TaskQuery) LimitOr() int { return EffectiveLimit(q.Limit) }

// SessionQuery narrows a session listing.
type SessionQuery struct {
	// TaskID selects one task's sessions. Required.
	TaskID string

	// Limit is the maximum number of sessions to return. Zero means
	// DefaultLimit; a value above MaxLimit is treated as MaxLimit.
	Limit int
}

// LimitOr returns the effective number of rows a read may fetch.
func (q SessionQuery) LimitOr() int { return EffectiveLimit(q.Limit) }

// TaskStatusChange is what a conditional task status update writes.
//
// From is the status the caller read and validated its transition against. The
// update applies only while the row still holds it, which is how two requests
// changing the same task at once cannot produce a state neither of them asked
// for - see docs/TASK_MODEL.md §10.
type TaskStatusChange struct {
	// From is the status the caller believes the row holds.
	From string

	// To is the status to write.
	To string

	// At is the new UpdatedAt.
	At time.Time

	// CompletedAt is the new completion time: a time when To is COMPLETED, and
	// nil otherwise, so that a failed task carries no completion time.
	CompletedAt *time.Time
}

// SessionStatusChange is what a conditional session status update writes.
type SessionStatusChange struct {
	// From is the status the caller believes the row holds.
	From string

	// To is the status to write.
	To string

	// StartedAt is the new start time, or nil to leave it unset. It is set when
	// a session enters RUNNING.
	StartedAt *time.Time

	// EndedAt is the new end time, or nil. It is set when a session enters a
	// terminal status.
	EndedAt *time.Time
}

// Repository is the persistence contract the task service depends on.
//
// It is declared here, in the package that consumes it, rather than in the
// storage package. The service states what it needs; storage satisfies it. That
// keeps the dependency pointing one way: storage imports task, never the
// reverse.
//
// There is no Delete for either aggregate. Nothing in this build deletes a task
// or a session and no endpoint could; the cascades the schema declares are
// there so that the day something does, the database decides what happens to
// the rows below rather than leaving them behind.
type Repository interface {
	// CreateTask stores a new task.
	CreateTask(ctx context.Context, t *Task) error

	// GetTask returns one task, or ErrNotFound.
	GetTask(ctx context.Context, id string) (*Task, error)

	// ListTasks returns tasks matching the query, newest first.
	ListTasks(ctx context.Context, query TaskQuery) ([]*Task, error)

	// UpdateTaskStatus applies a status change, but only while the stored row
	// still holds change.From.
	//
	// It returns ErrStatusConflict when it does not, which is not an error in
	// the caller's request: it means somebody else got there first, and the
	// transition the caller validated was never applied.
	UpdateTaskStatus(ctx context.Context, id string, change TaskStatusChange) error

	// UpdateTaskTitle rewrites a task's title and its UpdatedAt.
	UpdateTaskTitle(ctx context.Context, id, title string, at time.Time) error

	// CreateSession stores a new agent session.
	CreateSession(ctx context.Context, s *AgentSession) error

	// GetSession returns one session, or ErrSessionNotFound.
	GetSession(ctx context.Context, id string) (*AgentSession, error)

	// ListSessions returns one task's sessions, newest first.
	ListSessions(ctx context.Context, query SessionQuery) ([]*AgentSession, error)

	// UpdateSessionStatus applies a status change, but only while the stored
	// row still holds change.From. It returns ErrStatusConflict when it does
	// not.
	UpdateSessionStatus(ctx context.Context, id string, change SessionStatusChange) error

	// AttachSessionRuntime binds a runtime to a session, but only while the
	// session has none.
	//
	// It is one-way and it returns ErrStatusConflict when a runtime is already
	// attached. The column reads "the runtime this attempt ran in", and a field
	// that can be rewritten is a field that no longer answers that question
	// about the attempt it is on.
	AttachSessionRuntime(ctx context.Context, id, runtimeID string) error
}
