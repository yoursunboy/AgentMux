package claude

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
)

// These tests are about the adapter, not about a Claude Code installation.
// Nothing here runs the CLI, reads a credential, or needs an account: every
// dependency that touches the machine is either injected or is a loopback
// socket the test opens itself. The suite produces the same answers on a laptop
// with Claude Code installed, on a build server without it, and in CI.

// ---------------------------------------------------------------------------
// Recording, as the event service would do it
// ---------------------------------------------------------------------------

// recordedEvent is one event the adapter asked to have recorded.
type recordedEvent struct {
	ProjectID string
	RuntimeID string
	Type      string
	Source    string
	Payload   map[string]any
	Raw       json.RawMessage
}

// memoryRecorder stands in for the event service.
//
// It is deliberately not a permissive stub. It applies the same three checks
// the real Service.CreateEvent applies - the type is a dotted name, the source
// is declared, and the payload passes CheckPayload - and fails the test when
// one of them refuses. That is what makes every test in this package a test of
// the payloads as the event service will actually see them: a payload that
// names a credential, or that grew past the bound, fails here rather than at
// runtime in a warning nobody reads.
type memoryRecorder struct {
	t *testing.T

	mu     sync.Mutex
	events []recordedEvent
}

func newRecorder(t *testing.T) *memoryRecorder {
	t.Helper()
	return &memoryRecorder{t: t}
}

func (r *memoryRecorder) CreateEvent(
	_ context.Context,
	projectID, runtimeID, eventType, source string,
	payload json.RawMessage,
) (*event.AgentEvent, error) {
	r.t.Helper()

	if strings.TrimSpace(projectID) == "" {
		r.t.Errorf("recorded an event with no project: %s", eventType)
	}
	if !event.ValidType(eventType) {
		r.t.Errorf("recorded %q, which is not an event type", eventType)
	}
	if !event.ValidSource(source) {
		r.t.Errorf("recorded %q with source %q, which is not declared", eventType, source)
	}
	if err := event.CheckPayload(payload); err != nil {
		r.t.Errorf("recorded %s with a payload the event service refuses: %v", eventType, err)
	}

	var decoded map[string]any
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &decoded); err != nil {
			r.t.Errorf("recorded %s with a payload that is not an object: %v", eventType, err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Type:      eventType,
		Source:    source,
		Payload:   decoded,
		Raw:       append(json.RawMessage(nil), payload...),
	})
	return &event.AgentEvent{ID: "evt_test", ProjectID: projectID, RuntimeID: runtimeID, Type: eventType, Source: source}, nil
}

// all returns the events recorded so far, in order.
func (r *memoryRecorder) all() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedEvent, len(r.events))
	copy(out, r.events)
	return out
}

// types returns just the recorded types, which is what most tests assert on.
func (r *memoryRecorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Type)
	}
	return out
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

// testClock is a clock that stands still, so that an event's CreatedAt is a
// value a test can assert on rather than one it has to bracket.
var testClock = time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testClock }

// testConfig is a configuration that is complete and obviously not derived from
// anything on the machine.
func testConfig() Config {
	return Config{
		ProjectID:      "p_adaptertest",
		RuntimeID:      "amx-p_adaptertest-1",
		AgentSessionID: "sess_adaptertest",
		HookAddr:       DefaultHookAddr,
	}
}

// harness is a started adapter with its recorder and a client for its endpoint.
type harness struct {
	t        *testing.T
	adapter  *Adapter
	recorder *memoryRecorder
	config   Config
	client   *http.Client
}

// newTestAdapter builds an adapter that records into a fresh recorder.
func newTestAdapter(t *testing.T) (*Adapter, *memoryRecorder) {
	t.Helper()
	recorder := newRecorder(t)
	adapter := NewAdapter(AdapterOptions{
		Recorder: recorder,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      fixedNow,
	})
	t.Cleanup(func() {
		if err := adapter.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return adapter, recorder
}

// newHarness builds and starts an adapter on a real loopback socket.
//
// The receiver is exercised over a real HTTP server rather than by calling
// ServeHTTP directly, because the parts worth testing - the address the hooks
// are pointed at, the nonce in the path, the status codes - only exist once
// something is listening.
func newHarness(t *testing.T) *harness {
	t.Helper()
	adapter, recorder := newTestAdapter(t)
	cfg := testConfig()
	if err := adapter.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return &harness{
		t:        t,
		adapter:  adapter,
		recorder: recorder,
		config:   cfg,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

// deliver posts a hook body to the adapter's receiver and returns the status.
func (h *harness) deliver(body string) int {
	h.t.Helper()
	return h.deliverTo(h.adapter.HookURL(), body)
}

// deliverTo posts a hook body to an arbitrary URL.
func (h *harness) deliverTo(url, body string) int {
	h.t.Helper()
	resp, err := h.client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	// The body must be empty on every status: a command hook prints what it
	// receives, and what it prints becomes session context. TestHookResponses
	// AreAlwaysEmpty is the test that pins it; reading it here keeps the
	// assertion honest for every caller.
	if resp.StatusCode != http.StatusOK {
		// Error statuses are asserted on the status alone.
		return resp.StatusCode
	}
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read response: %v", err)
	}
	if len(payload) != 0 {
		h.t.Errorf("the receiver answered with a body: %q", payload)
	}
	return resp.StatusCode
}

// subscribe opens an observation channel that is closed when the test ends.
func (h *harness) subscribe() <-chan Event {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	ch, err := h.adapter.Subscribe(ctx)
	if err != nil {
		h.t.Fatalf("Subscribe: %v", err)
	}
	return ch
}

// drain collects what a subscriber has received so far, without waiting.
func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}
