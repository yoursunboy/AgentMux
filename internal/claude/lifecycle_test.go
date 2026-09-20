package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The lifecycle: what Start refuses, what Stop guarantees, and what a
// subscriber sees.

func TestStartRefusesAConfigThatCannotIdentifyTheRuntime(t *testing.T) {
	// §十一. The adapter must be told which runtime it is observing and must
	// not work one out. A config missing either identifier is refused rather
	// than defaulted, because a guessed correlation is written to a history
	// that cannot be corrected.
	cases := map[string]Config{
		"no project": {RuntimeID: "amx-p_x-1"},
		"no runtime": {ProjectID: "p_x"},
		"blank project": {
			ProjectID: "   ",
			RuntimeID: "amx-p_x-1",
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, recorder := newTestAdapter(t)
			err := adapter.Start(context.Background(), cfg)
			if !IsCode(err, CodeInvalidConfig) {
				t.Fatalf("Start(%+v) = %v; want %q", cfg, err, CodeInvalidConfig)
			}
			if adapter.Started() {
				t.Error("the adapter reports itself started after a refused Start")
			}
			if got := recorder.all(); len(got) != 0 {
				t.Errorf("a refused Start recorded %v", got)
			}
		})
	}
}

func TestStartIsRefusedTwice(t *testing.T) {
	// A second Start would bind a second address and mint a second path while
	// the first receiver kept running, so the hooks declared against the first
	// would arrive at an adapter the caller no longer holds. Refusing is the
	// honest answer.
	adapter, _ := newTestAdapter(t)
	if err := adapter.Start(context.Background(), testConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := adapter.Start(context.Background(), testConfig())
	if !IsCode(err, CodeAlreadyStarted) {
		t.Fatalf("second Start = %v; want %q", err, CodeAlreadyStarted)
	}
}

func TestStartReportsAnAddressThatCannotBeBound(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	cfg := testConfig()
	// Port 1 on loopback is privileged and nothing in the suite runs as root.
	cfg.HookAddr = "127.0.0.1:1"
	if err := adapter.Start(context.Background(), cfg); err == nil {
		// A machine that allows it would make this test meaningless rather
		// than failing, so the adapter is stopped and the test is skipped.
		_ = adapter.Stop(context.Background())
		t.Skip("this machine allows binding a privileged port; nothing to assert")
	} else if !IsCode(err, CodeInvalidConfig) {
		t.Fatalf("Start on an unboundable address = %v; want %q", err, CodeInvalidConfig)
	}
	if adapter.Started() {
		t.Error("the adapter reports itself started after a failed bind")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	if err := adapter.Start(context.Background(), testConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := adapter.Stop(context.Background()); err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
	}
	// Stopping one that never started is nothing to do, not a failure.
	fresh, _ := newTestAdapter(t)
	if err := fresh.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

func TestStopTakesTheAddressWithIt(t *testing.T) {
	// A caller that renders a settings document after Stop would otherwise get
	// one naming a port nothing is listening on, and every hook it declared
	// would be delivered into a closed socket - a session configured to report
	// nothing, with nothing saying so. The address is part of what Stop ends.
	adapter, _ := newTestAdapter(t)
	if err := adapter.Start(context.Background(), testConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if adapter.HookURL() == "" {
		t.Fatal("Start did not publish an address")
	}
	if err := adapter.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := adapter.HookURL(); got != "" {
		t.Errorf("HookURL after Stop = %q; want empty", got)
	}
	if _, err := adapter.HookSettings(); !IsCode(err, CodeNotStarted) {
		t.Errorf("HookSettings after Stop = %v; want %q", err, CodeNotStarted)
	}
}

func TestSubscribeNeedsARunningAdapter(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	if _, err := adapter.Subscribe(context.Background()); !IsCode(err, CodeNotStarted) {
		t.Fatalf("Subscribe before Start = %v; want %q", err, CodeNotStarted)
	}
}

func TestStopClosesEverySubscriber(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	if err := adapter.Start(context.Background(), testConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first, err := adapter.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	second, err := adapter.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := adapter.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i, ch := range []<-chan Event{first, second} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("subscriber %d received an event after Stop", i)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("subscriber %d was not closed by Stop", i)
		}
	}
}

func TestASubscriberThatGoesAwayIsReleased(t *testing.T) {
	// Without this, a caller that subscribed and then lost interest would be
	// written to for the life of the process, and the drop counter would climb
	// with nothing to attribute it to.
	adapter, _ := newTestAdapter(t)
	if err := adapter.Start(context.Background(), testConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := adapter.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	adapter.mu.Lock()
	before := len(adapter.subs)
	adapter.mu.Unlock()
	if before != 1 {
		t.Fatalf("subscribers = %d; want 1 before the context ends", before)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		adapter.mu.Lock()
		remaining := len(adapter.subs)
		adapter.mu.Unlock()
		if remaining == 0 {
			select {
			case _, ok := <-ch:
				if ok {
					t.Error("a released subscriber's channel was left open")
				}
			default:
				t.Error("a released subscriber's channel was not closed")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the subscription was not released when its context ended")
}

func TestObservationIsPublishedBeforeItIsRecorded(t *testing.T) {
	// A subscriber that reacts to an event must find the session already
	// correlated; otherwise the first thing it does with an event is ask who
	// it belongs to and be told nothing.
	h := newHarness(t)
	ch := h.subscribe()

	h.deliver(hookBody("SessionStart", `"session_id":"sess-publish","source":"startup"`))

	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("subscriber saw %d observations; want 1", len(events))
	}
	observed := events[0]
	if observed.Kind != KindSessionStart {
		t.Errorf("kind = %q; want %q", observed.Kind, KindSessionStart)
	}
	if observed.SessionID != "sess-publish" {
		t.Errorf("sessionId = %q; want the id Claude reported", observed.SessionID)
	}
	if observed.ProjectID != h.config.ProjectID || observed.RuntimeID != h.config.RuntimeID {
		t.Errorf("observation carried %s/%s; want the configured project and runtime",
			observed.ProjectID, observed.RuntimeID)
	}
	if _, ok := h.adapter.SessionFor("sess-publish"); !ok {
		t.Error("the session was published before it was correlated")
	}
}

func TestAnObservationWithoutASessionIDIsStillRecorded(t *testing.T) {
	// A `Stop` hook that omitted the session id is still a fact about the
	// runtime. Refusing to record it would lose the fact because a correlation
	// was missing, which is the wrong way round.
	h := newHarness(t)
	h.deliver(`{"hook_event_name":"Stop"}`)

	types := h.recorder.types()
	if len(types) != 1 || types[0] != TypeAgentCompletedCandidate {
		t.Fatalf("types = %v; want [%s]", types, TypeAgentCompletedCandidate)
	}
	if recorded := h.recorder.all()[0]; recorded.RuntimeID != h.config.RuntimeID {
		t.Errorf("runtimeId = %q; want the configured runtime", recorded.RuntimeID)
	}
}

// ---------------------------------------------------------------------------
// Session correlation
// ---------------------------------------------------------------------------

func TestTheClaudeSessionIDIsPreservedAcrossEveryPath(t *testing.T) {
	// §十八, session mapping. The id Claude reports is what ties a hook
	// delivery, the stream's init message and the final result to one session,
	// and it must survive all three unchanged.
	const sessionID = "7b3b0a10-0000-4000-8000-0000000000c1"

	h := newHarness(t)
	ch := h.subscribe()

	h.deliver(hookBody("SessionStart", `"session_id":"`+sessionID+`","source":"startup"`))
	h.deliver(hookBody("UserPromptSubmit", `"session_id":"`+sessionID+`"`))

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"` + sessionID + `","cwd":"/tmp/x"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"` + sessionID + `"}`,
	}, "\n")
	if err := h.adapter.ConsumeStream(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("ConsumeStream: %v", err)
	}

	for _, ev := range drain(ch) {
		if ev.SessionID != sessionID {
			t.Errorf("%s carried session %q; want %q", ev.Kind, ev.SessionID, sessionID)
		}
	}

	binding, ok := h.adapter.SessionFor(sessionID)
	if !ok {
		t.Fatalf("session %s was not correlated", sessionID)
	}
	if binding.ProjectID != h.config.ProjectID ||
		binding.RuntimeID != h.config.RuntimeID ||
		binding.AgentSessionID != h.config.AgentSessionID {
		t.Errorf("binding = %+v; want the configured context", binding)
	}
	if binding.FirstSeen.IsZero() || binding.LastSeen.IsZero() {
		t.Error("the binding has no seen times")
	}
	if binding.Events < 4 {
		t.Errorf("binding counted %d observations; want at least 4", binding.Events)
	}
}

func TestTheSessionIDNeverReachesAPayload(t *testing.T) {
	// §十 asks for the Claude session id to be kept, and the event service
	// refuses any payload field whose normalised name ends in "sessionid". Both
	// are satisfied by keeping the id in the adapter's mapping and never in a
	// row, and this is the test that says so: the id is in the binding, and it
	// is not in anything that was recorded.
	const sessionID = "7b3b0a10-0000-4000-8000-0000000000c2"

	h := newHarness(t)
	ch := h.subscribe()

	h.deliver(hookBody("SessionStart", `"session_id":"`+sessionID+`","source":"startup"`))
	h.deliver(hookBody("PermissionRequest", `"session_id":"`+sessionID+`","tool_name":"Bash"`))

	if _, ok := h.adapter.SessionFor(sessionID); !ok {
		t.Fatal("the session was not correlated")
	}
	seen := false
	for _, ev := range drain(ch) {
		if ev.SessionID == sessionID {
			seen = true
		}
	}
	if !seen {
		t.Error("no observation carried the session id")
	}

	for _, recorded := range h.recorder.all() {
		if strings.Contains(string(recorded.Raw), sessionID) {
			t.Errorf("%s stored the claude session id: %s", recorded.Type, recorded.Raw)
		}
		for key := range recorded.Payload {
			if strings.Contains(strings.ToLower(key), "session") {
				t.Errorf("%s has a payload field named %q", recorded.Type, key)
			}
		}
	}
}

func TestSessionsAreListedInAStableOrder(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"sess-c", "sess-a", "sess-b"} {
		h.deliver(hookBody("SessionStart", `"session_id":"`+id+`","source":"startup"`))
	}
	sessions := h.adapter.Sessions()
	if len(sessions) != 3 {
		t.Fatalf("sessions = %d; want 3", len(sessions))
	}
	want := []string{"sess-a", "sess-b", "sess-c"}
	for i, session := range sessions {
		if session.SessionID != want[i] {
			t.Errorf("sessions[%d] = %q; want %q", i, session.SessionID, want[i])
		}
	}
}

func TestAnUnknownSessionIsReportedAsAbsent(t *testing.T) {
	h := newHarness(t)
	if _, ok := h.adapter.SessionFor("sess-nobody"); ok {
		t.Error("an unobserved session id was reported as correlated")
	}
}

// ---------------------------------------------------------------------------
// The endpoint
// ---------------------------------------------------------------------------

func TestTheHookURLIsNotGuessable(t *testing.T) {
	// The path is the receiver's whole authentication. It is asserted rather
	// than described so that a change to a fixed path would fail here.
	h := newHarness(t)
	url := h.adapter.HookURL()

	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("HookURL = %q; want it to start with %q", url, prefix)
	}
	rest := strings.TrimPrefix(url, prefix)
	_, path, found := strings.Cut(rest, "/")
	if !found {
		t.Fatalf("HookURL = %q; want a path", url)
	}
	nonce := strings.TrimPrefix(path, "hooks/")
	if len(nonce) < 2*hookPathSpec.Bytes {
		t.Errorf("the hook path carries %d characters of nonce; want at least %d",
			len(nonce), 2*hookPathSpec.Bytes)
	}
}

func TestHookURLAndSettingsNeedARunningAdapter(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	if got := adapter.HookURL(); got != "" {
		t.Errorf("HookURL before Start = %q; want empty", got)
	}
	if _, err := adapter.HookSettings(); !IsCode(err, CodeNotStarted) {
		t.Errorf("HookSettings before Start = %v; want %q", err, CodeNotStarted)
	}
}

func TestTheReceiverAnswersOnlyItsOwnPath(t *testing.T) {
	h := newHarness(t)
	url := h.adapter.HookURL()

	wrong := strings.Replace(url, "/hooks/", "/hooks/x", 1)
	if status := h.deliverTo(wrong, hookBody("Stop", "")); status != 404 {
		t.Errorf("a request to a wrong path answered %d; want 404", status)
	}
	if got := h.recorder.types(); len(got) != 0 {
		t.Errorf("a misdirected request recorded %v", got)
	}
}

func TestTheReceiverRefusesAnythingThatIsNotAHook(t *testing.T) {
	h := newHarness(t)

	resp, err := h.client.Get(h.adapter.HookURL())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("GET answered %d; want 405", resp.StatusCode)
	}

	if status := h.deliver("this is not json"); status != 400 {
		t.Errorf("a body that is not JSON answered %d; want 400", status)
	}
	if got := h.recorder.types(); len(got) != 0 {
		t.Errorf("a refused delivery recorded %v", got)
	}
}

func TestAReceiverWhoseAdapterHasStoppedRecordsNothing(t *testing.T) {
	// The receiver outlives the adapter by however long the server takes to
	// stop, and in that window there is no configuration left to attribute a
	// delivery to. The receiver is driven directly here rather than over a
	// socket, because a stopped adapter's port is already closing and the
	// window this pins is the one before that - the state, not the socket.
	adapter, recorder := newTestAdapter(t)
	receiver := &hookReceiver{adapter: adapter, path: "/hooks/orphaned"}

	req := httptest.NewRequest(http.MethodPost, "/hooks/orphaned",
		strings.NewReader(hookBody("Stop", "")))
	rec := httptest.NewRecorder()
	receiver.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a delivery to a stopped adapter answered %d; want 503", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a delivery to a stopped adapter answered with a body: %q", rec.Body.String())
	}
	if got := recorder.types(); len(got) != 0 {
		t.Errorf("a delivery to a stopped adapter recorded %v", got)
	}
}

func TestAnOversizedDeliveryIsRefusedAndNotStored(t *testing.T) {
	h := newHarness(t)
	huge := `{"hook_event_name":"UserPromptSubmit","prompt":"` + strings.Repeat("x", hookMaxBody+1024) + `"}`
	if status := h.deliver(huge); status != 413 {
		t.Errorf("an oversized delivery answered %d; want 413", status)
	}
	if got := h.recorder.types(); len(got) != 0 {
		t.Errorf("an oversized delivery recorded %v", got)
	}
}

func TestAHookThisPhaseDoesNotReadIsAnsweredAndDropped(t *testing.T) {
	// Claude declares thirty-three hook events. One of the other twenty-eight
	// arriving is ordinary, not an error, and it is not recorded as anything.
	h := newHarness(t)
	if status := h.deliver(hookBody("PreToolUse", `"tool_name":"Bash"`)); status != 200 {
		t.Errorf("an unread hook answered %d; want 200", status)
	}
	if got := h.recorder.types(); len(got) != 0 {
		t.Errorf("an unread hook recorded %v", got)
	}
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func TestHookSettingsUseACommandForSessionStartAndHTTPForTheRest(t *testing.T) {
	// §十三, and Phase 7.3B-0 §11.5: Claude Code accepts no HTTP handler for
	// SessionStart, and the configuration that declares one anyway fails
	// silently. The asymmetry is the point of this test.
	h := newHarness(t)
	raw, err := h.adapter.HookSettings()
	if err != nil {
		t.Fatalf("HookSettings: %v", err)
	}

	var doc struct {
		AllowedHTTPHookURLs []string `json:"allowedHttpHookUrls"`
		Hooks               map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := jsonUnmarshal(raw, &doc); err != nil {
		t.Fatalf("the settings document is not the shape Claude reads: %v\n%s", err, raw)
	}

	url := h.adapter.HookURL()
	if len(doc.AllowedHTTPHookURLs) != 1 || doc.AllowedHTTPHookURLs[0] != url {
		t.Errorf("allowedHttpHookUrls = %v; want exactly [%s] - without it an HTTP hook is refused silently",
			doc.AllowedHTTPHookURLs, url)
	}

	want := []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop", "SessionEnd"}
	if len(doc.Hooks) != len(want) {
		t.Errorf("the settings declare %d events; want %d: %v", len(doc.Hooks), len(want), doc.Hooks)
	}
	for _, name := range want {
		entries, ok := doc.Hooks[name]
		if !ok {
			t.Errorf("%s is not declared", name)
			continue
		}
		if len(entries) != 1 || len(entries[0].Hooks) != 1 {
			t.Errorf("%s has %d matcher groups; want one handler", name, len(entries))
			continue
		}
		handler := entries[0].Hooks[0]
		handlerType, _ := handler["type"].(string)
		switch name {
		case "SessionStart":
			if handlerType != "command" {
				t.Errorf("SessionStart is declared as %q; Claude refuses anything but a command", handlerType)
			}
			command, _ := handler["command"].(string)
			if !strings.Contains(command, url) {
				t.Errorf("the SessionStart command does not reach the receiver: %q", command)
			}
			if !strings.Contains(command, "curl") {
				t.Errorf("the SessionStart command is not something that can post: %q", command)
			}
		default:
			if handlerType != "http" {
				t.Errorf("%s is declared as %q; want http", name, handlerType)
			}
			if got, _ := handler["url"].(string); got != url {
				t.Errorf("%s posts to %q; want %s", name, got, url)
			}
		}
	}
}

func TestHookSettingsNameNothingButTheReceiver(t *testing.T) {
	// The document is written into a Claude configuration. Nothing in it may
	// name a credential, and every URL in it must be the receiver this adapter
	// opened - a settings file that pointed a hook somewhere else would be
	// sending a session's events to a stranger.
	h := newHarness(t)
	raw, err := h.adapter.HookSettings()
	if err != nil {
		t.Fatalf("HookSettings: %v", err)
	}
	text := string(raw)
	for _, forbidden := range []string{"password", "token", "secret", "api_key", "apikey", "credential"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("the settings document names %q:\n%s", forbidden, text)
		}
	}
	if !strings.Contains(text, h.adapter.HookURL()) {
		t.Error("the settings do not point at the receiver")
	}
	for _, url := range urlsIn(text) {
		if url != h.adapter.HookURL() {
			t.Errorf("the settings name %q, which is not this receiver", url)
		}
	}
}

// urlsIn returns every http URL appearing in a document.
func urlsIn(text string) []string {
	var out []string
	for {
		_, rest, found := strings.Cut(text, "http://")
		if !found {
			return out
		}
		end := strings.IndexAny(rest, `"' `+"\n")
		if end < 0 {
			end = len(rest)
		}
		out = append(out, "http://"+rest[:end])
		text = rest[end:]
	}
}

// jsonUnmarshal is a local alias so the settings test reads as one assertion
// rather than an import.
func jsonUnmarshal(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}

// hookBody builds a hook payload body from a partial JSON object.
func hookBody(event, extra string) string {
	body := `{"hook_event_name":"` + event + `"`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}
