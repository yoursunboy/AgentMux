package task

import (
	"fmt"
	"time"

	"github.com/kutonlagos/agentmux/internal/idgen"
)

// SessionIDPrefix marks an agent session identifier.
const SessionIDPrefix = "sess_"

// sessionID is the shape of a session identifier. It matches a task id's
// entropy, because the two are read in the same places.
var sessionID = idgen.Spec{Prefix: SessionIDPrefix, Bytes: 12}

// NewSessionID returns a fresh agent session identifier.
func NewSessionID() (string, error) { return sessionID.New() }

// ValidSessionID reports whether id has the shape NewSessionID produces.
func ValidSessionID(id string) bool { return sessionID.Valid(id) }

// Agent session statuses.
//
// There is no WAITING, and its absence is a decision rather than an oversight:
// it is not clear what a session would be waiting for. A session whose runtime
// has stopped is not waiting, it is over; a session whose runtime is up and
// unused is idle, and idle is what RUNNING looks like between two things
// happening. WAITING belongs to the task, where being blocked on something
// outside the work is a real and long-lived condition.
const (
	// StatusSessionCreated means the session exists and has no runtime yet.
	// It is where every session begins.
	StatusSessionCreated = "CREATED"

	// StatusSessionRunning means work is under way in the session's runtime.
	StatusSessionRunning = "RUNNING"

	// StatusSessionCompleted means the attempt succeeded. Terminal.
	StatusSessionCompleted = "COMPLETED"

	// StatusSessionFailed means the attempt did not succeed. Terminal.
	StatusSessionFailed = "FAILED"

	// StatusSessionCancelled means the attempt was abandoned. Terminal.
	StatusSessionCancelled = "CANCELLED"
)

// AgentSessionStatuses lists every session status, in lifecycle order with the
// terminal states last.
func AgentSessionStatuses() []string {
	return []string{
		StatusSessionCreated,
		StatusSessionRunning,
		StatusSessionCompleted,
		StatusSessionFailed,
		StatusSessionCancelled,
	}
}

// ValidSessionStatus reports whether s is one of the declared session statuses.
func ValidSessionStatus(s string) bool {
	switch s {
	case StatusSessionCreated, StatusSessionRunning,
		StatusSessionCompleted, StatusSessionFailed, StatusSessionCancelled:
		return true
	default:
		return false
	}
}

// SessionTerminal reports whether a status is one a session cannot leave.
func SessionTerminal(s string) bool {
	switch s {
	case StatusSessionCompleted, StatusSessionFailed, StatusSessionCancelled:
		return true
	default:
		return false
	}
}

// sessionTransitions is the session lifecycle.
//
// CREATED -> FAILED is allowed and is not an oversight. A session is created
// before its runtime is, so a runtime that never came up produces a session
// that failed without ever having started. StartedAt being nil is how that
// reads, and it is a true description of what happened.
var sessionTransitions = map[string][]string{
	StatusSessionCreated: {StatusSessionRunning, StatusSessionFailed, StatusSessionCancelled},
	StatusSessionRunning: {StatusSessionCompleted, StatusSessionFailed, StatusSessionCancelled},
}

// CanTransitionSession reports whether a session may move from one status to
// another.
func CanTransitionSession(from, to string) bool {
	for _, allowed := range sessionTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// AllowedSessionTransitions returns the statuses a session may move to from
// from, in the order they are declared.
func AllowedSessionTransitions(from string) []string {
	allowed := sessionTransitions[from]
	out := make([]string, len(allowed))
	copy(out, allowed)
	return out
}

// AgentSession is one attempt at a task.
//
// A task with two sessions is a task that was tried twice: the first attempt
// went sideways and the second is a different attempt at the same goal. Neither
// replaces the other, and the task is what says both were for.
//
// The session is the level that exists because a task and a process have
// different lifetimes. If the task held the runtime id directly, restarting the
// terminal would either overwrite the first attempt's history or force a new
// task for what is plainly the same work.
type AgentSession struct {
	// ID is the session's identity: "sess_" followed by 96 random bits in hex.
	ID string `json:"id"`

	// TaskID is the task this is an attempt at. Always present, and always a
	// task that exists.
	TaskID string `json:"taskId"`

	// RuntimeID is the runtime this attempt runs in, or empty.
	//
	// Empty is a first-class value and not a placeholder: a session is created
	// before its runtime is, and the window between the two calls is a real
	// state that this field names rather than hides. See docs/TASK_MODEL.md §4.
	//
	// It is not a foreign key. The runtime record is deleted when a runtime is
	// destroyed, and "this attempt happened, and this is the runtime it ran in"
	// has to outlive it.
	RuntimeID string `json:"runtimeId,omitempty"`

	// Status is where the attempt stands. See the StatusSession* constants.
	Status string `json:"status"`

	// StartedAt is when the attempt started, or nil.
	//
	// It is set when the session enters RUNNING, and it is nil for a session
	// that failed before it ever ran - which is the honest reading of a runtime
	// that never came up.
	StartedAt *time.Time `json:"startedAt"`

	// EndedAt is when the attempt stopped being RUNNING, or nil while it is
	// still going or before it began.
	EndedAt *time.Time `json:"endedAt"`

	// CreatedAt is when the session was created, in UTC.
	CreatedAt time.Time `json:"createdAt"`
}

// IsTerminal reports whether this session can never change status again.
func (a *AgentSession) IsTerminal() bool {
	if a == nil {
		return false
	}
	return SessionTerminal(a.Status)
}

// HasRuntime reports whether a runtime has been attached.
func (a *AgentSession) HasRuntime() bool {
	return a != nil && a.RuntimeID != ""
}

// String renders a session for a log line. It carries no user text.
func (a *AgentSession) String() string {
	if a == nil {
		return "session(nil)"
	}
	if a.RuntimeID != "" {
		return fmt.Sprintf("session(%s %s task=%s runtime=%s)",
			a.ID, a.Status, a.TaskID, a.RuntimeID)
	}
	return fmt.Sprintf("session(%s %s task=%s)", a.ID, a.Status, a.TaskID)
}

// Clone returns a deep copy, so that callers cannot mutate stored state by
// holding on to a pointer.
func (a *AgentSession) Clone() *AgentSession {
	if a == nil {
		return nil
	}
	cp := *a
	if a.StartedAt != nil {
		at := *a.StartedAt
		cp.StartedAt = &at
	}
	if a.EndedAt != nil {
		at := *a.EndedAt
		cp.EndedAt = &at
	}
	return &cp
}
