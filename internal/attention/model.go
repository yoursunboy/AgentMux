// Package attention answers whether anybody needs to look at an agent, and what
// they might do about it.
//
// # Three readings of one log
//
//	agent_events      what happened, append-only, forever
//	agent_states      what the agent is doing
//	agent_attention   whether anybody needs to care
//	agent_actions     what they might do about it
//
// Four layers, and they are deliberately four. An agent that is RUNNING and an
// agent that is WAITING_PERMISSION are both working; only one of them is waiting
// on a person. A state describes the agent, and attention describes the
// relationship between the agent and whoever is watching it. Folding the second
// into the first would make "what is it doing" and "does anybody need to act"
// the same question, and they are not.
//
// # What this package does not do
//
// It does not answer anything. Raising a PERMISSION_REQUEST action records that
// Claude asked for something; it does not allow it, deny it, or influence it in
// any way. There is no ALLOW, no DENY and no EXECUTE in the vocabulary, and
// their absence is the boundary this phase stops at rather than an unfinished
// edge: answering a permission makes AgentMux a participant in a Claude session
// rather than an observer of one, and that decision belongs to a phase with its
// own threat model.
//
// It changes nothing else either. It does not move a task, does not stop a
// runtime, and does not write an event. Everything it stores is derived, and
// the whole of both tables can be deleted and rebuilt from the log.
//
// docs/AGENT_ATTENTION.md is the long form.
package attention

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Level is how much a person needs to care about an attempt.
//
// It is a judgement about urgency, and it is the only judgement this layer
// makes. It is not a status: an agent can be RUNNING at any level, and a level
// says nothing about what the agent is doing.
type Level string

// The attention levels, from least to most demanding.
const (
	// LevelNone means there is nothing to see.
	//
	// An agent that started, or that somebody has just spoken to, is working
	// normally. It is reported rather than left absent so that every attempt has
	// a level and a client renders one shape.
	LevelNone Level = "NONE"

	// LevelActionRequired means something is blocked until a person does
	// something.
	//
	// It is the only level that means "nothing progresses without you". Today
	// exactly one event produces it - a permission request - and the reason it
	// is separate from WARNING is that a warning can be read later while this
	// cannot.
	LevelActionRequired Level = "ACTION_REQUIRED"

	// LevelWarning means something went wrong.
	//
	// A failure is worth knowing about and is not blocking: the agent stopped,
	// and nothing is waiting on anybody.
	LevelWarning Level = "WARNING"

	// LevelInfo means it is worth knowing.
	//
	// A finished turn and an ended session are ordinary events that a person
	// watching a dashboard would want to see.
	LevelInfo Level = "INFO"
)

// levels is every level, in order of how much they demand.
var levels = []Level{LevelNone, LevelActionRequired, LevelWarning, LevelInfo}

// Levels returns every level, in a stable order.
func Levels() []Level {
	out := make([]Level, len(levels))
	copy(out, levels)
	return out
}

// LevelNames returns every level as a string, for an error message.
func LevelNames() []string {
	out := make([]string, 0, len(levels))
	for _, l := range levels {
		out = append(out, string(l))
	}
	sort.Strings(out)
	return out
}

// ValidLevel reports whether s is one of the Level constants.
func ValidLevel(s string) bool {
	for _, known := range levels {
		if string(known) == s {
			return true
		}
	}
	return false
}

// String implements fmt.Stringer.
func (l Level) String() string { return string(l) }

// Attention is how much a person needs to care about one attempt.
//
// Every field is an identifier, an enumeration or a time. There is no room in
// it for a prompt, a tool input, a transcript path or a credential, and their
// absence is the design: none of those is an attention, and a row that held one
// would be a row that could never be redacted. docs/AGENT_ATTENTION.md §7.
type Attention struct {
	// AgentSessionID is the attempt this is about. It is the identity of the
	// row.
	AgentSessionID string `json:"agentSessionId"`

	// ProjectID is the project the attempt belongs to. Always present.
	ProjectID string `json:"projectId"`

	// Level is how much a person needs to care.
	Level Level `json:"level"`

	// Reason is a short fixed phrase saying why, for example "permission
	// requested". It is never a quotation from an event payload.
	Reason string `json:"reason"`

	// UpdatedAt is when the event this level came from happened.
	//
	// It is the event's own time and not the clock of whoever wrote the row,
	// which is the same choice agent_states makes for deciding which event
	// wins: it makes the projection order-independent and it makes a rebuild
	// produce what the live projection produced.
	UpdatedAt time.Time `json:"updatedAt"`
}

// String renders the attention for a log line.
func (a Attention) String() string {
	return fmt.Sprintf("attention %s (%s)", a.Level, a.Reason)
}

// Validate checks that an attention is one this package is willing to store.
func (a Attention) Validate() error {
	if err := checkIdentifier("agentSessionId", a.AgentSessionID); err != nil {
		return err
	}
	if err := checkIdentifier("projectId", a.ProjectID); err != nil {
		return err
	}
	if !ValidLevel(string(a.Level)) {
		return newError(CodeInvalidAttention,
			"%q is not an attention level; expected one of %s",
			a.Level, strings.Join(LevelNames(), ", ")).
			withDetail("field", "level")
	}
	if strings.TrimSpace(a.Reason) == "" {
		return newError(CodeInvalidAttention,
			"an attention must say why it is one").withDetail("field", "reason")
	}
	if len(a.Reason) > maxReasonLen {
		return newError(CodeInvalidAttention,
			"an attention reason must be at most %d characters", maxReasonLen).
			withDetail("field", "reason")
	}
	if a.UpdatedAt.IsZero() {
		return newError(CodeInvalidAttention,
			"an attention must carry the time of the event it came from").
			withDetail("field", "updatedAt")
	}
	return nil
}

// ActionType is a thing a person might do about an attempt.
//
// Every one of them is something a person *looks at*. None of them is something
// AgentMux does, and that is the whole of the vocabulary this phase defines: an
// action is a note that something is waiting, not a command that something
// should happen.
type ActionType string

// The action types.
const (
	// ActionPermissionRequest means Claude asked to use a tool and is blocked
	// until somebody answers it.
	//
	// **AgentMux does not answer it.** The action records that the question was
	// asked; the answer is given where it has always been given, by a person at
	// the terminal Claude is running in.
	ActionPermissionRequest ActionType = "PERMISSION_REQUEST"

	// ActionViewFailure means a turn failed and is worth reading.
	ActionViewFailure ActionType = "VIEW_FAILURE"

	// ActionViewCompletion means a turn finished and is worth reading.
	ActionViewCompletion ActionType = "VIEW_COMPLETION"
)

// actionTypes is every type, in a stable order.
var actionTypes = []ActionType{ActionPermissionRequest, ActionViewFailure, ActionViewCompletion}

// ActionTypes returns every action type, in a stable order.
func ActionTypes() []ActionType {
	out := make([]ActionType, len(actionTypes))
	copy(out, actionTypes)
	return out
}

// ValidActionType reports whether s is one of the ActionType constants.
func ValidActionType(s string) bool {
	for _, known := range actionTypes {
		if string(known) == s {
			return true
		}
	}
	return false
}

// String implements fmt.Stringer.
func (t ActionType) String() string { return string(t) }

// ActionStatus is where an action is in its life.
type ActionStatus string

// The action statuses.
const (
	// ActionPending means nothing has resolved it and it is still waiting.
	ActionPending ActionStatus = "PENDING"

	// ActionResolved means the thing it was about has moved on.
	//
	// It is reached by evidence, not by a person: the event stream carries no
	// record of anybody having looked at anything, so an action can only be
	// resolved by something happening to the attempt afterwards. See
	// docs/AGENT_ATTENTION.md §3.
	ActionResolved ActionStatus = "RESOLVED"

	// ActionExpired means it stopped being worth doing without anything
	// resolving it.
	//
	// **Nothing produces this yet.** It is in the vocabulary because an action
	// queue needs a way to say "too late to matter", and the policy that would
	// decide when - an age, a count, a project being archived - is a product
	// decision no phase has made. It is listed as unreachable rather than left
	// for somebody to discover, the way WAITING_INPUT is in agent state.
	ActionExpired ActionStatus = "EXPIRED"
)

// actionStatuses is every status, in a stable order.
var actionStatuses = []ActionStatus{ActionPending, ActionResolved, ActionExpired}

// ActionStatuses returns every action status, in a stable order.
func ActionStatuses() []ActionStatus {
	out := make([]ActionStatus, len(actionStatuses))
	copy(out, actionStatuses)
	return out
}

// ValidActionStatus reports whether s is one of the ActionStatus constants.
func ValidActionStatus(s string) bool {
	for _, known := range actionStatuses {
		if string(known) == s {
			return true
		}
	}
	return false
}

// String implements fmt.Stringer.
func (s ActionStatus) String() string { return string(s) }

// Action is a pending item on an attempt.
//
// It is raised once and stays until something resolves it, which is what
// separates it from an attention: an attention is a level that the next event
// overwrites, and an action is a row that waits.
type Action struct {
	// ID is the action's identity, derived from the event that raised it.
	//
	// Deriving it is what makes the queue idempotent: the same event raises the
	// same action, so replaying it - or a rebuild - cannot produce a second
	// row. See ActionIDForEvent.
	ID string `json:"id"`

	// AgentSessionID is the attempt this is about.
	AgentSessionID string `json:"agentSessionId"`

	// ProjectID is the project the attempt belongs to.
	ProjectID string `json:"projectId"`

	// Type is what a person might do.
	Type ActionType `json:"type"`

	// Status is where it is in its life.
	Status ActionStatus `json:"status"`

	// Reason is a short fixed phrase, for example "permission requested".
	Reason string `json:"reason"`

	// CreatedAt is when the event that raised it happened.
	CreatedAt time.Time `json:"createdAt"`

	// ResolvedAt is when it stopped being pending, or nil.
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

// String renders the action for a log line.
func (a Action) String() string {
	return fmt.Sprintf("action %s %s (%s)", a.Type, a.Status, a.ID)
}

// Pending reports whether the action is still waiting.
func (a Action) Pending() bool { return a.Status == ActionPending }

// Validate checks that an action is one this package is willing to store.
func (a Action) Validate() error {
	if !ValidActionID(a.ID) {
		return newError(CodeInvalidAction,
			"%q is not an action id", a.ID).withDetail("field", "id")
	}
	if err := checkIdentifier("agentSessionId", a.AgentSessionID); err != nil {
		return err
	}
	if err := checkIdentifier("projectId", a.ProjectID); err != nil {
		return err
	}
	if !ValidActionType(string(a.Type)) {
		return newError(CodeInvalidAction,
			"%q is not an action type", a.Type).withDetail("field", "type")
	}
	if !ValidActionStatus(string(a.Status)) {
		return newError(CodeInvalidAction,
			"%q is not an action status", a.Status).withDetail("field", "status")
	}
	if strings.TrimSpace(a.Reason) == "" || len(a.Reason) > maxReasonLen {
		return newError(CodeInvalidAction,
			"an action reason must be between 1 and %d characters", maxReasonLen).
			withDetail("field", "reason")
	}
	if a.CreatedAt.IsZero() {
		return newError(CodeInvalidAction,
			"an action must carry the time of the event that raised it").
			withDetail("field", "createdAt")
	}
	if a.Status != ActionPending && a.ResolvedAt == nil {
		return newError(CodeInvalidAction,
			"an action that is not pending must say when it stopped being").
			withDetail("field", "resolvedAt")
	}
	return nil
}

// checkIdentifier applies the shared rule for the two identifiers a row carries.
func checkIdentifier(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return newError(CodeInvalidAttention,
			"an attention row must name the %s it is about", field).
			withDetail("field", field)
	}
	if len(value) > maxIdentifierLen {
		return newError(CodeInvalidAttention,
			"a %s must be at most %d characters", field, maxIdentifierLen).
			withDetail("field", field)
	}
	return nil
}

// maxIdentifierLen bounds every identifier a row carries, as the event and
// state models do.
const maxIdentifierLen = 128

// maxReasonLen bounds a reason.
//
// The bound is not a formatting preference. A reason is a fixed phrase chosen
// from this package's own vocabulary, and a bound is what makes it impossible
// for one to become an event payload by accident: the longest phrase here is
// nineteen characters, and anything approaching this limit is not one of them.
// docs/AGENT_ATTENTION.md §7.
const maxReasonLen = 64
