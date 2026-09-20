package claude

import (
	"encoding/json"
	"strings"
)

// This file is the translation: which of AgentMux's event types a Claude
// observation is recorded as, and what the stored row may say.
//
// It is deliberately the only place the two vocabularies meet. Everything
// upstream of it speaks in Kinds, which are Claude's names for Claude's facts;
// everything downstream speaks in AgentMux's event types. A second place where
// the two were compared would be a second place for them to drift.

// The AgentMux event types this adapter emits.
//
// They are declared here rather than in internal/event, following
// internal/task/events.go: the package that produces a type is the package that
// names it, so that adding one is a change in one place and the reader of the
// declaration is the writer of the event.
//
// docs/AGENT_EVENTS.md §5 carries the table of what each one means and what its
// payload holds.
const (
	// TypeAgentStarted means a Claude session began.
	//
	// It comes from the `SessionStart` hook. A stream's `system/init` message
	// does not produce one - it is used to correlate the session, and recording
	// it as a second start would put two starts for one session into a history
	// that cannot be corrected. docs/CLAUDE_ADAPTER.md §4 has the ordering that
	// makes this safe.
	TypeAgentStarted = "agent.started"

	// TypeAgentPromptSubmitted means a prompt was submitted to the session.
	//
	// It records that a prompt was submitted and nothing about what it said.
	TypeAgentPromptSubmitted = "agent.prompt_submitted"

	// TypeAgentPermissionRequested means Claude asked to use a tool.
	//
	// It is a record and not a request to AgentMux: nothing reads it, nothing
	// answers it, and the permission is decided by Claude exactly as it would
	// be with no adapter running.
	TypeAgentPermissionRequested = "agent.permission_requested"

	// TypeAgentCompletedCandidate means the turn ended.
	//
	// It exists as a type of its own because `Stop` does not mean success. It
	// means the model stopped producing, which is also what an aborted turn, a
	// refused tool call and a crash after the last token look like from there.
	// Recording it as `agent.completed` would put a claim into an immutable row
	// that the row's own evidence does not support.
	//
	// The turn's outcome is recorded separately, from the stream's `result`
	// envelope, which is the only thing that reports it. A reader who wants
	// "did this work" reads `agent.completed` or `agent.failed`; a reader who
	// wants "did it stop" reads this. docs/CLAUDE_ADAPTER.md §4.
	TypeAgentCompletedCandidate = "agent.completed_candidate"

	// TypeAgentCompleted means a turn finished and reported success.
	//
	// It is decided by `is_error` being false on the result envelope, and by
	// nothing else.
	TypeAgentCompleted = "agent.completed"

	// TypeAgentFailed means a turn finished and reported failure.
	//
	// It is decided by `is_error` being true. The envelope's `subtype` is not
	// consulted: it has been observed reading "success" on a turn that failed,
	// and docs/CLAUDE_RUNTIME_VALIDATION.md §11.2 is the measurement.
	TypeAgentFailed = "agent.failed"

	// TypeAgentSessionEnded means the session ended, for whatever reason.
	//
	// It says nothing about whether the work in it succeeded. The reason Claude
	// gave is in the payload; it is an enumeration such as "clear", "logout" or
	// "prompt_input_exit", not a sentence.
	TypeAgentSessionEnded = "agent.session_ended"
)

// agentEventType reports the AgentMux event type an observation is recorded as,
// and whether it is recorded at all.
//
// Not every observation is an event. A stream's init and hook lifecycle
// messages are how the adapter follows a session, not facts about the work in
// it: the hook events they describe already arrived, in full, through the hook
// receiver, and recording them again from the stream would put a duplicate of
// every hook event into an append-only log. They are published to subscribers
// and not recorded, which is the whole of the difference.
func agentEventType(kind Kind, payload json.RawMessage) (string, bool) {
	switch kind {
	case KindSessionStart:
		return TypeAgentStarted, true
	case KindUserPromptSubmit:
		return TypeAgentPromptSubmitted, true
	case KindPermissionRequest:
		return TypeAgentPermissionRequested, true
	case KindStop:
		return TypeAgentCompletedCandidate, true
	case KindSessionEnd:
		return TypeAgentSessionEnded, true
	case KindResult:
		if resultFailed(payload) {
			return TypeAgentFailed, true
		}
		return TypeAgentCompleted, true
	default:
		return "", false
	}
}

// resultOutcome is the part of a `result` payload that decides an outcome.
//
// It has two fields and no third. `subtype` is not here, and its absence is the
// rule: it is the field a reader reaches for first, and it is the field that
// has been measured lying.
type resultOutcome struct {
	IsError        bool   `json:"isError"`
	TerminalReason string `json:"terminalReason"`
}

// resultFailed reports whether a `result` payload describes a failed turn.
//
// A payload that cannot be read is treated as a failure. That is the direction
// to fail in: the payload is built by this package and is always readable, so
// the branch is unreachable in production, and if it were ever reached the
// alternative - recording an unreadable result as a success - would be the one
// mistake the row could not be corrected for.
func resultFailed(payload json.RawMessage) bool {
	var outcome resultOutcome
	if err := json.Unmarshal(payload, &outcome); err != nil {
		return true
	}
	return outcome.IsError
}

// maxFieldLen bounds one string inside a payload.
//
// Every string the adapter keeps is an identifier or an enumeration, and the
// longest of Claude's enumerations is a handful of characters. The bound is
// here rather than trusted: the values arrive from outside, and a payload is
// bounded at 4 KiB by the event service, so a value that grew without limit
// would turn a recorded event into a refused one. Truncation is visible in the
// value rather than silent.
const maxFieldLen = 128

// clip bounds a value and marks that it was bounded.
func clip(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxFieldLen {
		return value
	}
	return value[:maxFieldLen] + "..."
}

// payloadOf renders a payload from named fields.
//
// It takes a map rather than a struct so that the set of fields a given event
// carries is visible at the call site, where the decision to keep each one is
// made, instead of in a type shared by events that keep different things.
//
// A field whose value is empty is dropped rather than stored as an empty
// string. The difference matters to a reader: "this event did not say" and
// "this event said nothing" are not the same fact, and only the first is true.
//
// Marshal of a map of strings and booleans cannot fail, and a nil result would
// be an empty payload rather than a lost event, so the error is not propagated.
func payloadOf(fields map[string]any) json.RawMessage {
	for key, value := range fields {
		if text, ok := value.(string); ok && text == "" {
			delete(fields, key)
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return encoded
}
