// Package agentstate projects the event log into what is true about an agent
// now.
//
// # What this is
//
// An event is a fact about the past that never changes. A state is a reading of
// those facts that changes constantly, and the two answer different questions:
//
//	agent.permission_requested     something asked for a permission, at 10:04:11
//	WAITING_PERMISSION             it is waiting for one, right now
//
// The event is not the state and does not become one. This package folds the
// first into the second as events arrive, and stores the result, so that asking
// what an agent is doing costs one row rather than the whole history.
//
//	ClaudeAdapter ──▶ event.Service.CreateEvent ──▶ Projection ──▶ agent_states
//	                                                                    │
//	                                            GET /api/…/state ───────┘
//
// # What it deliberately is not
//
// It is not the runtime's state and it is not the task's. `internal/session`
// says whether a terminal exists; `internal/task` says what somebody wants done
// and whether they have said it is finished. This says what the agent reported
// about itself. Three layers, three vocabularies, and a phase that merges any
// two of them loses the ability to tell them apart.
//
// It changes nothing. It does not move a task, does not stop a runtime, does not
// answer a permission and does not write an event. Every row here is derived,
// and the whole table can be deleted and rebuilt from the log without anything
// else noticing - which is the property docs/AGENT_STATE.md §4 is built on.
package agentstate

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status is what an agent is doing, as far as the event log says.
//
// It is a closed set. A status invented at a call site would be a value nothing
// else could interpret, and the one operation this vocabulary has to support is
// comparing two readings of it.
type Status string

// The agent's states.
//
// They are the agent's own and are deliberately not the runtime's or the task's.
// A runtime is RUNNING whenever its terminal exists, whether or not anything is
// in it; a task is COMPLETED when a person says so. Three layers, three sets of
// words.
const (
	// StatusCreated means the attempt exists and nothing has started it.
	//
	// It comes from the attempt being recorded, not from the agent: an
	// AgentSession is created before a runtime is attached to it, and this is
	// the state of that window.
	StatusCreated Status = "CREATED"

	// StatusRunning means the agent is working.
	//
	// It is what a started session and a submitted prompt both produce. There is
	// deliberately no distinct "thinking" state: the event vocabulary has
	// nothing that reports one, and a state nothing can produce is a state that
	// would only ever be guessed.
	StatusRunning Status = "RUNNING"

	// StatusWaitingInput means the agent is waiting for a person to say
	// something.
	//
	// **Nothing produces this yet.** It is in the vocabulary because it is a
	// real thing for an agent to be doing and the API should not have to grow a
	// new word for it later, but no event this build records reports it - Claude
	// Code's idle notification is not among the hooks the adapter reads. A
	// status in the list that nothing produces is stated rather than hidden;
	// docs/AGENT_STATE.md §6.
	StatusWaitingInput Status = "WAITING_INPUT"

	// StatusWaitingPermission means the agent asked to use a tool and is
	// blocked on the answer.
	//
	// It is a *record* of the asking. AgentMux does not answer it, does not
	// influence it and cannot see the answer: the permission is decided where
	// it was always decided, and the state moves on when the next event arrives.
	StatusWaitingPermission Status = "WAITING_PERMISSION"

	// StatusCompleted means the turn finished and reported success.
	//
	// It is not the same as the attempt being over, and it is not the same as
	// the task being done.
	StatusCompleted Status = "COMPLETED"

	// StatusFailed means the turn finished and reported failure.
	StatusFailed Status = "FAILED"

	// StatusStopped means the session ended and nothing is running any more.
	StatusStopped Status = "STOPPED"
)

// agentStatuses is every status, in the order the API lists them.
var agentStatuses = []Status{
	StatusCreated,
	StatusRunning,
	StatusWaitingInput,
	StatusWaitingPermission,
	StatusCompleted,
	StatusFailed,
	StatusStopped,
}

// Statuses returns every status, in a stable order.
func Statuses() []Status {
	out := make([]Status, len(agentStatuses))
	copy(out, agentStatuses)
	return out
}

// StatusNames returns every status as a string, for an error message.
func StatusNames() []string {
	out := make([]string, 0, len(agentStatuses))
	for _, s := range agentStatuses {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

// ValidStatus reports whether s is one of the Status constants.
func ValidStatus(s string) bool {
	for _, known := range agentStatuses {
		if string(known) == s {
			return true
		}
	}
	return false
}

// Terminal reports whether a status is one the agent does not leave.
//
// It is used to describe a state, never to decide anything: nothing in this
// build refuses an event because the state looks finished, because an event
// that arrived is an event that happened whatever the row said a moment before.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusStopped:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (s Status) String() string { return string(s) }

// AgentState is what is true about one attempt, as of its last projected event.
//
// Every field is either an identifier or an enumeration. There is no room in it
// for a prompt, a tool input, a transcript path or a token count, and their
// absence is the design rather than an omission: none of those is a state, and
// a row that held one would be a row that could never be redacted.
// docs/AGENT_STATE.md §7.
type AgentState struct {
	// AgentSessionID is the attempt this is the state of. It is the identity of
	// the row.
	AgentSessionID string `json:"agentSessionId"`

	// ProjectID is the project the attempt belongs to. Always present.
	ProjectID string `json:"projectId"`

	// RuntimeID is the runtime the attempt ran in, or empty until it is bound
	// to one.
	RuntimeID string `json:"runtimeId,omitempty"`

	// Status is what the agent is doing.
	Status Status `json:"status"`

	// LastEvent is the type of the event that last moved this row. It is a type
	// and never a payload.
	LastEvent string `json:"lastEvent"`

	// LastEventAt is when that event happened, as the event recorded it.
	LastEventAt time.Time `json:"lastEventAt"`

	// UpdatedAt is when this row was last written.
	UpdatedAt time.Time `json:"updatedAt"`
}

// Clone returns a copy, so that a caller cannot reach the stored value.
func (s AgentState) Clone() AgentState { return s }

// String renders the state for a log line.
//
// It names the status and the event that produced it, and nothing else. The
// identifiers are left out for the reason every other String in this build
// leaves them out: a log line is copied around, and the row's own fields are
// what a reader goes to the database for.
func (s AgentState) String() string {
	return fmt.Sprintf("agent state %s (via %s)", s.Status, s.LastEvent)
}

// Validate checks that a state is one this package is willing to store.
//
// It is the same rule the other models apply: an identifier that cannot be
// stored is a row that cannot be read, and the place to notice is before the
// write rather than after it.
func (s AgentState) Validate() error {
	if strings.TrimSpace(s.AgentSessionID) == "" {
		return newError(CodeInvalidState,
			"an agent state must name the attempt it is about").
			withDetail("field", "agentSessionId")
	}
	if len(s.AgentSessionID) > maxIdentifierLen {
		return newError(CodeInvalidState,
			"an agent session id must be at most %d characters", maxIdentifierLen).
			withDetail("field", "agentSessionId")
	}
	if strings.TrimSpace(s.ProjectID) == "" {
		return newError(CodeInvalidState,
			"an agent state must name the project its attempt belongs to").
			withDetail("field", "projectId")
	}
	if len(s.ProjectID) > maxIdentifierLen {
		return newError(CodeInvalidState,
			"a project id must be at most %d characters", maxIdentifierLen).
			withDetail("field", "projectId")
	}
	if len(s.RuntimeID) > maxIdentifierLen {
		return newError(CodeInvalidState,
			"a runtime id must be at most %d characters", maxIdentifierLen).
			withDetail("field", "runtimeId")
	}
	if !ValidStatus(string(s.Status)) {
		return newError(CodeInvalidState,
			"%q is not an agent status; expected one of %s",
			s.Status, strings.Join(StatusNames(), ", ")).
			withDetail("field", "status")
	}
	if strings.TrimSpace(s.LastEvent) == "" {
		return newError(CodeInvalidState,
			"an agent state must name the event that produced it").
			withDetail("field", "lastEvent")
	}
	if s.LastEventAt.IsZero() {
		return newError(CodeInvalidState,
			"an agent state must carry the time of the event that produced it").
			withDetail("field", "lastEventAt")
	}
	return nil
}

// maxIdentifierLen bounds every identifier a state carries.
//
// It is the same bound the event model uses and for the same reason: a project
// id is 23 characters and a session name is 27, so a caller passing something
// an order of magnitude longer has passed the wrong value.
const maxIdentifierLen = 128
