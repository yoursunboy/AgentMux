// Package task implements the AgentMux task model: what somebody wants an agent
// to do, and the attempts made at it.
//
// The model has two aggregates and four levels of meaning:
//
//	Project        where the work lives
//	Task           what is wanted          this package
//	AgentSession   one attempt at it       this package
//	Runtime        the process it runs in  internal/session
//	AgentEvent     what happened           internal/event
//
// A task is not a runtime, and the confusion is the one this package exists to
// prevent. "Is the terminal up?" is a runtime question with a present-tense
// answer; "was this work finished?" is a task question whose answer stays true
// after every process involved has exited. The session is the level that makes
// the two compatible, and it is why a task can have more than one of them.
//
// Nothing here knows what an agent is doing. A status changes because a request
// changed it, and for no other reason - see docs/TASK_MODEL.md §8.
package task

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kutonlagos/agentmux/internal/idgen"
)

// IDPrefix marks a task identifier in a log line or a URL.
const IDPrefix = "task_"

// taskID is the shape of a task identifier.
//
// Twelve bytes is 96 bits. It sits between a project id (10 bytes, spoken aloud
// as part of a session name) and an event id (16 bytes, handed out as a
// pagination cursor): a task id appears in a URL and in a payload, and neither
// of those needs the extra entropy.
var taskID = idgen.Spec{Prefix: IDPrefix, Bytes: 12}

// NewID returns a fresh task identifier.
//
// It is generated rather than assigned by the database, for the reason every
// identifier in this codebase is: identity the storage engine hands out is
// identity only the storage engine knows.
func NewID() (string, error) { return taskID.New() }

// ValidID reports whether id has the shape NewID produces.
func ValidID(id string) bool { return taskID.Valid(id) }

// Task statuses.
//
// The set is deliberately six values and not a state machine's worth. Everything
// a task can be is one of these, and every one of them is a statement about the
// *work* rather than about any process pursuing it.
const (
	// StatusCreated means the task exists and nobody has started it. It is
	// where every task begins.
	StatusCreated = "CREATED"

	// StatusRunning means work is under way.
	StatusRunning = "RUNNING"

	// StatusWaiting means work is under way and is blocked on something
	// outside it - a review, a decision, another task.
	//
	// It belongs to the task and not to a session. Being blocked is a
	// long-lived condition of a goal; a session whose runtime is idle is not
	// waiting, it is idle, and idle is not a lifecycle state.
	StatusWaiting = "WAITING"

	// StatusCompleted means the work was done. Terminal.
	StatusCompleted = "COMPLETED"

	// StatusFailed means the work was attempted and did not succeed. Terminal.
	StatusFailed = "FAILED"

	// StatusCancelled means nobody is going to do this. Terminal.
	StatusCancelled = "CANCELLED"
)

// TaskStatuses lists every status, in lifecycle order with the terminal states
// last.
func TaskStatuses() []string {
	return []string{
		StatusCreated,
		StatusRunning,
		StatusWaiting,
		StatusCompleted,
		StatusFailed,
		StatusCancelled,
	}
}

// ValidTaskStatus reports whether s is one of the declared statuses.
func ValidTaskStatus(s string) bool {
	switch s {
	case StatusCreated, StatusRunning, StatusWaiting,
		StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// TaskTerminal reports whether a status is one a task cannot leave.
func TaskTerminal(s string) bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// taskTransitions is the lifecycle, and it is the whole of the rule.
//
// It is a map of allowed edges rather than a table of forbidden ones, so that a
// transition nobody thought about is refused: a graph written as "these are
// forbidden" admits every edge the author did not consider, and the edges a
// later phase adds are exactly the ones nobody considered.
//
// The three terminal statuses have no entry, which is how COMPLETED -> RUNNING
// is refused. A task reported as done and then reported as running again has
// made the first report false, and everything that read it was told something
// that turned out not to be true. Reopening finished work is a real workflow
// and a different operation; it gets its own name in a later phase or not at
// all.
//
// CREATED does not lead directly to COMPLETED. "Completed" is a claim about work
// that was done, and work that never started was not done.
var taskTransitions = map[string][]string{
	StatusCreated: {StatusRunning, StatusCancelled},
	StatusRunning: {StatusWaiting, StatusCompleted, StatusFailed, StatusCancelled},
	StatusWaiting: {StatusRunning, StatusCompleted, StatusFailed, StatusCancelled},
}

// CanTransitionTask reports whether a task may move from one status to another.
func CanTransitionTask(from, to string) bool {
	for _, allowed := range taskTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// AllowedTaskTransitions returns the statuses a task may move to from from, in
// the order they are declared. It is empty for a terminal status.
//
// It is exported because a refusal should say what would have worked, and
// because a client that renders a status control needs the same answer the
// server enforces rather than a second copy of the rule.
func AllowedTaskTransitions(from string) []string {
	allowed := taskTransitions[from]
	out := make([]string, len(allowed))
	copy(out, allowed)
	return out
}

// Task is a piece of work somebody wants an agent to do.
//
// It is not a terminal, not a runtime, not a WebSocket and not a Claude
// process. It is the thing those exist to serve.
type Task struct {
	// ID is the task's stable identity: "task_" followed by 96 random bits in
	// hex. It never changes.
	ID string `json:"id"`

	// ProjectID is the project this work belongs to. Always present, and
	// always a project that exists.
	ProjectID string `json:"projectId"`

	// Title is what somebody wants done, in their own words.
	//
	// It is free text and it is treated as such: never executed, never
	// interpolated into a shell, never interpreted as a path or as HTML. It is
	// bounded and it is not copied into the event log.
	Title string `json:"title"`

	// Status is where the work stands. See the Status* constants.
	Status string `json:"status"`

	// CreatedAt is when the task was created, in UTC.
	CreatedAt time.Time `json:"createdAt"`

	// UpdatedAt is when anything about it last changed, in UTC. It moves on a
	// status change and on a retitle.
	UpdatedAt time.Time `json:"updatedAt"`

	// CompletedAt is when it entered StatusCompleted, or nil.
	//
	// It is cleared when a task is not completed, so a FAILED task has no
	// completion time. That is the honest answer: nothing was completed.
	CompletedAt *time.Time `json:"completedAt"`
}

// IsTerminal reports whether this task can never change status again.
func (t *Task) IsTerminal() bool {
	if t == nil {
		return false
	}
	return TaskTerminal(t.Status)
}

// String renders a task for a log line.
//
// The title is left out on purpose, for the reason the event model leaves out
// its payload: a title is the one field in this model a person wrote, and a log
// is the easiest place for something that should not have been recorded to end
// up instead. What identifies a task is its id, its status, and its project.
func (t *Task) String() string {
	if t == nil {
		return "task(nil)"
	}
	return fmt.Sprintf("task(%s %s project=%s)", t.ID, t.Status, t.ProjectID)
}

// Clone returns a deep copy, so that callers cannot mutate stored state by
// holding on to a pointer.
func (t *Task) Clone() *Task {
	if t == nil {
		return nil
	}
	cp := *t
	if t.CompletedAt != nil {
		at := *t.CompletedAt
		cp.CompletedAt = &at
	}
	return &cp
}

// Title length bounds.
//
// The upper bound is a guard rather than a design: a title is a sentence
// somebody typed, and one an order of magnitude longer than a sentence is a
// paste that went wrong. It exists so that mistake becomes a rejection here
// rather than an oversized row and an oversized response.
const (
	// MinTitleLength is one character. An empty title names nothing.
	MinTitleLength = 1

	// MaxTitleLength matches the project name bound, which is the largest piece
	// of user text this API already accepted.
	MaxTitleLength = 200
)

// ValidateTitle checks a task title.
//
// It is rejected rather than sanitised, the same way a project name is: a title
// is what the user asked for, and quietly storing a trimmed or rewritten
// version means showing them something they did not write.
//
// The rules are about what can be stored, not about what a title may say. A
// title is free text; the only things refused are the ones that are not text -
// invalid UTF-8, control characters, and a length no sentence reaches.
func ValidateTitle(title string) error {
	if title == "" {
		return newError(CodeInvalidTitle, "a task title must not be empty").
			withDetail("field", "title")
	}
	if title != strings.TrimSpace(title) {
		return newError(CodeInvalidTitle,
			"a task title must not begin or end with whitespace").
			withDetail("field", "title")
	}
	if !utf8.ValidString(title) {
		return newError(CodeInvalidTitle, "a task title must be valid UTF-8").
			withDetail("field", "title")
	}
	// The bound is in runes and not in bytes, because it exists to bound what a
	// person sees, and 200 bytes is a different amount of text in every script.
	if count := utf8.RuneCountInString(title); count > MaxTitleLength {
		return newError(CodeInvalidTitle,
			"a task title must be at most %d characters, and this one is %d",
			MaxTitleLength, count).withDetail("field", "title")
	}
	for _, r := range title {
		if unicode.IsControl(r) {
			return newError(CodeInvalidTitle,
				"a task title must not contain control characters").
				withDetail("field", "title")
		}
	}
	return nil
}
