package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file receives Claude Code's hook events.
//
// # The two transports, and why there are two
//
// Claude Code delivers a hook over whichever handler type its configuration
// declares: a command that it runs, a prompt, an agent, an HTTP request, or an
// MCP tool. Two of those are usable by an adapter - a command it runs and an
// HTTP request it makes - and the choice between them is not AgentMux's.
//
// `SessionStart` accepts a command or an MCP tool handler and **refuses an HTTP
// handler**. That is not a documented preference; it was reproduced in Phase
// 7.3B-0 as a negative control, in a configuration where three other events
// were delivered over HTTP normally. So the receiver is reached two ways, and
// it is one receiver servicing both: the command handler runs `curl` against
// the same endpoint the HTTP handlers post to. Claude's payload is the same
// either way, so nothing downstream can tell which transport carried it.
//
// The requirement this creates is honest and is recorded rather than hidden:
// starting a session through this adapter needs `curl` on PATH inside the
// environment Claude runs in. docs/CLAUDE_ADAPTER.md §9.
//
// # What is read
//
// The five fields of hookPayload and nothing else. A Claude hook payload also
// carries the prompt, the tool input, the transcript path, the last assistant
// message and the working directory; none of them has a field here, so none of
// them is decoded, held, logged or stored. That is the difference between
// filtering a payload and not reading it: a filter is a decision made once per
// field and revisitable by mistake, and an absent field is not.

// hookMaxBody bounds a hook request body.
//
// A Claude hook payload is a few hundred bytes; the prompt inside a
// `UserPromptSubmit` one is what makes it large in the worst case. The bound is
// applied to the request rather than to the decoded value, so an oversized
// delivery is refused while it is still a stream and never becomes a string in
// this process.
const hookMaxBody = 256 << 10

// hookPayload is the whole of what the adapter reads out of a Claude hook.
//
// Every field is safe to keep: an identifier, an event name, and three short
// enumerations. The names are Claude's, spelled as Claude spells them, because
// this struct is the decoding edge and its job is to match the wire.
type hookPayload struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`

	// Source is the `SessionStart` source: "startup", "resume", "clear" or
	// "compact".
	Source string `json:"source"`

	// Reason is the `SessionEnd` reason, an enumeration such as "clear",
	// "logout" or "prompt_input_exit".
	Reason string `json:"reason"`

	// ToolName is the tool a `PermissionRequest` names, for example "Bash".
	//
	// It is the tool's *name*. The arguments it was called with are in
	// `tool_input`, which this struct does not have a field for.
	ToolName string `json:"tool_name"`
}

// hookKinds maps Claude's hook event name to the Kind the adapter records.
//
// The map is the allowed list, and a name that is not in it is dropped. Claude
// Code declares thirty-three hook events; this phase reads five, and the other
// twenty-eight are not recorded as anything. A settings file that declares more
// than these five therefore loses nothing that was ever going to be kept, and a
// later phase adds to the map rather than changing it.
var hookKinds = map[string]Kind{
	"SessionStart":      KindSessionStart,
	"UserPromptSubmit":  KindUserPromptSubmit,
	"PermissionRequest": KindPermissionRequest,
	"Stop":              KindStop,
	"SessionEnd":        KindSessionEnd,
}

// eventFromHook reduces a Claude hook payload to an observation.
//
// It reports false for an event name this phase does not read, which is the
// ordinary case for a hook Claude fires that AgentMux never asked for.
func eventFromHook(p hookPayload, projectID, runtimeID string, now time.Time) (Event, bool) {
	kind, ok := hookKinds[p.HookEventName]
	if !ok {
		return Event{}, false
	}
	return Event{
		Kind:      kind,
		SessionID: clip(p.SessionID),
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Payload:   hookPayloadOf(kind, p),
		CreatedAt: now,
	}, true
}

// hookPayloadOf is the stored shape of a hook event.
//
// The fields are chosen per event, and the rule for choosing is the one in
// docs/CLAUDE_ADAPTER.md §5: an identifier or an enumeration, never content.
// `prompt` and `tool_input` are the two that matter, and neither has a field
// here to be put in.
func hookPayloadOf(kind Kind, p hookPayload) json.RawMessage {
	fields := map[string]any{"event": string(kind)}
	switch kind {
	case KindSessionStart:
		fields["source"] = clip(p.Source)
	case KindPermissionRequest:
		fields["tool"] = clip(p.ToolName)
	case KindSessionEnd:
		fields["reason"] = clip(p.Reason)
	}
	return payloadOf(fields)
}

// hookReceiver is the endpoint every hook is delivered to, over both
// transports.
type hookReceiver struct {
	adapter *Adapter

	// path is the receiver's own path, carrying the nonce minted at Start.
	path string
}

// ServeHTTP accepts one hook delivery.
//
// # The response is always empty
//
// A Claude hook's stdout *is* its answer: what a hook prints is read as a
// decision to allow, a decision to deny, or context to add. This phase makes no
// decision - it records and returns - so the body is empty on every status,
// including the error ones.
//
// The error statuses matter as much as the success one. A `SessionStart` hook
// is delivered by running `curl` against this endpoint, and `curl` prints a
// response body to stdout by default. A "400 bad request" body would therefore
// be handed back to Claude as SessionStart context - an adapter's own error
// message becoming part of a session's prompt. Empty bodies everywhere is what
// makes that impossible rather than unlikely.
//
// # The write happens before the response
//
// A hook's delivery is not acknowledged until the event has been offered to the
// event service. That ordering is deliberate: it means a delivered hook is a
// recorded hook, and a caller that sees a 200 is not racing a background write.
// The write is a single local SQLite insert, and the cost of it is the cost of
// the hook.
func (h *hookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// The nonce is the path. A request that does not carry it is not from the
	// settings file this adapter generated, and is answered without being read.
	if r.URL.Path != h.path {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// The receiver outlives the adapter it serves by however long the server
	// takes to stop, and it is the adapter that holds the project and runtime a
	// delivery has to be attributed to. The two are read together, under the
	// adapter's mutex, because Start and Stop replace both - reading the fields
	// directly here would be a race with a restart, and the value read could be
	// half of one session's configuration and half of another's.
	//
	// This decides the delivery that arrives after Stop, not one already past
	// this line: a hook delivered while the adapter was recording is recorded,
	// which is right, because it did arrive.
	cfg, started := h.adapter.configNow()
	if !started {
		// Nothing is recording. The status says so, and the body is empty for
		// the reason every other status here is: a body printed by a `curl`
		// command hook would be read back by Claude as session context.
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hookMaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.adapter.log.Warn("a claude hook delivery was larger than the receiver accepts",
				"limit", hookMaxBody)
		} else {
			h.adapter.log.Warn("could not read a claude hook delivery", "error", err)
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	var payload hookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// The error is not echoed. It would be a body, and a body is answered
		// to Claude - see the note above.
		h.adapter.log.Warn("a claude hook delivery was not a JSON object", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ev, ok := eventFromHook(payload, cfg.ProjectID, cfg.RuntimeID, h.adapter.now())
	if !ok {
		// A hook this phase does not read. It is answered successfully, because
		// refusing it would tell Claude something went wrong when nothing did.
		h.adapter.log.Debug("ignoring a claude hook this adapter does not read",
			"event", payload.HookEventName)
		w.WriteHeader(http.StatusOK)
		return
	}

	h.adapter.observe(r.Context(), ev)
	w.WriteHeader(http.StatusOK)
}

// HookURL is the URL the hook receiver accepts deliveries on.
//
// It is empty until Start has run, because both the port and the path are
// decided there.
func (a *Adapter) HookURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.url
}

// HookSettings renders the settings document that points Claude's hooks at this
// receiver.
//
// It is returned as bytes for the caller to write. The adapter does not write
// it and does not choose where it goes: a settings file is Claude's
// configuration, and a component that decided where to put one would be
// editing a user's environment as a side effect of starting. The caller passes
// the file to the CLI with `--settings`, which is how Phase 7.3B-0 ran every
// experiment without touching any project or user configuration.
//
// # Why the two handler types are not uniform
//
// `SessionStart` is declared as a command and the other four as HTTP, because
// Claude Code accepts no HTTP handler for `SessionStart`. Declaring it as one
// anyway is a configuration that fails *silently* - the hook simply never
// arrives - so the asymmetry is written here, once, rather than left to
// whoever writes a settings file by hand.
func (a *Adapter) HookSettings() ([]byte, error) {
	a.mu.Lock()
	url := a.url
	a.mu.Unlock()
	if url == "" {
		return nil, newError(CodeNotStarted,
			"the adapter has not started, so it has no address for Claude to deliver to")
	}

	command := fmt.Sprintf(
		"curl -sS -X POST -H 'Content-Type: application/json' --data-binary @- %s",
		shellQuote(url))

	settings := map[string]any{
		// Without this list Claude refuses an HTTP hook, and refuses it
		// quietly. The entry is the receiver's own URL and nothing wider.
		"allowedHttpHookUrls": []string{url},
		"hooks": map[string]any{
			string(KindSessionStart): []any{
				hookEntry(map[string]any{"type": "command", "command": command}),
			},
			string(KindUserPromptSubmit):  []any{hookEntry(map[string]any{"type": "http", "url": url})},
			string(KindPermissionRequest): []any{hookEntry(map[string]any{"type": "http", "url": url})},
			string(KindStop):              []any{hookEntry(map[string]any{"type": "http", "url": url})},
			string(KindSessionEnd):        []any{hookEntry(map[string]any{"type": "http", "url": url})},
		},
	}
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, wrapError(err, CodeInvalidConfig, "could not render the hook settings document")
	}
	return encoded, nil
}

// hookEntry wraps one handler in the matcher object the settings schema
// expects.
//
// The matcher is empty, which is what makes the handler apply to the event
// rather than to a subset of it. None of the five events this phase reads takes
// a matcher.
func hookEntry(handler map[string]any) map[string]any {
	return map[string]any{"hooks": []any{handler}}
}

// shellQuote renders a URL as a single shell word.
//
// It is the same idiom Quote uses for a path, and it is here for the same
// reason: the command is a line a shell will parse, and the receiver's URL is
// built from an address the caller configured.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
