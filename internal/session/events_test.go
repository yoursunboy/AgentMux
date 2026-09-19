package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
)

// This file is the runtime half of the event bridge: what the runtime manager
// records when a runtime starts, stops, fails or is destroyed, and - just as
// importantly - what it does not.
//
// The tests drive a real tmux backend, because the events are emitted from
// inside operations that do real work, and a manager stubbed down to the point
// where it emits them would be testing a different function.

// recordedEvent is one CreateEvent call, kept as it was handed over.
type recordedEvent struct {
	ProjectID string
	RuntimeID string
	Type      string
	Source    string
	Payload   json.RawMessage

	// PayloadMap is the payload decoded, for an assertion that reads a field.
	// A payload that did not decode leaves it nil, which is itself worth
	// noticing in a test.
	PayloadMap map[string]any
}

// recordingEvents is a fake EventRecorder.
//
// It records rather than stores: nothing here filters, orders or persists, so a
// failure in these tests is about what the manager emitted and never about what
// a repository did with it.
type recordingEvents struct {
	mu       sync.Mutex
	recorded []recordedEvent

	// err, when set, is what CreateEvent returns. The bridge is supposed to
	// treat a history failure as non-fatal, and this is how that is tested.
	err error
}

var _ EventRecorder = (*recordingEvents)(nil)

func (r *recordingEvents) CreateEvent(
	_ context.Context,
	projectID, runtimeID, eventType, source string,
	payload json.RawMessage,
) (*event.AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := recordedEvent{
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Type:      eventType,
		Source:    source,
		Payload:   payload,
	}
	if len(payload) > 0 {
		// Best effort: a payload that will not decode is left nil rather than
		// failing the call, so that the assertion about the shape of a payload
		// is what reports it.
		_ = json.Unmarshal(payload, &entry.PayloadMap)
	}
	r.recorded = append(r.recorded, entry)

	if r.err != nil {
		return nil, r.err
	}
	return &event.AgentEvent{
		ID:        "evt_" + "0123456789abcdef0123456789abcdef",
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Type:      eventType,
		Source:    source,
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// events returns what has been recorded so far.
func (r *recordingEvents) events() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedEvent, len(r.recorded))
	copy(out, r.recorded)
	return out
}

// types returns the recorded event types, in the order they were recorded.
func (r *recordingEvents) types() []string {
	out := []string{}
	for _, ev := range r.events() {
		out = append(out, ev.Type)
	}
	return out
}

// only fails the test unless exactly one event was recorded, and returns it.
func (r *recordingEvents) only(t *testing.T, wantType string) recordedEvent {
	t.Helper()
	recorded := r.events()
	if len(recorded) != 1 {
		t.Fatalf("recorded %v, want exactly one %s event", r.types(), wantType)
	}
	if recorded[0].Type != wantType {
		t.Fatalf("recorded %s, want %s", recorded[0].Type, wantType)
	}
	return recorded[0]
}

// testManagerRecording builds a manager over a real per-project runtime factory
// with a recorder attached.
func testManagerRecording(t *testing.T, recorder EventRecorder, projects ...*project.Project) (*Manager, *TmuxBackend) {
	t.Helper()

	dir := uniqueSocketDir(t)
	runtimes := testRuntimes(t, dir)
	m, err := NewManager(ManagerOptions{
		Backends: runtimes,
		Sockets:  runtimes.Sockets(),
		Projects: newFakeProjects(projects...),
		Store:    newFakeStore(),
		Events:   recorder,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	var backend *TmuxBackend
	if len(projects) > 0 {
		backend = newTestBackendOn(t, runtimes.Sockets().Path(projects[0].ID))
	}
	return m, backend
}

// ---------------------------------------------------------------------------
// The four facts
// ---------------------------------------------------------------------------

// TestStartRecordsRuntimeStarted is §十四's first case.
func TestStartRecordsRuntimeStarted(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_start")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	got := recorder.only(t, event.TypeRuntimeStarted)
	if got.ProjectID != p.ID {
		t.Errorf("projectId = %q, want %q", got.ProjectID, p.ID)
	}
	if got.RuntimeID != rt.Session {
		t.Errorf("runtimeId = %q, want the session name %q", got.RuntimeID, rt.Session)
	}
	if got.Source != event.SourceRuntime {
		t.Errorf("source = %q, want %q", got.Source, event.SourceRuntime)
	}

	// The payload is a fact, not a document: a state name and a size. Nothing
	// here is a path, an environment variable or anything the session printed.
	want := map[string]any{
		"state": string(StateRunning),
		"cols":  float64(rt.Cols),
		"rows":  float64(rt.Rows),
	}
	if !equalPayload(got.PayloadMap, want) {
		t.Errorf("payload = %v, want %v", got.PayloadMap, want)
	}
}

// TestStartRecordsNoEventWhenAlreadyRunning is the idempotent case. A second
// start on a running runtime returns the runtime it already has, and a second
// `runtime.started` for it would be a fact that is not true - the runtime did
// not start again.
func TestStartRecordsNoEventWhenAlreadyRunning(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_idempotent_start")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("the first Start returned an error: %v", err)
	}
	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("the second Start returned an error: %v", err)
	}

	if got := recorder.types(); len(got) != 1 || got[0] != event.TypeRuntimeStarted {
		t.Errorf("recorded %v, want one %s - a start that changed nothing is not an event",
			got, event.TypeRuntimeStarted)
	}
}

// TestStopRecordsRuntimeStopped is §十四's second case.
func TestStopRecordsRuntimeStopped(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_stop")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}

	if got := recorder.types(); len(got) != 2 ||
		got[0] != event.TypeRuntimeStarted || got[1] != event.TypeRuntimeStopped {
		t.Fatalf("recorded %v, want [%s %s]", got, event.TypeRuntimeStarted, event.TypeRuntimeStopped)
	}

	stopped := recorder.events()[1]
	if stopped.RuntimeID != rt.Session {
		t.Errorf("runtimeId = %q, want the session name %q", stopped.RuntimeID, rt.Session)
	}
	if stopped.Source != event.SourceRuntime {
		t.Errorf("source = %q, want %q", stopped.Source, event.SourceRuntime)
	}
	want := map[string]any{"state": string(StateStopped)}
	if !equalPayload(stopped.PayloadMap, want) {
		t.Errorf("payload = %v, want %v", stopped.PayloadMap, want)
	}
}

// TestStopRecordsNoEventWhenAlreadyStopped is the idempotent case on the other
// side.
func TestStopRecordsNoEventWhenAlreadyStopped(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_idempotent_stop")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("the first Stop returned an error: %v", err)
	}
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("the second Stop returned an error: %v", err)
	}

	want := []string{event.TypeRuntimeStarted, event.TypeRuntimeStopped}
	got := recorder.types()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("recorded %v, want %v - a stop that changed nothing is not an event", got, want)
	}
}

// TestDestroyRecordsRuntimeDestroyed is §十四's third case.
func TestDestroyRecordsRuntimeDestroyed(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_destroy")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	recorded := recorder.events()
	if len(recorded) != 2 {
		t.Fatalf("recorded %v, want [%s %s]",
			recorder.types(), event.TypeRuntimeStarted, event.TypeRuntimeDestroyed)
	}
	destroyed := recorded[1]
	if destroyed.Type != event.TypeRuntimeDestroyed {
		t.Fatalf("recorded %s after the start, want %s", destroyed.Type, event.TypeRuntimeDestroyed)
	}
	if destroyed.RuntimeID != rt.Session {
		t.Errorf("runtimeId = %q, want the session name %q", destroyed.RuntimeID, rt.Session)
	}
	if destroyed.Source != event.SourceRuntime {
		t.Errorf("source = %q, want %q", destroyed.Source, event.SourceRuntime)
	}
	want := map[string]any{"state": string(StateStopped)}
	if !equalPayload(destroyed.PayloadMap, want) {
		t.Errorf("payload = %v, want %v", destroyed.PayloadMap, want)
	}
}

// TestDestroyRecordsNoEventWhenNeverStarted covers destroying what was never
// there. Nothing was destroyed, so nothing is recorded.
func TestDestroyRecordsNoEventWhenNeverStarted(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_destroy_absent")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	if got := recorder.types(); len(got) != 0 {
		t.Errorf("recorded %v, want nothing - no runtime was destroyed", got)
	}
}

// TestDestroyKeepsTheHistory is the property the whole phase is for. The
// runtime record is deleted by Destroy; the events about it are not, because
// what happened does not stop having happened when the thing it happened to is
// removed.
func TestDestroyKeepsTheHistory(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_history")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	// The runtime record is gone.
	if _, err := m.store.Get(ctx, p.ID); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("the runtime record survived Destroy: %v", err)
	}

	// And the timeline is intact, with the same runtime id - which is what makes
	// a destroyed-and-restarted runtime readable as one history.
	recorded := recorder.events()
	if len(recorded) != 2 {
		t.Fatalf("recorded %v, want the two events from the runtime's life", recorder.types())
	}
	for _, ev := range recorded {
		if ev.RuntimeID != rt.Session {
			t.Errorf("an event names runtime %q, want %q", ev.RuntimeID, rt.Session)
		}
	}
}

// TestFailedStartRecordsRuntimeError is the error path. The runtime moves to
// ERROR and the event says which operation failed - the two are produced
// together by failRuntime so that they cannot drift apart.
func TestFailedStartRecordsRuntimeError(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_error")
	// A runtime path that does not exist: the session cannot be created in it.
	p.RuntimePath = t.TempDir() + "/does-not-exist"
	m, _ := testManagerRecording(t, recorder, p)

	if _, err := m.Start(ctx, p.ID); err == nil {
		t.Fatal("Start succeeded with a runtime path that does not exist")
	}

	got := recorder.only(t, event.TypeRuntimeError)
	want := map[string]any{
		"operation": operationStart,
		"state":     string(StateError),
	}
	if !equalPayload(got.PayloadMap, want) {
		t.Errorf("payload = %v, want %v", got.PayloadMap, want)
	}

	// The state and the event agree: failRuntime is the one place both are
	// produced, which is what makes that a property rather than a convention.
	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt == nil || rt.State != StateError {
		t.Errorf("the runtime state is %v, want %s alongside the error event", rt, StateError)
	}
}

// TestErrorPayloadNamesNoMessage records a deliberate omission: the event
// carries which operation failed, and not the error text. An error string can
// hold a path, a host name or a command line, and an append-only row cannot be
// redacted afterwards.
func TestErrorPayloadNamesNoMessage(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_error_payload")
	p.RuntimePath = t.TempDir() + "/does-not-exist"
	m, _ := testManagerRecording(t, recorder, p)

	if _, err := m.Start(ctx, p.ID); err == nil {
		t.Fatal("Start succeeded with a runtime path that does not exist")
	}

	got := recorder.only(t, event.TypeRuntimeError)
	for _, key := range []string{"message", "error", "err", "reason", "path", "output"} {
		if _, present := got.PayloadMap[key]; present {
			t.Errorf("the error payload has a %q field: %v", key, got.PayloadMap)
		}
	}
	if len(got.PayloadMap) != 2 {
		t.Errorf("payload = %v, want exactly an operation and a state", got.PayloadMap)
	}
}

// ---------------------------------------------------------------------------
// The bridge's contract with the runtime
// ---------------------------------------------------------------------------

// TestEventFailureDoesNotFailTheRuntime is the rule that keeps a history from
// becoming a dependency of the thing it records. A recorder that fails is a
// lost record, not a failed start.
func TestEventFailureDoesNotFailTheRuntime(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{err: errors.New("the event store is unavailable")}

	p := testProject("evt_recorder_down")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start failed because the recorder did: %v", err)
	}
	if rt.State != StateRunning {
		t.Fatalf("the runtime is %q, want %s", rt.State, StateRunning)
	}

	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("Stop failed because the recorder did: %v", err)
	}
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy failed because the recorder did: %v", err)
	}
}

// TestNilRecorderIsAWorkingConfiguration records that a manager built without a
// recorder behaves exactly as one built with a broken one: every test in this
// package that predates events builds it this way.
func TestNilRecorderIsAWorkingConfiguration(t *testing.T) {
	ctx := context.Background()

	p := testProject("evt_no_recorder")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, nil, p)

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if rt.State != StateRunning {
		t.Fatalf("the runtime is %q, want %s", rt.State, StateRunning)
	}
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}
}

// TestEveryRuntimeEventNamesItsRuntime pins the scope. A runtime event with no
// runtime id would be filed under the project's timeline and would be
// unreachable from the runtime endpoint, which is the one a client reads after
// a failure.
func TestEveryRuntimeEventNamesItsRuntime(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_scope")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	recorded := recorder.events()
	if len(recorded) != 3 {
		t.Fatalf("recorded %v, want three events", recorder.types())
	}
	for _, ev := range recorded {
		if ev.RuntimeID == "" {
			t.Errorf("a %s event carries no runtime id", ev.Type)
		}
		if ev.ProjectID != p.ID {
			t.Errorf("a %s event names project %q, want %q", ev.Type, ev.ProjectID, p.ID)
		}
		if ev.Source != event.SourceRuntime {
			t.Errorf("a %s event has source %q, want %q", ev.Type, ev.Source, event.SourceRuntime)
		}
	}
}

// TestRuntimeLifecycleReadsAsATimeline is the end-to-end shape of what a client
// sees: start, stop, start again, destroy - one runtime, one ordered list of
// what happened to it, including the restart.
func TestRuntimeLifecycleReadsAsATimeline(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingEvents{}

	p := testProject("evt_lifecycle")
	p.RuntimePath = t.TempDir()
	m, _ := testManagerRecording(t, recorder, p)

	steps := []struct {
		what string
		do   func() error
	}{
		{"start", func() error { _, err := m.Start(ctx, p.ID); return err }},
		{"stop", func() error { _, err := m.Stop(ctx, p.ID); return err }},
		{"start again", func() error { _, err := m.Start(ctx, p.ID); return err }},
		{"destroy", func() error { return m.Destroy(ctx, p.ID) }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%s returned an error: %v", step.what, err)
		}
	}

	want := []string{
		event.TypeRuntimeStarted,
		event.TypeRuntimeStopped,
		event.TypeRuntimeStarted,
		event.TypeRuntimeDestroyed,
	}
	got := recorder.types()
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recorded %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func equalPayload(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}
