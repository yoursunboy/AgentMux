package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/event"
)

// The mapping: which of Claude's facts becomes which of AgentMux's events, what
// each one's payload holds, and what never reaches a payload at all.

func TestEveryHookThisPhaseReadsMapsToAnAgentMuxType(t *testing.T) {
	cases := []struct {
		hook string
		want string
	}{
		{"SessionStart", TypeAgentStarted},
		{"UserPromptSubmit", TypeAgentPromptSubmitted},
		{"PermissionRequest", TypeAgentPermissionRequested},
		{"Stop", TypeAgentCompletedCandidate},
		{"SessionEnd", TypeAgentSessionEnded},
	}
	for _, tc := range cases {
		t.Run(tc.hook, func(t *testing.T) {
			ev, ok := eventFromHook(hookPayload{HookEventName: tc.hook}, "p_x", "amx-p_x-1", testClock)
			if !ok {
				t.Fatalf("%s was not read", tc.hook)
			}
			got, record := agentEventType(ev.Kind, ev.Payload)
			if !record {
				t.Fatalf("%s mapped to no event type", tc.hook)
			}
			if got != tc.want {
				t.Errorf("%s maps to %q; want %q", tc.hook, got, tc.want)
			}
		})
	}
}

func TestStopIsACompletionCandidateAndNotACompletion(t *testing.T) {
	// §八, stated as a test. `Stop` means the model stopped producing. A turn
	// that was aborted, a tool call that was refused and a crash after the last
	// token all look exactly like this from here, so recording it as a
	// completed turn would put a claim into an immutable row that the evidence
	// does not support.
	ev, ok := eventFromHook(hookPayload{HookEventName: "Stop"}, "p_x", "amx-p_x-1", testClock)
	if !ok {
		t.Fatal("Stop was not read")
	}
	got, _ := agentEventType(ev.Kind, ev.Payload)
	if got != TypeAgentCompletedCandidate {
		t.Fatalf("Stop maps to %q; want %q", got, TypeAgentCompletedCandidate)
	}
	if got == TypeAgentCompleted {
		t.Fatal("Stop was recorded as a completed turn")
	}
	// The candidate type is a type of its own and not an alias, so a reader
	// filtering for completed turns does not accidentally match it.
	if TypeAgentCompletedCandidate == TypeAgentCompleted {
		t.Fatal("the candidate type is the completion type")
	}
}

func TestTheResultEnvelopeDecidesTheOutcome(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		want    string
		isError bool
	}{
		{
			name: "a turn that succeeded",
			line: `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed"}`,
			want: TypeAgentCompleted,
		},
		{
			name: "a turn that failed",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"api_error"}`,
			want: TypeAgentFailed,
			isError: true,
		},
		{
			// The measurement from Phase 7.3B-0 §11.2, replayed here. `subtype`
			// reads "success" on a turn that did not succeed, which is exactly
			// why §九 forbids deciding anything from it.
			name:    "the subtype lies and is_error is believed",
			line:    `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error"}`,
			want:    TypeAgentFailed,
			isError: true,
		},
		{
			name: "an envelope that says nothing about error",
			line: `{"type":"result","result":"done"}`,
			want: TypeAgentCompleted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var message streamMessage
			if err := json.Unmarshal([]byte(tc.line), &message); err != nil {
				t.Fatalf("decode: %v", err)
			}
			ev, ok := streamEvent(message, "p_x", "amx-p_x-1", testClock)
			if !ok {
				t.Fatal("the result envelope was not read")
			}
			got, record := agentEventType(ev.Kind, ev.Payload)
			if !record {
				t.Fatal("the result envelope mapped to no event type")
			}
			if got != tc.want {
				t.Errorf("maps to %q; want %q", got, tc.want)
			}

			var payload map[string]any
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if payload["isError"] != tc.isError {
				t.Errorf("payload isError = %v; want %v", payload["isError"], tc.isError)
			}
		})
	}
}

func TestAResultThatCannotBeReadIsAFailure(t *testing.T) {
	// Unreachable in production - the payload is built by this package - so the
	// direction is chosen for what it would mean if it were reached. Recording
	// an unreadable result as a success is the one mistake the row could not be
	// corrected for.
	if !resultFailed(json.RawMessage(`not an object`)) {
		t.Error("an unreadable result was treated as a success")
	}
	if resultFailed(json.RawMessage(`{"isError":false}`)) {
		t.Error("a readable result saying no error was treated as a failure")
	}
}

func TestObservationsThatAreNotEventsAreNotRecorded(t *testing.T) {
	// The stream's init and hook lifecycle messages are how a session is
	// followed, not facts about the work in it. The hook events they describe
	// already arrived through the receiver; recording them a second time would
	// put a duplicate of every hook event into a log that cannot be corrected.
	for _, kind := range []Kind{KindInitialized, KindHookStarted, KindHookResponse} {
		payload := payloadOf(map[string]any{"event": string(kind)})
		if _, record := agentEventType(kind, payload); record {
			t.Errorf("%s is recorded; want it published only", kind)
		}
	}
}

func TestEveryRecordedPayloadIsAcceptedByTheEventService(t *testing.T) {
	// The mapping decides what a payload holds; this is the test that the
	// decision is one the event service will take. It walks every kind the
	// adapter can produce, with the fullest payload each one can carry, and
	// puts it through the same check Service.CreateEvent applies.
	events := []Event{
		hookEvent(t, hookPayload{HookEventName: "SessionStart", SessionID: "s", Source: "startup"}),
		hookEvent(t, hookPayload{HookEventName: "UserPromptSubmit", SessionID: "s"}),
		hookEvent(t, hookPayload{HookEventName: "PermissionRequest", SessionID: "s", ToolName: "Bash"}),
		hookEvent(t, hookPayload{HookEventName: "Stop", SessionID: "s"}),
		hookEvent(t, hookPayload{HookEventName: "SessionEnd", SessionID: "s", Reason: "prompt_input_exit"}),
		streamEventOf(t, `{"type":"system","subtype":"init","session_id":"s"}`),
		streamEventOf(t, `{"type":"system","subtype":"hook_started","hook_name":"Stop:1"}`),
		streamEventOf(t, `{"type":"system","subtype":"hook_response","hook_name":"Stop:1"}`),
		streamEventOf(t, `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed"}`),
		streamEventOf(t, `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error"}`),
	}

	recorded := 0
	for _, ev := range events {
		eventType, record := agentEventType(ev.Kind, ev.Payload)
		if !record {
			continue
		}
		recorded++
		if !event.ValidType(eventType) {
			t.Errorf("%s is not an event type", eventType)
		}
		if err := event.CheckPayload(ev.Payload); err != nil {
			t.Errorf("%s carries a payload the event service refuses: %v\n%s", eventType, err, ev.Payload)
		}
	}
	if recorded != 7 {
		t.Errorf("%d of the %d kinds are recorded; want the 7 the brief names", recorded, len(events))
	}
}

func TestAPayloadCarriesIdentifiersAndEnumerationsOnly(t *testing.T) {
	// §十四. The allowed shape is stated as a test rather than as a rule in
	// prose, so that adding a field to a payload is a change that has to come
	// past it.
	allowed := map[string]bool{
		"event":          true,
		"source":         true,
		"reason":         true,
		"tool":           true,
		"hook":           true,
		"isError":        true,
		"terminalReason": true,
	}

	cases := []json.RawMessage{
		hookEvent(t, hookPayload{HookEventName: "SessionStart", Source: "startup"}).Payload,
		hookEvent(t, hookPayload{HookEventName: "UserPromptSubmit"}).Payload,
		hookEvent(t, hookPayload{HookEventName: "PermissionRequest", ToolName: "Bash"}).Payload,
		hookEvent(t, hookPayload{HookEventName: "Stop"}).Payload,
		hookEvent(t, hookPayload{HookEventName: "SessionEnd", Reason: "clear"}).Payload,
		streamEventOf(t, `{"type":"system","subtype":"init"}`).Payload,
		streamEventOf(t, `{"type":"system","subtype":"hook_started","hook_name":"Stop:1"}`).Payload,
		streamEventOf(t, `{"type":"system","subtype":"hook_response","hook_name":"Stop:1"}`).Payload,
		streamEventOf(t, `{"type":"result","is_error":true,"terminal_reason":"api_error"}`).Payload,
	}

	seen := map[string]bool{}
	for _, payload := range cases {
		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("payload %s: %v", payload, err)
		}
		if len(decoded) == 0 {
			t.Errorf("an empty payload: %s", payload)
		}
		for key, value := range decoded {
			seen[key] = true
			if !allowed[key] {
				t.Errorf("payload field %q is not one this phase keeps: %s", key, payload)
			}
			switch value.(type) {
			case string, bool:
			default:
				t.Errorf("payload field %q holds a %T; an identifier or an enumeration is all that belongs here",
					key, value)
			}
		}
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("the allow list names %q, which no payload carries; the list has drifted", key)
		}
	}
}

func TestACredentialInAHookPayloadNeverReachesTheRecord(t *testing.T) {
	// §十八, secret filtering, and §十四's forbidden list. The payload below is
	// what a real `UserPromptSubmit` hook carries - a prompt, a transcript
	// path, the working directory - with a credential in each of the places one
	// could plausibly be. None of it may reach the event log.
	h := newHarness(t)

	body := `{
		"session_id": "sess-secrets",
		"transcript_path": "/home/someone/.claude/projects/-secret/transcript.jsonl",
		"cwd": "/home/someone/private-project",
		"hook_event_name": "UserPromptSubmit",
		"prompt": "deploy with password hunter2 and token ghp_ABCDEFGHIJKLMNOP",
		"prompt_id": "prompt-1",
		"permission_mode": "default",
		"api_key": "sk-ant-not-a-real-key",
		"client_secret": "not-a-real-secret",
		"tool_input": {"command": "export AWS_SECRET_ACCESS_KEY=not-a-real-key"}
	}`
	if status := h.deliver(body); status != 200 {
		t.Fatalf("the delivery answered %d", status)
	}

	recorded := h.recorder.all()
	if len(recorded) != 1 {
		t.Fatalf("recorded %v; want exactly one prompt event", h.recorder.types())
	}
	ev := recorded[0]
	if ev.Type != TypeAgentPromptSubmitted {
		t.Fatalf("type = %q; want %q", ev.Type, TypeAgentPromptSubmitted)
	}

	text := string(ev.Raw)
	for _, secret := range []string{
		"hunter2", "ghp_ABCDEFGHIJKLMNOP", "sk-ant-not-a-real-key", "not-a-real-secret",
		"not-a-real-key", "AWS_SECRET_ACCESS_KEY",
		"deploy with", "transcript.jsonl", "private-project", "/home/someone",
		"prompt-1", "default",
	} {
		if strings.Contains(text, secret) {
			t.Errorf("the recorded payload carries %q: %s", secret, text)
		}
	}

	// What it does carry: that a prompt was submitted, and for which session's
	// observation. Nothing else.
	if len(ev.Payload) != 1 || ev.Payload["event"] != "UserPromptSubmit" {
		t.Errorf("payload = %v; want only the event name", ev.Payload)
	}
}

func TestAHookPayloadIsReducedToItsSafeFields(t *testing.T) {
	// The reduction happens at the decode edge. Everything a Claude hook
	// carries that AgentMux does not keep has nowhere to go, which is a
	// stronger guarantee than filtering it afterwards.
	ev, ok := eventFromHook(hookPayload{
		SessionID:     "sess-1",
		HookEventName: "PermissionRequest",
		Source:        "startup",
		Reason:        "clear",
		ToolName:      "Bash",
	}, "p_x", "amx-p_x-1", testClock)
	if !ok {
		t.Fatal("the hook was not read")
	}
	if ev.SessionID != "sess-1" {
		t.Errorf("sessionId = %q; want sess-1", ev.SessionID)
	}

	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	want := map[string]any{"event": "PermissionRequest", "tool": "Bash"}
	if len(payload) != len(want) {
		t.Fatalf("payload = %v; want %v", payload, want)
	}
	for key, value := range want {
		if payload[key] != value {
			t.Errorf("payload[%q] = %v; want %v", key, payload[key], value)
		}
	}
}

func TestAFieldWithNothingInItIsNotStored(t *testing.T) {
	// "This event did not say" and "this event said nothing" are different
	// facts, and only the first is true when Claude omitted the field.
	ev, _ := eventFromHook(hookPayload{HookEventName: "SessionEnd"}, "p_x", "amx-p_x-1", testClock)
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if _, present := payload["reason"]; present {
		t.Errorf("payload = %v; want no reason field when Claude gave none", payload)
	}
}

func TestAnOversizedFieldIsBoundedRatherThanRefused(t *testing.T) {
	// A value that grew past the bound would make the whole event too large for
	// the event service, and the event would be dropped. Bounding it keeps the
	// event and shows that it was cut.
	long := strings.Repeat("A", maxFieldLen*2)
	ev, _ := eventFromHook(hookPayload{HookEventName: "SessionEnd", Reason: long}, "p_x", "amx-p_x-1", testClock)

	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	reason, _ := payload["reason"].(string)
	if len(reason) > maxFieldLen+3 {
		t.Errorf("reason is %d bytes; want it bounded at %d", len(reason), maxFieldLen)
	}
	if !strings.HasSuffix(reason, "...") {
		t.Errorf("reason = %q; want it to show that it was cut", reason)
	}
	if err := event.CheckPayload(ev.Payload); err != nil {
		t.Errorf("a bounded payload was refused: %v", err)
	}
}

func TestEventStringOmitsThePayload(t *testing.T) {
	// A log line has no key check in front of it, so the one part of an event a
	// decoder shaped must not reach it.
	ev := Event{
		Kind:      KindPermissionRequest,
		SessionID: "sess-log",
		ProjectID: "p_x",
		RuntimeID: "amx-p_x-1",
		Payload:   json.RawMessage(`{"event":"PermissionRequest","tool":"do-not-log-me"}`),
	}
	rendered := ev.String()
	if strings.Contains(rendered, "do-not-log-me") {
		t.Fatalf("String() leaked the payload: %s", rendered)
	}
	for _, want := range []string{"sess-log", "p_x", "amx-p_x-1", "PermissionRequest"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("String() = %q; want it to contain %q", rendered, want)
		}
	}
}

// hookEvent builds the observation a hook payload produces.
func hookEvent(t *testing.T, payload hookPayload) Event {
	t.Helper()
	ev, ok := eventFromHook(payload, "p_x", "amx-p_x-1", testClock)
	if !ok {
		t.Fatalf("%s was not read", payload.HookEventName)
	}
	return ev
}

// streamEventOf builds the observation a stream line produces.
func streamEventOf(t *testing.T, line string) Event {
	t.Helper()
	var message streamMessage
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	ev, ok := streamEvent(message, "p_x", "amx-p_x-1", testClock)
	if !ok {
		t.Fatalf("%s was not read", line)
	}
	return ev
}
