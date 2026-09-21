package agentstate

import (
	"encoding/json"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/task"
)

// This file is the translation: which event asserts which status, and which
// event names the attempt it is about.
//
// It is deliberately the only place the two vocabularies meet. Upstream of it,
// events are spelled the way the packages that produce them spell them;
// downstream, a state is one of this package's own constants. A second place
// where an event type were compared against a status would be a second place
// for them to drift.
//
// # Why the event types are imported rather than restated
//
// Phase 7.3B-1 settled that the package producing an event type is the package
// that names it. Restating seven strings here would make this package the
// second owner of them, and the failure that produces is quiet: a rename in the
// adapter would leave a projection that compiles, runs, and silently stops
// projecting anything. Importing costs a dependency on the package that
// observes Claude, which is honest - this is a projection of what that
// observer reports, and it has no meaning without it.

// statusFor returns the status an event puts an attempt into.
//
// The second result reports whether the event is one this package projects. An
// event that is projected but asserts no status returns the empty status and
// true, and the projection records that it arrived without moving the state.
// That case is `agent.completed_candidate` and nothing else, and §6 of the
// phase brief is the reason it exists.
//
// Every value here is a *reading* of an event, never a decision made from one.
// Nothing in this table refuses an event, blocks a transition or decides what a
// state may become next: an event that arrived is an event that happened, and a
// projection that started enforcing a lifecycle would be inventing rules the
// log does not have.
func statusFor(eventType string) (Status, bool) {
	switch eventType {
	case claude.TypeAgentStarted,
		claude.TypeAgentPromptSubmitted:
		// A session began, or somebody said something to it. Both mean the
		// agent is working. There is no separate "starting" state: the moment
		// the session exists it is running, and a state that lasted between two
		// events nothing distinguishes would be a state nothing could observe.
		return StatusRunning, true

	case claude.TypeAgentPermissionRequested:
		// It asked for something and is blocked on the answer. This is a record
		// of the asking; the answer is not an event this build records, so the
		// state moves on when the next thing happens rather than when the
		// question is settled.
		return StatusWaitingPermission, true

	case claude.TypeAgentCompleted:
		return StatusCompleted, true

	case claude.TypeAgentFailed:
		return StatusFailed, true

	case claude.TypeAgentSessionEnded:
		// The session ended, for whatever reason. It says nothing about whether
		// the work succeeded, which is why it is not COMPLETED: a session that
		// was killed and one that finished cleanly both end here.
		return StatusStopped, true

	case claude.TypeAgentCompletedCandidate:
		// The model stopped producing. That is not the work succeeding - an
		// aborted turn, a refused tool call and a crash after the last token
		// all look the same from the `Stop` hook - so it moves nothing and only
		// records that it was the last thing seen. docs/AGENT_STATE.md §3.
		return "", true

	default:
		return "", false
	}
}

// statusForSession maps an attempt's own status onto the agent's.
//
// The task model and this one describe different things and happen to share
// some words, which is exactly why the translation is written out rather than
// cast: CANCELLED and STOPPED are the same fact seen from two sides, and
// COMPLETED means "the attempt is over" to the task model and "the turn
// succeeded" here. A cast would have made those the same thing.
func statusForSession(sessionStatus string) (Status, bool) {
	switch sessionStatus {
	case task.StatusSessionCreated:
		return StatusCreated, true
	case task.StatusSessionRunning:
		return StatusRunning, true
	case task.StatusSessionCompleted:
		return StatusCompleted, true
	case task.StatusSessionFailed:
		return StatusFailed, true
	case task.StatusSessionCancelled:
		// Somebody stopped it. The agent is not running any more and nothing
		// said it finished, which is STOPPED and not COMPLETED.
		return StatusStopped, true
	default:
		return "", false
	}
}

// sessionPayload is the part of a session event that names the attempt.
//
// It is a decoding edge and it has one field. A `session.status_changed`
// payload also carries `from` and `to`, and the `to` is read separately; the
// rest of the payload is not decoded, held or stored.
type sessionPayload struct {
	// AgentSession is the attempt's id.
	//
	// It is spelled `agentSession` because the field it replaced - `sessionId` -
	// was refused by the event service: any field whose normalised name ends in
	// `sessionid` is treated as a session credential. Phase 7.3B-2 renamed it
	// and docs/AGENT_RUNTIME_BINDING.md §7 is the account.
	AgentSession string `json:"agentSession"`

	// To is the status a session status change moved to.
	To string `json:"to"`
}

// sessionPayloadOf reads the attempt and the target status out of a session
// event.
//
// A payload that cannot be decoded yields nothing rather than an error: the
// event is already stored, and a projection that failed on it would be a
// projection that could not be re-run. The rebuild would meet the same payload
// and make the same choice, which is the property that matters.
func sessionPayloadOf(payload json.RawMessage) (sessionPayload, bool) {
	if len(payload) == 0 {
		return sessionPayload{}, false
	}
	var decoded sessionPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return sessionPayload{}, false
	}
	if decoded.AgentSession == "" {
		return sessionPayload{}, false
	}
	return decoded, true
}
