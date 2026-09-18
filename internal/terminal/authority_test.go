package terminal

import (
	"sync"
	"testing"
	"time"
)

// This file tests the control plane on its own: no sockets, no hub, no
// protocol. The authority is the thing that decides who may type, and a rule
// that can only be checked by opening a WebSocket is a rule whose edge cases are
// checked by opening a WebSocket.
//
// Time is the test's to move, mostly. Every question about a grace period is a
// question about what happens when time passes, and a test that had to wait
// thirty seconds to ask it is a test somebody deletes. Where a rule is genuinely
// about a timer firing, the timer is real and the grace period is short.

// testAuthority builds an authority whose clock the test drives.
func testAuthority(t *testing.T, grace time.Duration) (*Authority, *testClock, *expiries) {
	t.Helper()

	clock := &testClock{at: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	expired := &expiries{}
	a := NewAuthority(AuthorityOptions{
		Grace:   grace,
		Now:     clock.read,
		Logger:  discardLogger(),
		Expired: expired.record,
	})
	t.Cleanup(a.Close)
	return a, clock, expired
}

// testClock is a clock a test moves by hand.
type testClock struct {
	at time.Time
}

func (c *testClock) read() time.Time { return c.at }

// expiries records the projects whose leases lapsed.
//
// It is guarded rather than a bare slice because the callback runs on a timer's
// goroutine while the test is polling on its own - and a race detector is
// entitled to complain about that even when the test would pass.
type expiries struct {
	mu       sync.Mutex
	projects []string
}

func (e *expiries) record(projectID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.projects = append(e.projects, projectID)
}

func (e *expiries) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.projects...)
}

// ---------------------------------------------------------------------------
// Acquiring

func TestAnUnclaimedProjectGoesToTheFirstClientThatAsks(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)

	outcome, reason := a.Request(testProject, "c_aaaa", "ws_1", "Chrome on Windows")
	if outcome != RequestGranted {
		t.Fatalf("the first request was answered %v, want a grant", outcome)
	}
	if reason != ReasonAvailable {
		t.Errorf("the grant gave reason %q, want %q", reason, ReasonAvailable)
	}
	lease := a.Lease(testProject)
	if lease.ControllerID != "c_aaaa" {
		t.Errorf("the lease is held by %q, want %q", lease.ControllerID, "c_aaaa")
	}
	if lease.Device != "Chrome on Windows" {
		t.Errorf("the lease carries device %q, want the one that asked", lease.Device)
	}
	if lease.GrantedAt.IsZero() {
		t.Error("the lease does not record when it was granted")
	}
}

// TestNobodyMayTypeIntoAProjectNobodyIsControlling is the rule the phase exists
// for.
//
// A client that has not asked is a viewer, and a viewer's keystroke does not
// reach a terminal - nor is it queued until somebody claims the project. Control
// is asked for and granted, never assumed.
func TestNobodyMayTypeIntoAProjectNobodyIsControlling(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)

	for name, decision := range map[string]Decision{
		"input":  a.MayInput(testProject, "c_aaaa"),
		"resize": a.MayResize(testProject, "c_aaaa"),
	} {
		if decision.Allowed {
			t.Errorf("a client that has not asked was allowed to %s", name)
			continue
		}
		if decision.Code != CodeNotController {
			t.Errorf("the %s refusal carried code %q, want %q", name, decision.Code, CodeNotController)
		}
		if decision.Message == "" {
			t.Errorf("the %s refusal does not say what to do instead", name)
		}
	}

	// And an unknown project is not a special case: it is simply not controlled.
	if a.MayInput("p_neverheardofthem", "c_aaaa").Allowed {
		t.Error("a project the server has never seen was typable")
	}
}

func TestAViewerIsRefusedAndTheControllerIsAllowed(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	if !a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("the controller was refused its own terminal")
	}
	if !a.MayResize(testProject, "c_aaaa").Allowed {
		t.Error("the controller was refused the size of its own terminal")
	}
	if a.MayInput(testProject, "c_bbbb").Allowed {
		t.Error("a viewer was allowed to type")
	}
	if a.MayResize(testProject, "c_bbbb").Allowed {
		t.Error("a viewer was allowed to resize")
	}
}

func TestAskingTwiceIsNotASecondGrant(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	grantedAt := a.Lease(testProject).GrantedAt

	outcome, reason := a.Request(testProject, "c_aaaa", "ws_2", "Chrome on Windows")
	if outcome != RequestHeld {
		t.Fatalf("asking again was answered %v, want %v", outcome, RequestHeld)
	}
	if reason != ReasonHeld {
		t.Errorf("the answer gave reason %q, want %q", reason, ReasonHeld)
	}
	// The grant time is unchanged, because nothing was granted. A client that
	// re-affirms on every reconnect must not be able to make its own lease look
	// newer than it is.
	if got := a.Lease(testProject).GrantedAt; !got.Equal(grantedAt) {
		t.Errorf("the grant time moved from %v to %v", grantedAt, got)
	}
}

func TestARequestAgainstALiveControllerIsQueuedRatherThanGranted(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	outcome, _ := a.Request(testProject, "c_bbbb", "ws_2", "Safari on iPad")
	if outcome != RequestQueued {
		t.Fatalf("the second request was answered %v, want a queue", outcome)
	}
	// Nothing moved. A request is not a takeover: the terminal is still the
	// first client's, and the first client is the one that decides.
	if got := a.Lease(testProject).ControllerID; got != "c_aaaa" {
		t.Errorf("the controller is now %q, want it unchanged at %q", got, "c_aaaa")
	}
	if a.MayInput(testProject, "c_bbbb").Allowed {
		t.Error("asking for control granted it without the controller agreeing")
	}
	pending := a.Lease(testProject).Pending
	if len(pending) != 1 || pending[0].ClientID != "c_bbbb" {
		t.Fatalf("pending requests = %+v, want one from c_bbbb", pending)
	}
	if pending[0].Device != "Safari on iPad" {
		t.Errorf("the pending request carries device %q, want the asker's", pending[0].Device)
	}
	if pending[0].Since.IsZero() {
		t.Error("the pending request does not record when it was made, so a roster cannot order it")
	}
}

func TestARequestIsQueuedOnlyOnce(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	// A client that asks on every reconnect - or a page that re-sends on every
	// render - must not fill the queue with copies of itself.
	for i := 0; i < 4; i++ {
		if outcome, _ := a.Request(testProject, "c_bbbb", "ws_2", "iPad"); outcome != RequestQueued {
			t.Fatalf("request %d was answered %v, want a queue", i, outcome)
		}
	}
	if got := len(a.Lease(testProject).Pending); got != 1 {
		t.Errorf("there are %d pending requests from one client, want 1", got)
	}
}

func TestTheQueueIsBounded(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	for i := 0; i < MaxPendingRequests; i++ {
		mustQueue(t, a, testProject, askerID(i), askerID(i))
	}
	if got := len(a.Lease(testProject).Pending); got != MaxPendingRequests {
		t.Fatalf("the queue holds %d requests, want %d", got, MaxPendingRequests)
	}

	// The refusal is explicit rather than a silent drop: a client left believing
	// it is waiting for an answer that will never come has no way to find out.
	outcome, reason := a.Request(testProject, "c_late", "ws_y", "iPad")
	if outcome != RequestDenied {
		t.Fatalf("the request past the limit was answered %v, want a refusal", outcome)
	}
	if reason != ReasonTooManyRequests {
		t.Errorf("the refusal gave reason %q, want %q", reason, ReasonTooManyRequests)
	}
	if got := len(a.Lease(testProject).Pending); got != MaxPendingRequests {
		t.Errorf("the refused request was queued anyway: %d entries", got)
	}
}

// TestSimultaneousRequestsProduceExactlyOneController is the race the lease
// exists to make impossible.
//
// Two devices asking at the same instant is the ordinary case, not an exotic
// one: two tabs restored by a session restore, or a person clicking Request
// Control on a tablet while their laptop reconnects. Exactly one may win, and
// nobody else may be told they have it.
func TestSimultaneousRequestsProduceExactlyOneController(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)

	const askers = 24
	outcomes := make([]RequestOutcome, askers)
	reasons := make([]string, askers)
	start := make(chan struct{})
	done := make(chan struct{}, askers)

	for i := 0; i < askers; i++ {
		go func(i int) {
			<-start
			outcomes[i], reasons[i] = a.Request(testProject, askerID(i), "ws_x", "iPad")
			done <- struct{}{}
		}(i)
	}
	close(start)
	for i := 0; i < askers; i++ {
		<-done
	}

	granted := 0
	for i, outcome := range outcomes {
		switch outcome {
		case RequestGranted:
			granted++
		case RequestQueued:
		case RequestDenied:
			// The queue filled before this one arrived. That is a legitimate
			// answer, and it is the bound doing its job rather than a second
			// grant slipping through.
			if reasons[i] != ReasonTooManyRequests {
				t.Errorf("asker %d was refused for %q, want %q", i, reasons[i], ReasonTooManyRequests)
			}
		default:
			t.Errorf("asker %d was answered %v, want a grant, a queue or a refusal", i, outcome)
		}
	}
	if granted != 1 {
		t.Fatalf("%d askers were granted the lease, want exactly 1", granted)
	}

	// And the winner is a real client that can now type, while every loser
	// cannot.
	lease := a.Lease(testProject)
	if lease.ControllerID == "" {
		t.Fatal("the lease names no controller after a grant")
	}
	if !a.MayInput(testProject, lease.ControllerID).Allowed {
		t.Error("the client that won the race cannot type")
	}
	for i, outcome := range outcomes {
		if outcome == RequestGranted {
			continue
		}
		if a.MayInput(testProject, askerID(i)).Allowed {
			t.Errorf("asker %d lost the race and can still type", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Releasing

func TestReleasingHandsTheLeaseBackToNobody(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	if !a.Release(testProject, "c_aaaa") {
		t.Fatal("the controller could not give up its own lease")
	}
	if lease := a.Lease(testProject); lease.Holder() {
		t.Errorf("the lease is still held by %q after a release", lease.ControllerID)
	}
	if a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("a client that released its lease was still allowed to type")
	}
	// A release is not a transfer. The next client to ask gets it, but nobody
	// gets it by being next in line.
	if outcome, _ := a.Request(testProject, "c_bbbb", "ws_2", "iPad"); outcome != RequestGranted {
		t.Errorf("the project could not be taken after a release: %v", outcome)
	}
}

func TestAViewerCannotReleaseSomebodyElsesLease(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	if a.Release(testProject, "c_bbbb") {
		t.Fatal("a viewer released a lease it did not hold")
	}
	if got := a.Lease(testProject).ControllerID; got != "c_aaaa" {
		t.Errorf("the controller is %q, want it unchanged at %q", got, "c_aaaa")
	}
}

func TestReleasingDoesNotHandTheLeaseToWhoeverIsWaiting(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	a.Release(testProject, "c_aaaa")

	// The queued client is not promoted by a release. Stepping back is not the
	// same as choosing a successor - that is what Accept is - and a design where
	// it were would make "release" a way to hand somebody control without
	// saying so.
	if got := a.Lease(testProject).ControllerID; got != "" {
		t.Errorf("a release granted the lease to %q", got)
	}
	if a.MayInput(testProject, "c_bbbb").Allowed {
		t.Error("a queued client can type because the controller stepped back")
	}
	// And the queue is gone with the controller. A pending request is a question
	// put to whoever holds the lease; leaving it behind would show every browser
	// a client waiting on a decision that nobody is in a position to make.
	if pending := a.Lease(testProject).Pending; len(pending) != 0 {
		t.Errorf("a released project still lists %+v as waiting", pending)
	}
	// Asking again is what gets it, and it gets it because the project is free
	// rather than because it waited longest.
	if outcome, reason := a.Request(testProject, "c_bbbb", "ws_2", "iPad"); outcome != RequestGranted {
		t.Errorf("asking for a released project was answered %v (%s), want a grant", outcome, reason)
	}
}

// ---------------------------------------------------------------------------
// Transferring

func TestTransferMovesTheLeaseToTheClientThatAsked(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	if outcome := a.Accept(testProject, "c_aaaa", "c_bbbb"); outcome != TransferDone {
		t.Fatalf("the transfer was answered %v, want done", outcome)
	}
	if !a.MayInput(testProject, "c_bbbb").Allowed {
		t.Error("the new controller cannot type")
	}
	if a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("the old controller can still type")
	}
	// The queue is cleared, not merely shortened: the question everybody was
	// waiting on has been answered.
	if pending := a.Lease(testProject).Pending; len(pending) != 0 {
		t.Errorf("the queue still holds %+v after a transfer", pending)
	}
	// The new lease names the client that asked, and one of that client's
	// connections - the transfer is performed by somebody who has no idea what
	// socket the other device is on.
	lease := a.Lease(testProject)
	if lease.ControllerID != "c_bbbb" {
		t.Errorf("the lease is held by %q, want %q", lease.ControllerID, "c_bbbb")
	}
	if lease.ConnectionID != "ws_2" {
		t.Errorf("the lease names connection %q, want the new controller's", lease.ConnectionID)
	}
	if lease.Suspended {
		t.Error("a freshly transferred lease is marked suspended")
	}
}

// TestATransferPicksAConnectionOfTheClientItMovesTo covers the one thing a
// transfer has to guess at.
//
// The controller performing it has no idea what socket the other device is on,
// so the authority looks one up. Any of them would do - the field is a record,
// not a route - but the choice is made deterministically, because a lease that
// named a different connection on each run would be a lease no test could assert
// on and no log could be compared against.
func TestATransferPicksAConnectionOfTheClientItMovesTo(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	// The client has two connections open: an old socket not yet reaped and the
	// one that replaced it.
	a.Attach("c_bbbb", "ws_9", "iPad")
	a.Attach("c_bbbb", "ws_3", "iPad")
	mustQueue(t, a, testProject, "c_bbbb", "ws_3")

	if outcome := a.Accept(testProject, "c_aaaa", "c_bbbb"); outcome != TransferDone {
		t.Fatalf("the transfer was answered %v, want done", outcome)
	}
	if got := a.Lease(testProject).ConnectionID; got != "ws_3" {
		t.Errorf("the lease names connection %q, want the lowest of the client's connections", got)
	}
}

func TestATransferToAClientWithNoConnectionsIsStillATransfer(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")
	a.Detach("c_bbbb", "ws_2")

	// A client that asked and then closed its browser still has a claim: it is
	// in the queue, and the controller can still choose to hand the terminal
	// over to it. The lease records no connection, which is the truth - the
	// client has none - and the grant stands for whenever it comes back.
	if outcome := a.Accept(testProject, "c_aaaa", "c_bbbb"); outcome != TransferDone {
		t.Fatalf("the transfer was answered %v, want done", outcome)
	}
	lease := a.Lease(testProject)
	if lease.ControllerID != "c_bbbb" {
		t.Errorf("the lease is held by %q, want %q", lease.ControllerID, "c_bbbb")
	}
	if lease.ConnectionID != "" {
		t.Errorf("the lease names connection %q, but the client has none open", lease.ConnectionID)
	}
	// And the new holder is suspended from the outset, because it is not
	// present - so its grace period is already running rather than the lease
	// sitting live for a client that has gone.
	if !lease.Suspended {
		t.Error("a lease granted to an absent client is not suspended")
	}
}

func TestATransferNeedsTheAskerToHaveAsked(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	// Nobody may be handed control that did not ask for it: a client pushed
	// into being the controller is a client whose next keystroke goes somewhere
	// it did not expect.
	if outcome := a.Accept(testProject, "c_aaaa", "c_stranger"); outcome != TransferNotPending {
		t.Fatalf("handing control to a client that never asked was answered %v, want %v",
			outcome, TransferNotPending)
	}
	if got := a.Lease(testProject).ControllerID; got != "c_aaaa" {
		t.Errorf("the lease moved to %q, want it unchanged at %q", got, "c_aaaa")
	}
}

func TestATransferNeedsTheSenderToBeTheController(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	// A viewer cannot hand over what it does not hold, and it cannot push
	// control at somebody else - which is also what stops a queued client from
	// promoting itself.
	if outcome := a.Accept(testProject, "c_bbbb", "c_bbbb"); outcome != TransferNotController {
		t.Errorf("a non-controller's transfer was answered %v, want %v",
			outcome, TransferNotController)
	}
	if a.MayInput(testProject, "c_bbbb").Allowed {
		t.Error("a queued client promoted itself by accepting its own request")
	}
}

func TestRejectingLeavesTheControllerInPlace(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	if outcome := a.Reject(testProject, "c_aaaa", "c_bbbb"); outcome != TransferDone {
		t.Fatalf("the refusal was answered %v, want done", outcome)
	}
	if got := a.Lease(testProject).ControllerID; got != "c_aaaa" {
		t.Errorf("the controller is %q, want it unchanged at %q", got, "c_aaaa")
	}
	if pending := a.Lease(testProject).Pending; len(pending) != 0 {
		t.Errorf("the refused request is still queued: %+v", pending)
	}
	if !a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("refusing a request cost the controller its own lease")
	}
}

func TestOnlyTheControllerMayReject(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	// Otherwise a queued client could clear the queue - or dismiss its own
	// request and be re-queued behind everybody.
	if outcome := a.Reject(testProject, "c_bbbb", "c_bbbb"); outcome != TransferNotController {
		t.Errorf("a non-controller's rejection was answered %v, want %v",
			outcome, TransferNotController)
	}
	if len(a.Lease(testProject).Pending) != 1 {
		t.Error("a non-controller's rejection removed a pending request")
	}
}

func TestWithdrawingRemovesAQueuedRequest(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	a.Withdraw(testProject, "c_bbbb")

	if pending := a.Lease(testProject).Pending; len(pending) != 0 {
		t.Errorf("a withdrawn request is still queued: %+v", pending)
	}
	// Withdrawing is not releasing: the client that withdrew was never in
	// charge, and the controller is untouched.
	if got := a.Lease(testProject).ControllerID; got != "c_aaaa" {
		t.Errorf("the controller is %q, want it unchanged at %q", got, "c_aaaa")
	}
}

// ---------------------------------------------------------------------------
// Disconnecting

func TestALeaseIsSuspendedWhenItsControllerDisconnects(t *testing.T) {
	a, clock, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	suspended := a.Detach("c_aaaa", "ws_1")
	if len(suspended) != 1 || suspended[0] != testProject {
		t.Fatalf("the disconnect suspended %v, want just %q", suspended, testProject)
	}

	lease := a.Lease(testProject)
	if !lease.Suspended {
		t.Fatal("the lease is not marked suspended while its controller is away")
	}
	if got := lease.ExpiresAt.Sub(clock.at); got != time.Minute {
		t.Errorf("the lease expires in %v, want the grace period of %v", got, time.Minute)
	}
	// A suspended lease is still held. If it were free, the grace period would
	// be a window in which anybody could take the terminal simply by asking
	// while its owner's laptop was closed.
	if !lease.Holder() {
		t.Error("a suspended lease reports as unheld, so anybody could take it")
	}
}

func TestASuspendedLeaseRefusesARequestRatherThanGrantingIt(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	a.Detach("c_aaaa", "ws_1")

	outcome, reason := a.Request(testProject, "c_bbbb", "ws_2", "iPad")
	if outcome != RequestDenied {
		t.Fatalf("a request during the grace period was answered %v, want a refusal", outcome)
	}
	if reason != ReasonControllerExists {
		t.Errorf("the refusal gave reason %q, want %q", reason, ReasonControllerExists)
	}
	if got := a.Lease(testProject).ControllerID; got != "c_aaaa" {
		t.Errorf("the lease moved to %q during the grace period", got)
	}
	if len(a.Lease(testProject).Pending) != 0 {
		t.Error("a request refused during the grace period was queued anyway")
	}
}

func TestASuspendedLeaseCannotBeUsedByItsAbsentController(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	a.Detach("c_aaaa", "ws_1")

	// The lease names this client, but the client has no connection - so a
	// message arriving under that identifier is a stale or forged one, and the
	// answer is the same as for a stranger. Without this, a leaked client id
	// would be a terminal anyone could type into while its owner was away.
	if a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("an absent controller was allowed to type")
	}
	if a.Release(testProject, "c_aaaa") {
		t.Error("an absent controller released a lease it is not present for")
	}
}

func TestTheSameClientReconnectingResumesItsLease(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	grantedAt := a.Lease(testProject).GrantedAt
	a.Detach("c_aaaa", "ws_1")

	resumed := a.Attach("c_aaaa", "ws_2", "Chrome on Windows")
	if len(resumed) != 1 || resumed[0] != testProject {
		t.Fatalf("the reconnect resumed %v, want just %q", resumed, testProject)
	}
	lease := a.Lease(testProject)
	if lease.Suspended {
		t.Error("the lease is still suspended after its client came back")
	}
	if !lease.ExpiresAt.IsZero() {
		t.Errorf("a resumed lease still carries an expiry of %v", lease.ExpiresAt)
	}
	if !a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("the client that reconnected cannot type")
	}
	// Resuming is not a fresh grant: a client's tenure is not restarted by its
	// wifi dropping, or a laptop that reconnects every minute would hold the
	// terminal forever on renewals nobody asked for.
	if !lease.GrantedAt.Equal(grantedAt) {
		t.Errorf("the resumed lease was granted at %v, want the original %v",
			lease.GrantedAt, grantedAt)
	}
}

// TestASecondConnectionKeepsTheLease is the case the presence map exists for.
//
// A browser opens its replacement socket before the old one is reaped, and a
// design that suspended on every socket close would suspend on every reconnect
// - which is exactly the case the grace period exists to make invisible.
func TestASecondConnectionKeepsTheLease(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	a.Attach("c_aaaa", "ws_2", "Chrome on Windows")

	if suspended := a.Detach("c_aaaa", "ws_1"); len(suspended) != 0 {
		t.Fatalf("closing one of two connections suspended %v", suspended)
	}
	if a.Lease(testProject).Suspended {
		t.Error("the lease was suspended while the client still had a connection open")
	}
	if !a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("the client was refused while one of its connections was still open")
	}
	// And when the last one goes, the lease is suspended as it should be.
	if suspended := a.Detach("c_aaaa", "ws_2"); len(suspended) != 1 {
		t.Errorf("closing the last connection suspended %v, want one project", suspended)
	}
	if !a.Lease(testProject).Suspended {
		t.Error("the lease survived the client's last connection closing")
	}
}

func TestALeaseLapsesWhenTheGracePeriodRunsOut(t *testing.T) {
	// A grace period short enough to wait out for real. This is the one case in
	// the file where a timer must actually fire, and a timer that is faked is a
	// timer that is not being tested.
	a, _, expired := testAuthority(t, 40*time.Millisecond)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	a.Detach("c_aaaa", "ws_1")

	// Wait on the callback rather than on the release. The expiry releases the
	// lease under the lock and calls back after letting go of it - deliberately,
	// so that a callback which broadcasts cannot deadlock against the authority
	// - which leaves a window where the lease is free and nothing has been
	// reported yet.
	waitUntil(t, 5*time.Second, func() bool { return len(expired.list()) == 1 })
	if a.Lease(testProject).Holder() {
		t.Fatal("the lease is still held after its grace period lapsed")
	}
	if got := expired.list(); got[0] != testProject {
		t.Errorf("the expiry named %q, want just %q", got[0], testProject)
	}
	// And now the project is free, which is the point of a grace period ending:
	// somebody else can have the terminal without going to find a person.
	if outcome, _ := a.Request(testProject, "c_bbbb", "ws_2", "iPad"); outcome != RequestGranted {
		t.Errorf("the project could not be taken after the grace period: %v", outcome)
	}
}

// TestALapsedLeaseLeavesNoQueueBehind is the queue following the controller out.
//
// A pending request is a question put to whoever holds the lease. When the lease
// lapses there is nobody left holding it, so an entry that survived would be a
// browser showing a client as waiting for a decision that cannot be made - in a
// roster that says, correctly, that the project is free.
func TestALapsedLeaseLeavesNoQueueBehind(t *testing.T) {
	a, _, expired := testAuthority(t, 40*time.Millisecond)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	a.Detach("c_aaaa", "ws_1")

	waitUntil(t, 5*time.Second, func() bool { return len(expired.list()) == 1 })
	if pending := a.Lease(testProject).Pending; len(pending) != 0 {
		t.Errorf("a lapsed lease still lists %+v as waiting", pending)
	}
}

// TestAGraceTimerCannotReleaseTheLeaseThatReplacedIt is the generation counter.
//
// A reconnect and a timer cross: the client comes back at the moment its grace
// period ends. Without the counter, the timer armed for the old connection would
// release the lease the reconnect just restored, and the client would be told it
// had control while the server thought nobody did.
//
// The crossing is staged rather than raced, because a test that depends on two
// goroutines interleaving in a particular order passes on the machine where the
// bug is invisible.
func TestAGraceTimerCannotReleaseTheLeaseThatReplacedIt(t *testing.T) {
	a, _, expired := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	a.Detach("c_aaaa", "ws_1")

	stale := generationOf(t, a, testProject)

	// The client comes back. Its lease is restored and the timer is superseded.
	a.Attach("c_aaaa", "ws_2", "Chrome on Windows")

	// Now the old timer runs late, believing it is still responsible.
	a.expire(testProject, "c_aaaa", stale)

	lease := a.Lease(testProject)
	if !lease.Holder() || lease.Suspended {
		t.Fatalf("a stale timer released a live lease: %+v", lease)
	}
	if !a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("a stale timer cost the controller its terminal")
	}
	if got := expired.list(); len(got) != 0 {
		t.Errorf("a stale timer reported an expiry for %v", got)
	}
}

// TestAnExpiryForAProjectSomebodyElseTookDoesNothing is the other half of the
// same guard: the lease did not merely get restored, it changed hands.
func TestAnExpiryForAProjectSomebodyElseTookDoesNothing(t *testing.T) {
	a, _, expired := testAuthority(t, 40*time.Millisecond)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	a.Detach("c_aaaa", "ws_1")
	stale := generationOf(t, a, testProject)

	// Let the grace period pass for real, and let somebody else take it.
	waitUntil(t, 5*time.Second, func() bool { return len(expired.list()) == 1 })
	mustRequest(t, a, testProject, "c_bbbb", "ws_2")

	// The late timer names the previous controller, which is no longer this
	// project's. It must not release the lease the new one holds.
	a.expire(testProject, "c_aaaa", stale)

	lease := a.Lease(testProject)
	if lease.ControllerID != "c_bbbb" || lease.Suspended {
		t.Fatalf("a timer for the previous controller disturbed the new lease: %+v", lease)
	}
	if !a.MayInput(testProject, "c_bbbb").Allowed {
		t.Error("a timer for the previous controller cost the new one its terminal")
	}
	if got := len(expired.list()); got != 1 {
		t.Errorf("the stale timer reported a second expiry: %v", expired.list())
	}
}

func TestDisconnectingAViewerChangesNothing(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")

	// A viewer's connection closing suspends nothing - it never held anything to
	// suspend.
	if suspended := a.Detach("c_bbbb", "ws_2"); len(suspended) != 0 {
		t.Errorf("a viewer's disconnect suspended %v, want nothing", suspended)
	}
	if got := a.Lease(testProject).ControllerID; got != "c_aaaa" {
		t.Errorf("a viewer's disconnect moved the lease to %q", got)
	}
	// Its request is left where it is: the client is not gone, one of its
	// sockets is. A client with no connections at all is a different question,
	// and the answer to it belongs to the queue rather than to the lease.
	if len(a.Lease(testProject).Pending) != 1 {
		t.Error("a viewer's disconnect silently dropped its queued request")
	}
}

func TestDisconnectingAClientWithNoLeaseIsHarmless(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)

	// Every path here is reached in production by a socket that closed before it
	// ever subscribed.
	if got := a.Detach("c_never-seen", "ws_404"); got != nil {
		t.Errorf("detaching an unknown client reported %v", got)
	}
	if got := a.Detach("", "ws_404"); got != nil {
		t.Errorf("detaching an empty client id reported %v", got)
	}
	if got := a.Attach("", "ws_404", "Chrome"); got != nil {
		t.Errorf("attaching an empty client id resumed %v", got)
	}
}

// ---------------------------------------------------------------------------
// Isolation

// TestOneProjectsLeaseIsAnothersNothing is §三十四's isolation, at the level
// where it can actually be broken.
//
// The authority is one map keyed by project, and every method reads exactly one
// key. A client holding control of one project must be a viewer of every other,
// and no amount of control in one may leak into another.
func TestOneProjectsLeaseIsAnothersNothing(t *testing.T) {
	const other = "p_ffffffffffffffffffff"
	a, _, _ := testAuthority(t, time.Minute)
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")

	if a.MayInput(other, "c_aaaa").Allowed {
		t.Error("control of one project allowed typing into another")
	}
	if a.Lease(other).Holder() {
		t.Error("granting control of one project created one for another")
	}
	// And the other project can be held by somebody else entirely, at once.
	mustRequest(t, a, other, "c_bbbb", "ws_2")
	if !a.MayInput(other, "c_bbbb").Allowed {
		t.Error("the second project's controller cannot type into it")
	}
	if !a.MayInput(testProject, "c_aaaa").Allowed {
		t.Error("the second grant disturbed the first project's lease")
	}
	if a.MayInput(testProject, "c_bbbb").Allowed {
		t.Error("control of one project leaked into another")
	}
	// A disconnect is per project too: one client letting go of one terminal
	// must not suspend the other.
	a.Detach("c_aaaa", "ws_1")
	if !a.MayInput(other, "c_bbbb").Allowed {
		t.Error("one project's disconnect suspended another's lease")
	}
}

// TestTheAuthorityForgetsProjectsNobodyIsUsing is the map's bound.
//
// Without it, a server grows one entry per project anybody ever controlled, and
// the entries are exactly the ones that will never be looked at again.
func TestTheAuthorityForgetsProjectsNobodyIsUsing(t *testing.T) {
	a, _, _ := testAuthority(t, time.Minute)

	entries := func() int {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.project)
	}

	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	if got := entries(); got != 1 {
		t.Fatalf("a grant left %d entries, want 1", got)
	}
	a.Release(testProject, "c_aaaa")
	if got := entries(); got != 0 {
		t.Errorf("the authority holds %d entries after a release, want none", got)
	}

	// A queue is the other way an entry outlives its controller, and a client
	// withdrawing is what empties it.
	mustRequest(t, a, testProject, "c_aaaa", "ws_1")
	mustQueue(t, a, testProject, "c_bbbb", "ws_2")
	a.Release(testProject, "c_aaaa")
	a.Withdraw(testProject, "c_bbbb")
	if got := entries(); got != 0 {
		t.Errorf("the authority holds %d entries after the last request withdrew, want none", got)
	}
}

// ---------------------------------------------------------------------------
// Helpers

// mustRequest gives a client a connection and grants it a lease, failing the
// test if either does not happen.
//
// The attach is not decoration. A request in production arrives on a socket that
// was attached when it opened, and the presence it records is what tells "the
// client left" apart from "one of the client's sockets closed" - so a helper
// that granted a lease to a client with no connections would be testing a state
// the server cannot reach.
func mustRequest(t *testing.T, a *Authority, projectID, clientID, connectionID string) {
	t.Helper()
	a.Attach(clientID, connectionID, "Chrome on Windows")
	outcome, reason := a.Request(projectID, clientID, connectionID, "Chrome on Windows")
	if outcome != RequestGranted {
		t.Fatalf("%s asking for %s was answered %v (%s), want a grant",
			clientID, projectID, outcome, reason)
	}
}

// mustQueue connects a client and puts it in line, failing the test if it is not
// queued.
func mustQueue(t *testing.T, a *Authority, projectID, clientID, connectionID string) {
	t.Helper()
	a.Attach(clientID, connectionID, "iPad")
	outcome, reason := a.Request(projectID, clientID, connectionID, "iPad")
	if outcome != RequestQueued {
		t.Fatalf("%s asking for %s was answered %v (%s), want a queue",
			clientID, projectID, outcome, reason)
	}
}

// askerID names the nth client asking at once.
func askerID(i int) string {
	return "c_ask" + string(rune('a'+i))
}

// generationOf reads a project's lease generation under the authority's lock.
func generationOf(t *testing.T, a *Authority, projectID string) uint64 {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.project[projectID]
	if state == nil {
		t.Fatalf("%s has no control state to read a generation from", projectID)
	}
	return state.generation
}

// waitUntil polls a condition, failing the test if it is still false at the
// deadline.
func waitUntil(t *testing.T, within time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never became true")
		}
		time.Sleep(time.Millisecond)
	}
}
