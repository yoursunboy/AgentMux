package claude

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
)

// The manager owns adapters. These tests are about the ownership rather than
// about what an adapter observes: which runtime a receiver belongs to, what
// happens when one is attached twice, and what a caller asking about a runtime
// nobody is observing is told.

// testManager builds a manager whose adapters record into the given recorder.
func testManager(t *testing.T, recorder EventRecorder) *Manager {
	t.Helper()
	m := NewManager(ManagerOptions{
		Adapter: AdapterOptions{
			Recorder: recorder,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Now:      fixedNow,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("closing the manager failed: %v", err)
		}
	})
	return m
}

func TestAttachBindsAReceiverToTheRuntimeItWasGiven(t *testing.T) {
	m := testManager(t, newRecorder(t))

	att, err := m.Attach(context.Background(), Attachment{
		ProjectID:      "p_one",
		RuntimeID:      "amx-p_one",
		AgentSessionID: "sess_one",
		SessionID:      "11111111-2222-4333-8444-555555555555",
	})
	if err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	if att.HookURL == "" {
		t.Fatal("Attach returned no hook URL, so nothing could tell Claude where to deliver")
	}
	if !strings.HasPrefix(att.HookURL, "http://127.0.0.1:") {
		t.Errorf("HookURL = %q; want a loopback address", att.HookURL)
	}

	got, ok := m.Attachment("amx-p_one")
	if !ok {
		t.Fatal("the attachment was not recorded")
	}
	if got != att {
		t.Errorf("Attachment() = %+v; want what Attach returned, %+v", got, att)
	}
	if _, ok := m.Attachment("amx-p_other"); ok {
		t.Error("a runtime that was never attached reports an attachment")
	}
}

// TestAttachReplacesThePreviousAdapter is the rule that makes "the adapter for
// this runtime" a single fact.
//
// A second Claude session in one runtime has an id of its own, so the first
// adapter's port, hook path and settings document all describe something that no
// longer exists. Keeping it would leave a listener nothing will ever post to.
func TestAttachReplacesThePreviousAdapter(t *testing.T) {
	m := testManager(t, newRecorder(t))
	ctx := context.Background()

	first, err := m.Attach(ctx, Attachment{
		ProjectID: "p_one", RuntimeID: "amx-p_one", SessionID: "first",
	})
	if err != nil {
		t.Fatalf("the first Attach returned an error: %v", err)
	}
	second, err := m.Attach(ctx, Attachment{
		ProjectID: "p_one", RuntimeID: "amx-p_one", SessionID: "second",
	})
	if err != nil {
		t.Fatalf("the second Attach returned an error: %v", err)
	}

	if first.HookURL == second.HookURL {
		t.Errorf("both attachments share the address %q; a replacement mints a new one", first.HookURL)
	}
	if got, _ := m.Attachment("amx-p_one"); got.SessionID != "second" {
		t.Errorf("the recorded attachment is %q; want the second one", got.SessionID)
	}
	if got := len(m.Attachments()); got != 1 {
		t.Errorf("%d attachments are recorded; want 1", got)
	}

	// The old receiver is really gone: its address no longer answers.
	if _, err := net.Dial("tcp", strings.TrimPrefix(first.HookURL, "http://")); err == nil {
		t.Error("the replaced receiver is still accepting connections")
	}
}

// TestAttachRefusesARuntimeItWasNotTold pins §十一 of the phase that built the
// adapter: the identity is given and never guessed.
func TestAttachRefusesARuntimeItWasNotTold(t *testing.T) {
	m := testManager(t, newRecorder(t))

	_, err := m.Attach(context.Background(), Attachment{ProjectID: "p_one"})
	if !IsCode(err, CodeInvalidConfig) {
		t.Errorf("Attach with no runtime = %v; want %q", err, CodeInvalidConfig)
	}
	if got := len(m.Attachments()); got != 0 {
		t.Errorf("%d attachments were recorded for a refused call; want none", got)
	}
}

func TestDetachIsIdempotent(t *testing.T) {
	m := testManager(t, newRecorder(t))
	ctx := context.Background()

	if err := m.Detach(ctx, "amx-p_never_attached"); err != nil {
		t.Errorf("detaching a runtime that was never attached = %v; want nil", err)
	}

	if _, err := m.Attach(ctx, Attachment{ProjectID: "p_one", RuntimeID: "amx-p_one"}); err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if err := m.Detach(ctx, "amx-p_one"); err != nil {
			t.Fatalf("Detach #%d returned an error: %v", i, err)
		}
	}
	if _, ok := m.Attachment("amx-p_one"); ok {
		t.Error("the attachment survived a detach")
	}
}

// TestHookSettingsNeedsAnAttachedRuntime is the failure that would otherwise be
// silent: a settings document written for a receiver that is not listening names
// a port nothing will answer on, and an undelivered hook produces no error.
func TestHookSettingsNeedsAnAttachedRuntime(t *testing.T) {
	m := testManager(t, newRecorder(t))

	if _, err := m.HookSettings("amx-p_absent"); !IsCode(err, CodeNotStarted) {
		t.Errorf("HookSettings for an unattached runtime = %v; want %q", err, CodeNotStarted)
	}
	if _, err := m.Subscribe(context.Background(), "amx-p_absent"); !IsCode(err, CodeNotStarted) {
		t.Errorf("Subscribe for an unattached runtime = %v; want %q", err, CodeNotStarted)
	}

	if _, err := m.Attach(context.Background(), Attachment{
		ProjectID: "p_one", RuntimeID: "amx-p_one",
	}); err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	doc, err := m.HookSettings("amx-p_one")
	if err != nil {
		t.Fatalf("HookSettings returned an error: %v", err)
	}
	if !strings.Contains(string(doc), "allowedHttpHookUrls") {
		t.Errorf("the document is not a Claude settings document:\n%s", doc)
	}
}

// TestCloseDetachesEverything is what a server shutdown relies on. An adapter
// left listening is a port held for the life of the process, and in a test suite
// it is a leaked listener across tests.
func TestCloseDetachesEverything(t *testing.T) {
	m := testManager(t, newRecorder(t))
	ctx := context.Background()

	for _, id := range []string{"p_one", "p_two", "p_three"} {
		if _, err := m.Attach(ctx, Attachment{ProjectID: id, RuntimeID: "amx-" + id}); err != nil {
			t.Fatalf("Attach(%s) returned an error: %v", id, err)
		}
	}
	if got := len(m.Attachments()); got != 3 {
		t.Fatalf("%d attachments before Close; want 3", got)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
	if got := len(m.Attachments()); got != 0 {
		t.Errorf("%d attachments survived Close; want none", got)
	}
	if err := m.Close(ctx); err != nil {
		t.Errorf("a second Close returned an error: %v", err)
	}
}

// TestAttachIsSafeUnderConcurrency checks the locking rather than the outcome:
// the adapter's own guard, and the manager's, have to hold when several projects
// start at once. It is not a race-detector run - this build has no cgo, so
// `-race` is unavailable - but it is a test that fails loudly if the locks are
// removed rather than merely one that would.
func TestAttachIsSafeUnderConcurrency(t *testing.T) {
	m := testManager(t, newRecorder(t))
	ctx := context.Background()

	const projects = 8
	var wg sync.WaitGroup
	errs := make([]error, projects)
	for i := 0; i < projects; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "p_" + string(rune('a'+i))
			_, errs[i] = m.Attach(ctx, Attachment{ProjectID: id, RuntimeID: "amx-" + id})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Attach #%d returned an error: %v", i, err)
		}
	}
	if got := len(m.Attachments()); got != projects {
		t.Errorf("%d attachments; want %d", got, projects)
	}
}

// TestAttachmentsAreOrderedByRuntimeId is the determinism the adapter's own
// listings promise, held one level up.
func TestAttachmentsAreOrderedByRuntimeId(t *testing.T) {
	m := testManager(t, newRecorder(t))
	ctx := context.Background()

	for _, id := range []string{"p_c", "p_a", "p_b"} {
		if _, err := m.Attach(ctx, Attachment{ProjectID: id, RuntimeID: "amx-" + id}); err != nil {
			t.Fatalf("Attach(%s) returned an error: %v", id, err)
		}
	}
	got := m.Attachments()
	for i := 1; i < len(got); i++ {
		if got[i-1].RuntimeID >= got[i].RuntimeID {
			t.Errorf("attachments are not ordered: %q before %q", got[i-1].RuntimeID, got[i].RuntimeID)
		}
	}
}

// TestASessionIdIsPerLaunch is the property the whole correlation rests on.
//
// The manager does not mint ids - the coordinator does - and what it must not do
// is carry one forward. An attachment is the one it was given, and a fresh
// attachment of the same runtime with a different id reports the new one.
func TestASessionIdIsPerLaunch(t *testing.T) {
	m := testManager(t, newRecorder(t))
	ctx := context.Background()

	if _, err := m.Attach(ctx, Attachment{
		ProjectID: "p_one", RuntimeID: "amx-p_one", SessionID: "first",
	}); err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	second, err := m.Attach(ctx, Attachment{
		ProjectID: "p_one", RuntimeID: "amx-p_one", SessionID: "second",
	})
	if err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	if second.SessionID != "second" {
		t.Errorf("the attachment reports session %q; want the one it was given", second.SessionID)
	}
}
