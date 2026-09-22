package terminal

import (
	"context"
	"sync"
	"testing"

	"github.com/kutonlagos/agentmux/internal/usage"
)

// These tests are about the three events this package records and, more
// importantly, about the ones it does not.
//
// The interesting claim is not that a subscribe produces a row - it is that a
// resync does not, that a malformed message does not, and that a refused lease
// release does not. Each of those is a message a client genuinely sends, and a
// recorder that counted them would make "terminals opened" mean "frames
// received".

// recordingRecorder is a usage.Recorder that keeps what it was handed.
//
// It is guarded because the calls arrive from connection goroutines: a
// subscription is created in a read loop and the assertion is made from the
// test's goroutine, so without a lock this would be a data race the package's
// own tests are otherwise careful never to have.
type recordingRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingRecorder) Record(_ context.Context, eventType usage.EventType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, string(eventType))
}

// recorded returns a copy of what has been recorded so far.
func (r *recordingRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// newHubWithUsage builds a hub that records what it is told to.
//
// It is newHub with the one option that helper does not take. Everything else -
// the timings in particular - is the same, because a test about recording must
// run against the same transport the rest of the package is tested against.
func newHubWithUsage(t *testing.T, rt *fakeRuntime, recorder *recordingRecorder) *Hub {
	t.Helper()
	hub, err := NewHub(rt, HubOptions{
		Logger:       discardLogger(),
		Timings:      fastTimings(),
		ControlGrace: 0,
		Usage:        recorder,
	})
	if err != nil {
		t.Fatalf("terminal.NewHub returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := hub.Close(); err != nil {
			t.Errorf("closing the hub failed: %v", err)
		}
	})
	return hub
}

// TestTheTerminalRecordsItsThreeEvents is the happy path, in order.
//
// The order is asserted rather than the set, because the three are a sequence:
// a keyboard is asked for after a terminal is open, and given back after it was
// taken. A recorder wired into the wrong handler would still produce the same
// three strings.
func TestTheTerminalRecordsItsThreeEvents(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	recorder := &recordingRecorder{}
	hub := newHubWithUsage(t, rt, recorder)

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_0123456789abcdef"})

	// Connecting is not opening a terminal. A browser that has opened a socket
	// and subscribed to nothing is a page that has loaded.
	if got := recorder.recorded(); len(got) != 0 {
		t.Fatalf("connecting recorded %v, want nothing", got)
	}

	subscribeAll(t, testProject, desktop)
	desktop.becomeController(testProject)
	desktop.releaseControl(testProject)
	desktop.expectMessageOfType(MsgControlRevoked)
	desktop.expectMessageOfType(MsgControlChanged)

	want := []string{"terminal.connect", "controller.request", "controller.release"}
	got := recorder.recorded()
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAResyncIsNotASecondTerminal pins the boundary between opening a terminal
// and re-establishing its stream.
//
// A resync is what a client sends when its picture of a terminal has a gap, and
// it happens on a terminal that is already open - sometimes many times over one
// long session. Counting each one would make the number a count of network
// trouble rather than of terminals, which is the opposite of what it is for.
func TestAResyncIsNotASecondTerminal(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	recorder := &recordingRecorder{}
	hub := newHubWithUsage(t, rt, recorder)

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_0123456789abcdef"})
	subscribeAll(t, testProject, desktop)

	desktop.send(map[string]any{"type": MsgResync, "projectId": testProject})
	// The resync produces a fresh snapshot, which is how the test knows it was
	// acted on rather than ignored.
	desktop.expectFrame()
	desktop.drainControl()

	got := recorder.recorded()
	if len(got) != 1 || got[0] != "terminal.connect" {
		t.Errorf("recorded %v after a resync, want one terminal.connect", got)
	}
}

// TestABrokenControlRequestIsNotRecorded draws the line the handler draws.
//
// The three refusals below are a client that sent something unusable: a project
// id that is not one, a message with fields that do not belong on it, and a
// request to control a terminal the client is not watching. None of them is a
// person reaching for a keyboard, and a count that included them would be a
// count of messages.
func TestABrokenControlRequestIsNotRecorded(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	recorder := &recordingRecorder{}
	hub := newHubWithUsage(t, rt, recorder)

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_0123456789abcdef"})
	subscribeAll(t, testProject, desktop)
	before := len(recorder.recorded())

	desktop.send(map[string]any{"type": MsgControlRequest, "projectId": "not-a-project-id"})
	desktop.expectMessageOfType(MsgError)
	desktop.send(map[string]any{"type": MsgControlRequest, "projectId": testProject, "cols": 80, "rows": 24})
	desktop.expectMessageOfType(MsgError)

	// A second connection that has subscribed to nothing at all.
	stranger := connection(t, hub, rt, clientOptions{clientID: "c_fedcba9876543210"})
	stranger.requestControl(testProject)
	stranger.expectMessageOfType(MsgError)

	if got := recorder.recorded(); len(got) != before {
		t.Errorf("recorded %v for three unusable messages, want nothing new", got[before:])
	}
}

// TestARefusedControlRequestIsRecorded is the other half of the line above, and
// it is the one that would be lost by moving the record after the answer.
//
// A request that is denied because another device holds the lease is still a
// person asking for a keyboard. An installation where it happens constantly is
// one whose people are competing for a terminal, and that is a finding - one
// that a count of granted leases alone would report as a quiet, healthy beta.
func TestARefusedControlRequestIsRecorded(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	recorder := &recordingRecorder{}
	hub := newHubWithUsage(t, rt, recorder)

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_0123456789abcdef"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_fedcba9876543210"})
	subscribeAll(t, testProject, desktop, tablet)

	desktop.becomeController(testProject)

	// The tablet is watching, so its request reaches the authority - and the
	// authority queues it rather than granting it.
	tablet.requestControl(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	tablet.drainControl()

	// The desktop is told somebody is waiting, which is how the test knows the
	// request was received rather than still in flight.
	desktop.expectMessageOfType(MsgControlChanged)

	var requests int
	for _, event := range recorder.recorded() {
		if event == "controller.request" {
			requests++
		}
	}
	if requests != 2 {
		t.Errorf("recorded %d controller requests, want one per client", requests)
	}
}

// TestAReleaseThatChangedNothingIsNotRecorded is the asymmetry between the two
// lease messages, stated as a test.
//
// A viewer that sends a release holds nothing, and the answer is an error. That
// is a client talking about a keyboard it never had, not a keyboard being given
// back - and unlike a request, the outcome it would be folded into is one where
// it is simply wrong.
func TestAReleaseThatChangedNothingIsNotRecorded(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	recorder := &recordingRecorder{}
	hub := newHubWithUsage(t, rt, recorder)

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_0123456789abcdef"})
	subscribeAll(t, testProject, desktop)

	desktop.releaseControl(testProject)
	desktop.expectMessageOfType(MsgError)

	for _, event := range recorder.recorded() {
		if event == "controller.release" {
			t.Fatalf("a release that changed nothing was recorded: %v", recorder.recorded())
		}
	}
}

// TestAHubWithNoRecorderIsSilent covers the ordinary installation.
//
// It is not decoration: every other test in this package builds a hub with no
// recorder at all, so a nil dereference here would already have failed the
// whole suite. It is written down so that the reason those tests pass is a
// property somebody stated rather than a coincidence.
func TestAHubWithNoRecorderIsSilent(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_0123456789abcdef"})
	subscribeAll(t, testProject, desktop)
	desktop.becomeController(testProject)
	desktop.releaseControl(testProject)
	desktop.expectMessageOfType(MsgControlRevoked)
	desktop.expectMessageOfType(MsgControlChanged)
}
