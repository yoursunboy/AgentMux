package attention

import (
	"strings"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/idgen"
	"github.com/kutonlagos/agentmux/internal/task"
)

// This file is the translation: which event demands attention, which event
// raises an action, and what an action is called.
//
// It is deliberately the only place the two vocabularies meet. Upstream of it,
// events are spelled the way the packages that produce them spell them;
// downstream, a level is one of this package's own constants. A second place
// where an event type were compared against a level would be a second place for
// them to drift.
//
// The event types are imported rather than restated, for the reason
// internal/agentstate gives: the package producing an event type is the package
// that names it, and a restated string would stop the projection silently the
// day it was renamed.

// The reasons, as fixed phrases.
//
// They are constants rather than free text at each call site, and the whole set
// of them is here. A reason describes an event type and nothing else: it never
// quotes a payload, never names a tool, and never includes anything a person
// wrote. docs/AGENT_ATTENTION.md §7.
const (
	ReasonAttemptCreated      = "attempt created"
	ReasonAttemptRunning      = "attempt running"
	ReasonAttemptCompleted    = "attempt completed"
	ReasonAttemptFailed       = "attempt failed"
	ReasonAttemptCancelled    = "attempt cancelled"
	ReasonAgentStarted        = "agent started"
	ReasonPromptSubmitted     = "prompt submitted"
	ReasonPermissionRequested = "permission requested"
	ReasonAgentCompleted      = "agent completed"
	ReasonAgentFailed         = "agent failed"
	ReasonSessionEnded        = "session ended"
)

// attentionFor returns the level and reason an event asserts.
//
// The second result reports whether the event is one this package reads at all.
// An event that is read but asserts no level returns the empty level and true,
// and the projection records that it arrived without moving the level. That
// case is `agent.completed_candidate` and nothing else.
//
// Every value here is a *reading* of an event, never a decision made from one.
// Nothing in this table blocks anything, answers anything or changes anything:
// a level is a description of how much a person needs to care, and the moment
// it started to gate something it would stop being one.
// # Why an attempt's own lifecycle is in this table
//
// §7 of the phase brief gives six rows, all of them `agent.*`. This adds the
// attempt's own events, and the reason is that without them the projection
// would miss the commonest failure there is. An agent that never launches
// produces no `agent.*` event at all: the coordinator marks the attempt FAILED,
// and the only record is a `session.status_changed`. A home screen built on
// this projection would say "nothing needs you" about a start that failed -
// which is the one answer it must never give.
//
// The session status is read from the event's own `to` field, so nothing here
// is inferred.
func attentionFor(eventType, sessionStatus string) (Level, string, bool) {
	switch eventType {
	case task.TypeSessionCreated:
		// An attempt exists and nothing has started it. There is nothing to
		// look at, and the row is created so that every attempt has a level
		// rather than the API having to explain an absence.
		return LevelNone, ReasonAttemptCreated, true

	case task.TypeSessionStatusChanged:
		return levelForSessionStatus(sessionStatus)

	case claude.TypeAgentStarted:
		return LevelNone, ReasonAgentStarted, true

	case claude.TypeAgentPromptSubmitted:
		return LevelNone, ReasonPromptSubmitted, true

	case claude.TypeAgentPermissionRequested:
		// The only level that means nothing progresses without a person.
		return LevelActionRequired, ReasonPermissionRequested, true

	case claude.TypeAgentCompleted:
		return LevelInfo, ReasonAgentCompleted, true

	case claude.TypeAgentSessionEnded:
		return LevelInfo, ReasonSessionEnded, true

	case claude.TypeAgentFailed:
		return LevelWarning, ReasonAgentFailed, true

	case claude.TypeAgentCompletedCandidate:
		// The model stopped producing. It is not a state (docs/AGENT_STATE.md
		// §2) and it is not a level either: an aborted turn, a refused tool call
		// and a crash after the last token all look the same from there, and a
		// level that said "look at this" would be saying it about all three.
		return "", "", true

	default:
		return "", "", false
	}
}

// actionFor returns the action an event raises, if it raises one.
//
// Three events raise one and the rest do not. An action is something waiting
// for a person, so an event that leaves nothing waiting raises nothing - which
// is why a started session and a submitted prompt produce a level and no
// action.
func actionFor(eventType, sessionStatus string) (ActionType, string, bool) {
	switch eventType {
	case claude.TypeAgentPermissionRequested:
		return ActionPermissionRequest, ReasonPermissionRequested, true
	case claude.TypeAgentFailed:
		return ActionViewFailure, ReasonAgentFailed, true
	case claude.TypeAgentCompleted:
		return ActionViewCompletion, ReasonAgentCompleted, true

	case task.TypeSessionStatusChanged:
		// An attempt that failed before it ever ran is as worth reading as one
		// that ran and failed - see the note on attentionFor.
		switch sessionStatus {
		case task.StatusSessionFailed:
			return ActionViewFailure, ReasonAttemptFailed, true
		case task.StatusSessionCompleted:
			return ActionViewCompletion, ReasonAttemptCompleted, true
		}
		return "", "", false

	default:
		return "", "", false
	}
}

// levelForSessionStatus maps an attempt's own status onto a level.
//
// It is deliberately not the same table as the agent state's: the state
// translates an attempt's status into what the *agent* is doing, and this
// translates it into whether a person needs to care. A cancelled attempt is
// STOPPED and quiet; a failed one is FAILED and needs looking at.
func levelForSessionStatus(sessionStatus string) (Level, string, bool) {
	switch sessionStatus {
	case task.StatusSessionCreated:
		return LevelNone, ReasonAttemptCreated, true
	case task.StatusSessionRunning:
		return LevelNone, ReasonAttemptRunning, true
	case task.StatusSessionCompleted:
		return LevelInfo, ReasonAttemptCompleted, true
	case task.StatusSessionFailed:
		return LevelWarning, ReasonAttemptFailed, true
	case task.StatusSessionCancelled:
		// Somebody stopped it, which is not something to look at. It is a level
		// rather than a skip so that the row records the attempt ending.
		return LevelNone, ReasonAttemptCancelled, true
	default:
		return "", "", false
	}
}

// actionIDSpec is the shape of an action identifier.
//
// It is declared with idgen rather than spelled out, so that an action id has
// the same shape as every other identifier this build mints, and so that the
// validator and the generator cannot drift apart.
var actionIDSpec = idgen.Spec{Prefix: "act_", Bytes: 16}

// eventIDSpec is the shape of an event identifier, which action ids are derived
// from.
var eventIDSpec = idgen.Spec{Prefix: "evt_", Bytes: 16}

// ActionIDForEvent derives an action's identity from the event that raised it.
//
// # Why it is derived rather than generated
//
// It is what makes the queue idempotent without a uniqueness rule on anything
// else. A rebuild replays the whole log; an event delivered twice would be
// projected twice; and either would put a second copy of the same action into
// the queue if the identity were random. Deriving it means the same event
// raises the same row, and the insert is a no-op the second time.
//
// It returns the empty string for an identifier that is not an event id, which
// is unreachable from the event service - it validates the id before storing -
// and is reported rather than guessed at, because an action with an invented
// identity would be an action that could be raised twice.
func ActionIDForEvent(eventID string) string {
	if !eventIDSpec.Valid(eventID) {
		return ""
	}
	id := actionIDSpec.Prefix + strings.TrimPrefix(eventID, eventIDSpec.Prefix)
	if !actionIDSpec.Valid(id) {
		return ""
	}
	return id
}

// ValidActionID reports whether id has the shape ActionIDForEvent produces.
func ValidActionID(id string) bool { return actionIDSpec.Valid(id) }

// settles reports whether an event is evidence that whatever was waiting on an
// attempt has moved on.
//
// # Why this is a property of the event and not of the state
//
// A pending permission request is resolved by the next thing that happens to
// the attempt - there is no event that says "the question was answered", and
// docs/AGENT_ATTENTION.md §3 is why. The obvious way to decide that would be to
// read the agent state and ask whether it is still WAITING_PERMISSION, and it
// is wrong for one reason: a rebuild reads the state as it is *now*, long after
// the event it is replaying, so every replayed event would see the attempt's
// final status and resolve on the first one. The rebuild would then disagree
// with the live projection about when an action was resolved, and §十七 of the
// phase brief asks for the two to agree.
//
// Reading it off the event makes the decision the same in both, because it is
// the same input both times.
//
// What advances: anything that asserts a level other than ACTION_REQUIRED. An
// event that asserts no level at all - `agent.completed_candidate` - is not
// evidence of anything; the model stopping is not the question being settled.
func settles(eventType, sessionStatus string) bool {
	level, _, read := attentionFor(eventType, sessionStatus)
	return read && level != "" && level != LevelActionRequired
}
