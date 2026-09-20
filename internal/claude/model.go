package claude

import (
	"encoding/json"
	"fmt"
	"time"
)

// This file is the adapter's internal model: what AgentMux observes about a
// Claude Code session, in AgentMux's own vocabulary.
//
// # Why there is a model here at all
//
// Claude Code publishes its facts in Claude Code's format - hook payloads on a
// transport, stream-json messages on stdout - and that format is not AgentMux's
// to depend on. It is versioned by the CLI, it gains fields between releases,
// and it spells its own identifiers its own way. A product that stored those
// documents directly would have Claude's schema as its schema, and the day the
// CLI renamed a field would be the day the history changed meaning.
//
// So nothing in AgentMux outside this package reads a Claude payload. What
// crosses the boundary is an Event: a small, closed record of what was
// observed, already reduced to the fields AgentMux is willing to keep.
//
//	docs/CLAUDE_ADAPTER.md is the long form.

// Kind names one observation the adapter makes about a Claude Code session.
//
// The values are Claude's own spellings - the hook event name, or the
// "type/subtype" pair of a stream message - rather than a private vocabulary.
// A Kind is the one place where Claude's naming is allowed to survive, because
// it is the one place whose only job is to say which of Claude's facts this is;
// everything downstream of it is spelled in AgentMux's terms.
type Kind string

// The hook events this phase reads.
//
// These five are the first batch, and the list is short on purpose. Claude Code
// declares thirty-three hook events; five of them answer the questions this
// phase asks. The rest - PreToolUse, PostToolUse, SubagentStop, Notification
// and the others - are dropped rather than recorded as "something else
// happened", because an event of unknown meaning in an append-only history is
// a row nobody can ever interpret or correct.
const (
	// KindSessionStart means a Claude session began. It is delivered as a
	// command hook, because Claude Code accepts no other handler for it.
	KindSessionStart Kind = "SessionStart"

	// KindUserPromptSubmit means a prompt was submitted to the session.
	//
	// The prompt itself is not read. See docs/CLAUDE_ADAPTER.md §5.
	KindUserPromptSubmit Kind = "UserPromptSubmit"

	// KindPermissionRequest means Claude asked to use a tool.
	//
	// The adapter records that it asked and which tool it named. It does not
	// answer: no decision is made, returned, or influenced, and Claude's own
	// default applies. docs/CLAUDE_ADAPTER.md §6 is why that is a rule and not
	// an omission.
	KindPermissionRequest Kind = "PermissionRequest"

	// KindStop means the turn ended. It is *not* a statement that it succeeded.
	KindStop Kind = "Stop"

	// KindSessionEnd means the session ended.
	KindSessionEnd Kind = "SessionEnd"
)

// The stream-json messages this phase reads.
//
// They are spelled "type/subtype", which is how the CLI composes them, so that
// a Kind is greppable against a raw stream capture.
const (
	// KindInitialized is the stream's opening system/init message. It carries
	// the session id, which is how a session started outside the hook path is
	// still correlated.
	KindInitialized Kind = "system/init"

	// KindHookStarted means a hook began running.
	KindHookStarted Kind = "system/hook_started"

	// KindHookResponse means a hook finished.
	//
	// The message carries the hook's own output. That field is deliberately not
	// read: see the streamMessage type in stream.go.
	KindHookResponse Kind = "system/hook_response"

	// KindResult is the stream's final envelope for a turn.
	//
	// It is the message that says whether the turn succeeded, and it says so in
	// `is_error`. It is not the message's `subtype` that says it - that field
	// has been observed reading "success" on a turn that failed. See
	// docs/CLAUDE_ADAPTER.md §4.
	KindResult Kind = "result"
)

// Event is one observation of a Claude Code session, in AgentMux's terms.
//
// It is the whole of what crosses from Claude's world into AgentMux's, and it
// is deliberately narrower than any Claude message: there is no field here for
// a prompt, an assistant message, a tool input, or a credential, so no value of
// those kinds can be carried across by a later change to a decoder. A field
// that does not exist is a field that cannot leak.
//
// The brief calls this the ClaudeEvent. It is passed in the adapter's Config
// and remembered for correlation only - it is never copied into a payload,
// because a stored event's payload may not carry a session identifier of any
// kind. docs/CLAUDE_ADAPTER.md §5 records that collision and why it was not
// resolved by weakening the check.
type Event struct {
	// Kind is which of Claude's facts this is.
	Kind Kind

	// SessionID is Claude's own session id, as Claude reported it.
	//
	// It is AgentMux's correlation key for everything else: it is what ties a
	// hook delivery, a stream init and a final result to one session. It is
	// carried here and indexed in the adapter, and it is never written to the
	// event log.
	SessionID string

	// RuntimeID is the AgentMux runtime the session is hosted in.
	//
	// It is the value Config was given, never a value discovered from a working
	// directory, a tmux session name, or a timestamp. docs/CLAUDE_ADAPTER.md
	// §7 records why guessing it is forbidden.
	RuntimeID string

	// ProjectID is the AgentMux project the runtime belongs to, from Config.
	ProjectID string

	// Payload is the reduced, metadata-only body that will be stored if this
	// event is recorded.
	//
	// It is built here and not carried over from Claude: it is a small object
	// of names and enumerations - an event name, a tool name, a session start
	// source, a stop reason, a terminal reason. Every one of its fields is
	// chosen by name in hooks.go or stream.go, and each of those choices is a
	// decision to keep something. docs/CLAUDE_ADAPTER.md §5 lists them.
	Payload json.RawMessage

	// CreatedAt is when AgentMux observed it, on AgentMux's clock.
	//
	// It is not a timestamp from Claude's payload. A recorded time that came
	// from the observed system would be that system's clock and its timezone
	// decisions stored as fact.
	CreatedAt time.Time
}

// String renders an event for a log line.
//
// The payload is deliberately absent, for the reason AgentEvent.String gives:
// the payload is the one part of this record a decoder shaped, and a log line
// has no key check in front of it. What identifies the event is its kind, its
// session and where it came from.
func (e Event) String() string {
	return fmt.Sprintf("claude %s session=%s project=%s runtime=%s",
		e.Kind, e.SessionID, e.ProjectID, e.RuntimeID)
}

// Config is what the adapter is told when it starts.
//
// Every field is given, and none is discovered. The adapter does not look at a
// working directory, does not read a tmux session name, and does not derive an
// identity from a timestamp: those are the three ways a correlation can be
// silently wrong, and a wrong correlation in an append-only log is permanent.
// A caller that cannot say which runtime it is observing does not start an
// adapter.
type Config struct {
	// ProjectID is the AgentMux project the observed runtime belongs to.
	// Required.
	ProjectID string

	// RuntimeID is the AgentMux runtime the observed Claude session runs in.
	// Required.
	//
	// It is the correlation key that survives into the event log: a stored
	// event names its runtime and its project, and those two are what a reader
	// has to place the row. It is the reason a Claude session can be followed
	// across the log without Claude's session id ever being stored.
	RuntimeID string

	// AgentSessionID is the AgentMux agent session this is an attempt at, when
	// there is one.
	//
	// It is optional, because the task model and the Claude adapter are joined
	// by a later phase and an adapter that required an attempt record could not
	// be used before then. When it is given it is remembered with the session
	// and reported by Binding; it is never written to the event log, for the
	// reason Event's doc comment gives.
	AgentSessionID string

	// HookAddr is the local address the hook receiver binds.
	//
	// Empty means DefaultHookAddr, which is loopback on an ephemeral port. The
	// receiver is the endpoint Claude delivers hook events to; binding it
	// anywhere but loopback would be publishing an unauthenticated write path
	// to the event log. docs/CLAUDE_ADAPTER.md §8 says what that path is
	// protected by and what it is not.
	HookAddr string
}

// DefaultHookAddr is where the hook receiver binds when Config names nothing.
//
// Port zero asks the kernel for a free port, which is what makes two adapters -
// two projects, or a test suite running suites in parallel - not collide. The
// address actually bound is reported by HookURL.
const DefaultHookAddr = "127.0.0.1:0"

// Binding is what a Claude session id was correlated to.
//
// It is the adapter's mapping and nothing else: it lives in memory, it is
// rebuilt by the next adapter that observes the same session, and no part of it
// is stored. docs/CLAUDE_ADAPTER.md §5 records what is lost by that and what a
// later phase would have to add to keep it.
type Binding struct {
	// SessionID is Claude's session id, the key this binding is held under.
	SessionID string

	// ProjectID, RuntimeID and AgentSessionID are the AgentMux context the
	// session was observed in, as given in Config.
	ProjectID      string
	RuntimeID      string
	AgentSessionID string

	// FirstSeen is when the adapter first saw this session id, and
	// LastSeen is when it last did. The pair is what makes a binding that has
	// gone quiet distinguishable from one that is still being observed.
	FirstSeen time.Time
	LastSeen  time.Time

	// Events counts the observations attributed to this session.
	Events int
}
