package terminal

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// These are the control plane's tests at the protocol boundary: what a browser
// sends, what it is sent back, and what the other browsers see.
//
// The three files divide the phase along the lines it is built on. authority.go
// is tested in authority_test.go, with no sockets at all, because the rules
// about who may type are rules about state rather than about messages. The
// transport's ordinary behaviour - output, framing, resynchronisation, the
// reaping of a client that stopped reading - is tested in hub_test.go. This file
// is the seam between them, and it is where the things that can only be wrong
// there are checked: the roster a subscribe is answered with, the message a
// viewer gets instead of a keystroke, what happens to everybody else when one
// client takes control or loses it, and whether any of it reaches a log.
//
// A recurring shape below is worth naming: almost every assertion is made about
// what a *second* client is told. A control plane that only tells the client
// that acted is one that works perfectly until somebody else opens the same
// project, and the tests that would catch that are exactly the ones that watch
// the other browser.

// subscribeAll puts every named client on a hub to watching one project and
// leaves all of them with nothing left to read.
//
// It exists because subscribing is not a private act: a client that starts
// watching is counted in everybody else's roster, so each subscribe sends a
// roster to the clients that were already there. A test that set its clients up
// one at a time would otherwise begin with a queue of rosters it did not ask for
// and has to know the count of. Draining here means the setup is over when this
// returns, and everything after it is the test's own.
func subscribeAll(t *testing.T, projectID string, clients ...*client) {
	t.Helper()
	for _, c := range clients {
		c.sendSubscribe(projectID, 0, 0)
	}
	// Every client's first frame is its snapshot, so reading one each proves the
	// subscribes have been acted on. Control messages are skipped on the way.
	for _, c := range clients {
		c.expectFrame()
	}
	for _, c := range clients {
		c.drainControl()
	}
}

// connection starts a client on a hub that already exists and consumes its
// greeting.
//
// It is `newClient` for a hub with more than one browser on it, which is every
// test where the interesting question is what somebody else was told.
func connection(t *testing.T, hub *Hub, rt *fakeRuntime, opts clientOptions) *client {
	t.Helper()
	c := attach(t, hub, rt, opts)
	c.greet()
	return c
}

// disconnect closes a client's socket and waits for the server to notice.
//
// Waiting is the point. Everything a disconnect causes - the lease being
// suspended, the roster being broadcast to the others - happens in the hub's
// teardown, and a test that closed the socket and asserted immediately would be
// asserting about a race it did not know it had entered.
func (c *client) disconnect() {
	c.t.Helper()
	_ = c.ws.Close()
	select {
	case <-c.served:
	case <-time.After(socketWait):
		c.t.Fatal("the connection did not end after the socket was closed")
	}
}

// drainControl discards every control message already queued for a client.
//
// It is how a test gets to a known state after a sequence of subscribes, each of
// which broadcasts to everybody who was already watching. It discards frames
// too, so it is only used by tests that are not about terminal output.
func (c *client) drainControl() {
	c.t.Helper()
	for {
		select {
		case <-c.ws.writes:
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Identity

func TestAConnectionIsGreetedWithTheIdentifierItClaimed(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	c := connection(t, hub, rt, clientOptions{
		clientID:  "c_0123456789abcdef",
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36",
	})

	if c.hello.Type != MsgHello {
		t.Fatalf("the first message was %q, want %q", c.hello.Type, MsgHello)
	}
	if c.hello.ClientID != "c_0123456789abcdef" {
		t.Errorf("the greeting names client %q, want the one the client claimed", c.hello.ClientID)
	}
	if c.hello.Device != "Chrome on Windows" {
		t.Errorf("the greeting reports device %q, want %q", c.hello.Device, "Chrome on Windows")
	}
	if c.hello.Protocol != ProtocolVersion {
		t.Errorf("the greeting states protocol %d, want %d", c.hello.Protocol, ProtocolVersion)
	}
}

func TestAClientThatClaimsNothingIsGivenAnIdentifier(t *testing.T) {
	// A client that is not a browser has no session to resume and no reason to
	// invent one. Refusing it would refuse every script and every test in
	// exchange for nothing, so it is given an identifier and told what it was
	// given - which is also what keeps the rest of the server to one rule.
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	if !validClientID(c.hello.ClientID) {
		t.Errorf("the server issued %q, which is not a valid client identifier", c.hello.ClientID)
	}
	if c.hello.ClientID != c.conn.clientID {
		t.Errorf("the greeting names %q but the connection is %q",
			c.hello.ClientID, c.conn.clientID)
	}
	if c.hello.Device != DeviceUnknown {
		t.Errorf("a client with no user agent was labelled %q, want %q",
			c.hello.Device, DeviceUnknown)
	}
}

func TestAClientThatClaimsAnUnusableIdentifierIsGivenOne(t *testing.T) {
	// The identifier is shape-checked rather than trusted, and an unusable one
	// is replaced rather than refused. It is handed back through greetings,
	// rosters, error messages and log lines, so a value that could be a path, a
	// sentence, or an escape sequence is one worth not carrying anywhere -
	// and the client that sent it is not left without a session for it.
	rt := newFakeRuntime().add(testProject, 80, 24)

	for name, claimed := range map[string]string{
		"a path":                   "/home/sunboy/projects",
		"a sentence":               "my laptop",
		"no prefix":                "0123456789abcdef",
		"uppercase":                "c_0123456789ABCDEF",
		"a separator":              "c_0123/4567",
		"nothing after the prefix": "c_",
		"very long":                "c_" + strings.Repeat("a", MaxClientIDLen),
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, rt, clientOptions{clientID: claimed})
			if c.hello.ClientID == claimed {
				t.Fatalf("the server accepted %q as a client identifier", claimed)
			}
			if !validClientID(c.hello.ClientID) {
				t.Errorf("the server issued %q, which is not a valid client identifier",
					c.hello.ClientID)
			}
		})
	}
}

func TestTheDeviceLabelIsDerivedFromTheUserAgent(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)

	// The labels are a closed vocabulary, and the order of the checks is what
	// makes them right: every Chromium browser claims to be Chrome and every
	// browser on macOS claims to be Safari, so a naive search reports Edge on
	// Windows as "Chrome Safari on macOS".
	for name, tc := range map[string]struct {
		userAgent string
		want      string
	}{
		"chrome on windows": {
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			"Chrome on Windows",
		},
		"safari on an ipad": {
			"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			"Safari on iPad",
		},
		"chrome on android": {
			"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			"Chrome on Android",
		},
		"edge claims to be chrome": {
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
			"Edge on Windows",
		},
		"nothing recognisable": {"curl/8.4.0", DeviceUnknown},
		"nothing at all":       {"", DeviceUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, rt, clientOptions{userAgent: tc.userAgent})
			if c.hello.Device != tc.want {
				t.Errorf("device = %q, want %q", c.hello.Device, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The roster

// TestASubscribeIsAnsweredWithTheRosterBeforeAnythingElse is the ordering the
// rest of the client is built on.
//
// A browser draws a terminal the moment it is told to, and a terminal drawn
// before the browser knows who owns it is a terminal that offers a keyboard it
// may not use. The roster therefore arrives first - before the first frame, and
// on a re-subscribe before the resynchronisation that follows it.
func TestASubscribeIsAnsweredWithTheRosterBeforeAnythingElse(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	rt.setScreen(testProject, "\x1b[2Jhello", 80, 24)
	c := newClient(t, rt, clientOptions{})

	c.sendSubscribe(testProject, 0, 0)

	first := c.decode(c.next())
	if first.Type != MsgControlChanged {
		t.Fatalf("the first message was %q, want %q", first.Type, MsgControlChanged)
	}
	if first.ProjectID != testProject {
		t.Errorf("the roster is for %q, want %q", first.ProjectID, testProject)
	}
	if first.Control.Controller != nil {
		t.Errorf("a project nobody has taken reports controller %+v, want none",
			first.Control.Controller)
	}
	if first.Control.Viewers != 0 {
		t.Errorf("a lone viewer is told %d others are watching, want 0", first.Control.Viewers)
	}

	// And then the terminal, which is what it subscribed for.
	if frame := c.expectFrame(); frame.Type != FrameSnapshot {
		t.Errorf("the first frame is type %#x, want a snapshot", frame.Type)
	}
}

// TestTheRosterNamesTheControllerAndCountsTheOthers is the viewer count, which
// is the one number in the roster that is not a fact about the project.
//
// It is built per recipient, so the assertion is made from three sides at once:
// the controller is not one of the people watching it, and nobody counts
// themselves.
func TestTheRosterNamesTheControllerAndCountsTheOthers(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop", userAgent: "Mozilla/5.0 (Windows NT 10.0) Chrome/120.0 Safari/537.36"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet", userAgent: "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Safari/604.1"})
	phone := connection(t, hub, rt, clientOptions{clientID: "c_phone1", userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Safari/604.1"})

	desktop.sendSubscribe(testProject, 0, 0)
	tablet.sendSubscribe(testProject, 0, 0)
	phone.sendSubscribe(testProject, 0, 0)

	// Each subscribe broadcasts to everybody already watching, so the clients
	// are at a known state only once the pile is cleared.
	desktop.drainControl()
	tablet.drainControl()
	phone.drainControl()

	desktop.requestControl(testProject)

	granted := desktop.expectMessageOfType(MsgControlGranted)
	if granted.Control.Controller == nil {
		t.Fatal("the grant carries a roster with no controller")
	}
	if got := granted.Control.Controller.ClientID; got != "c_desktop" {
		t.Errorf("the roster names controller %q, want this client", got)
	}
	if got := granted.Control.Controller.Device; got != "Chrome on Windows" {
		t.Errorf("the roster names the controller's device as %q, want %q",
			got, "Chrome on Windows")
	}
	// Who is the controller is not a field. A client compares the identifier in
	// the roster with its own, which is why the roster carries the identifier
	// rather than a boolean: a boolean would be different for every recipient
	// and would have to be rebuilt for each one anyway.
	if got := granted.Control.Viewers; got != 2 {
		t.Errorf("the controller is told %d others are watching, want 2", got)
	}
	desktop.expectMessageOfType(MsgControlChanged)

	for name, c := range map[string]*client{"the tablet": tablet, "the phone": phone} {
		msg := c.expectMessageOfType(MsgControlChanged)
		if msg.Control.Controller == nil || msg.Control.Controller.ClientID != "c_desktop" {
			t.Errorf("%s was told the controller is %+v, want c_desktop", name, msg.Control.Controller)
		}
		if msg.Control.Viewers != 1 {
			t.Errorf("%s is told %d others are watching, want 1", name, msg.Control.Viewers)
		}
	}
}

// ---------------------------------------------------------------------------
// Asking

func TestAViewerThatAsksForAFreeProjectIsGivenIt(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	one := connection(t, hub, rt, clientOptions{clientID: "c_aaaaaa"})
	two := connection(t, hub, rt, clientOptions{clientID: "c_bbbbbb"})

	subscribeAll(t, testProject, one, two)
	one.drainControl()

	one.requestControl(testProject)

	granted := one.expectMessageOfType(MsgControlGranted)
	if granted.Reason != ReasonAvailable {
		t.Errorf("the grant gave reason %q, want %q", granted.Reason, ReasonAvailable)
	}
	// The other browser is told, and told enough to stop offering its own
	// keyboard: the roster it receives names somebody else.
	changed := two.expectMessageOfType(MsgControlChanged)
	if changed.Control.Controller == nil || changed.Control.Controller.ClientID != "c_aaaaaa" {
		t.Errorf("the other viewer was told the controller is %+v", changed.Control.Controller)
	}
	if len(changed.Control.Pending) != 0 {
		t.Errorf("a grant left %d requests queued", len(changed.Control.Pending))
	}
}

// TestARequestAgainstATakenProjectIsQueuedInEverybodySRoster is the decision
// not to have a control.requested message.
//
// A queue is a change to the roster, and the roster is broadcast to everybody
// watching - including the controller, who is the one person who needs to know
// that somebody is asking. A second message saying the same thing would be a
// second source of truth for it.
func TestARequestAgainstATakenProjectIsQueuedInEverybodySRoster(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop", userAgent: "Mozilla/5.0 (Windows NT 10.0) Chrome/120.0 Safari/537.36"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet", userAgent: "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Safari/604.1"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	tablet.requestControl(testProject)

	// The asker is not told "granted", and is not told anything of its own: the
	// broadcast is the answer, and it says the asker is waiting.
	asking := tablet.expectMessageOfType(MsgControlChanged)
	if len(asking.Control.Pending) != 1 {
		t.Fatalf("the asker's roster lists %+v, want one waiting request", asking.Control.Pending)
	}
	if asking.Control.Pending[0].ClientID != "c_tablet" {
		t.Errorf("the waiting request names %q, want the client that asked",
			asking.Control.Pending[0].ClientID)
	}
	if asking.Control.Pending[0].Device != "Safari on iPad" {
		t.Errorf("the waiting request names device %q, want the asker's",
			asking.Control.Pending[0].Device)
	}

	// And the controller is told, which is the whole reason the queue is in the
	// roster rather than in a message of its own.
	held := desktop.expectMessageOfType(MsgControlChanged)
	if held.Control.Controller == nil || held.Control.Controller.ClientID != "c_desktop" {
		t.Errorf("the controller's own roster names %+v, want itself", held.Control.Controller)
	}
	if len(held.Control.Pending) != 1 || held.Control.Pending[0].ClientID != "c_tablet" {
		t.Errorf("the controller was told %+v is waiting, want the tablet", held.Control.Pending)
	}
}

func TestARequestIsRefusedWhenTheQueueIsFull(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	subscribeAll(t, testProject, desktop)
	desktop.becomeController(testProject)

	askers := make([]*client, 0, MaxPendingRequests+1)
	for i := 0; i < MaxPendingRequests+1; i++ {
		c := connection(t, hub, rt, clientOptions{clientID: askerID(i)})
		c.subscribe(testProject, 0, 0)
		c.expectFrame()
		c.drainControl()
		c.requestControl(testProject)
		askers = append(askers, c)
	}

	// Every asker but the last is queued, and the last is told so rather than
	// left waiting for an answer that will never come.
	last := askers[len(askers)-1]
	denied := last.expectMessageOfType(MsgControlDenied)
	if denied.Reason != ReasonTooManyRequests {
		t.Errorf("the refusal gave reason %q, want %q", denied.Reason, ReasonTooManyRequests)
	}
	if denied.Message == "" {
		t.Error("the refusal does not say what happened")
	}
}

func TestARequestWithoutASubscriptionIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	// Asking to control a terminal you are not looking at would put a name in
	// every roster for a client that cannot see what it is typing into.
	c.requestControl(testProject)

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeNotSubscribed {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotSubscribed)
	}
	if msg.About != MsgControlRequest {
		t.Errorf("the error is about %q, want %q", msg.About, MsgControlRequest)
	}
}

func TestARequestForSomethingThatIsNotAProjectIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	for name, projectID := range map[string]string{
		"a path":     "/home/sunboy/projects",
		"not an id":  "my-project",
		"nothing":    "",
		"another id": "c_0123456789abcdef",
	} {
		t.Run(name, func(t *testing.T) {
			c.send(map[string]any{"type": MsgControlRequest, "projectId": projectID})
			msg := c.expectMessageOfType(MsgError)
			if msg.Code != CodeBadProject {
				t.Errorf("code = %q, want %q", msg.Code, CodeBadProject)
			}
		})
	}
}

func TestARequestThatCarriesMoreThanAProjectIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()

	c.send(map[string]any{
		"type": MsgControlRequest, "projectId": testProject, "cols": 80, "rows": 24,
	})

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeBadMessage {
		t.Errorf("code = %q, want %q", msg.Code, CodeBadMessage)
	}
}

// ---------------------------------------------------------------------------
// What a viewer may do

// TestAViewersKeystrokeIsRefusedAndNeverReachesTheTerminal is §二十六 and the
// point of the phase.
//
// The assertion is made twice on purpose: the client is told, and the runtime
// never saw the bytes. A refusal that is only a message would be a refusal that
// depends on the client believing it.
func TestAViewersKeystrokeIsRefusedAndNeverReachesTheTerminal(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	tablet.input(testProject, []byte("rm -rf /\r"))

	msg := tablet.expectMessageOfType(MsgError)
	if msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	if msg.About != MsgInput {
		t.Errorf("the error is about %q, want %q", msg.About, MsgInput)
	}
	if msg.Message == "" {
		t.Error("the refusal does not say what to do instead")
	}
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Fatalf("a viewer's keystroke reached the runtime: %+v", inputs)
	}
}

func TestAViewersResizeIsRefusedAndNeverReachesTheTerminal(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	tablet.resize(testProject, 40, 100)

	// A viewer's resize is refused rather than ignored. A phone that has asked
	// for a shape and not been given it should be told, or it draws a terminal
	// that does not match the one the program is drawing for.
	msg := tablet.expectMessageOfType(MsgError)
	if msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	if msg.About != MsgResize {
		t.Errorf("the error is about %q, want %q", msg.About, MsgResize)
	}
	if resizes := rt.recordedResizes(); len(resizes) != 0 {
		t.Fatalf("a viewer's resize reached the runtime: %+v", resizes)
	}
	// And nobody was told the terminal changed shape, because it did not.
	desktop.expectSilence(50 * time.Millisecond)
}

func TestAViewersInputToAProjectItIsNotWatchingIsRefusedFirst(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	subscribeAll(t, testProject, desktop)
	desktop.becomeController(testProject)

	stranger := connection(t, hub, rt, clientOptions{clientID: "c_stranger"})
	stranger.input(testProject, []byte("who owns this"))

	// Not-subscribed is answered before not-controller, because it is the more
	// specific fact: the client is not even looking at the terminal.
	msg := stranger.expectMessageOfType(MsgError)
	if msg.Code != CodeNotSubscribed {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotSubscribed)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Fatalf("a stranger's keystroke reached the runtime: %+v", inputs)
	}
}

func TestTheControllerMayTypeAndResize(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	viewer := connection(t, hub, rt, clientOptions{clientID: "c_viewer"})

	subscribeAll(t, testProject, desktop, viewer)
	desktop.becomeController(testProject)
	viewer.expectMessageOfType(MsgControlChanged)

	desktop.input(testProject, []byte("ls\r"))
	desktop.waitForInputs(1)
	if got := string(rt.recordedInputs()[0].data); got != "ls\r" {
		t.Errorf("the runtime received %q, want %q", got, "ls\r")
	}

	desktop.resize(testProject, 120, 40)

	// Both the controller and the viewer are told the shape, because the shape
	// belongs to the terminal rather than to either of them.
	for name, c := range map[string]*client{"the controller": desktop, "the viewer": viewer} {
		msg := c.expectMessageOfType(MsgResized)
		if msg.Cols != 120 || msg.Rows != 40 {
			t.Errorf("%s was told %dx%d, want 120x40", name, msg.Cols, msg.Rows)
		}
	}
}

// ---------------------------------------------------------------------------
// Giving it up

func TestTheControllerCanGiveUpItsLease(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	viewer := connection(t, hub, rt, clientOptions{clientID: "c_viewer"})

	subscribeAll(t, testProject, desktop, viewer)
	desktop.becomeController(testProject)
	viewer.expectMessageOfType(MsgControlChanged)

	desktop.releaseControl(testProject)

	revoked := desktop.expectMessageOfType(MsgControlRevoked)
	if revoked.Reason != ReasonReleased {
		t.Errorf("the revocation gave reason %q, want %q", revoked.Reason, ReasonReleased)
	}
	if revoked.Control.Controller != nil {
		t.Errorf("the released roster still names %+v", revoked.Control.Controller)
	}
	// The release is broadcast as well as directed, because it is a change to
	// the project and the client that caused it is still watching the project.
	desktop.expectMessageOfType(MsgControlChanged)

	// The other browser is told, or it goes on drawing a keyboard it does not
	// have.
	changed := viewer.expectMessageOfType(MsgControlChanged)
	if changed.Control.Controller != nil {
		t.Errorf("the viewer was still told the controller is %+v", changed.Control.Controller)
	}

	// And the client that gave it up no longer has it.
	desktop.input(testProject, []byte("still there?"))
	msg := desktop.expectMessageOfType(MsgError)
	if msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Fatalf("a client that released its lease still typed into the terminal: %+v", inputs)
	}
}

func TestAViewerCannotGiveUpSomebodyElsesLease(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	viewer := connection(t, hub, rt, clientOptions{clientID: "c_viewer"})

	subscribeAll(t, testProject, desktop, viewer)
	desktop.becomeController(testProject)
	viewer.expectMessageOfType(MsgControlChanged)

	viewer.releaseControl(testProject)

	msg := viewer.expectMessageOfType(MsgError)
	if msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	// The controller still has it, and nothing was broadcast.
	desktop.expectSilence(50 * time.Millisecond)
	desktop.input(testProject, []byte("mine\r"))
	desktop.waitForInputs(1)
}

// TestAControllerThatStoppedWatchingCanStillGiveUpControl is the asymmetry
// between asking and releasing.
//
// Asking requires a subscription: you may only ask for a terminal you can see.
// Releasing deliberately does not, because a controller that paged away is no
// longer watching and still holds the lease - and letting go is the one thing it
// must be able to do from there.
func TestAControllerThatStoppedWatchingCanStillGiveUpControl(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	viewer := connection(t, hub, rt, clientOptions{clientID: "c_viewer"})

	subscribeAll(t, testProject, desktop, viewer)
	desktop.becomeController(testProject)
	viewer.expectMessageOfType(MsgControlChanged)

	// The panel is unmounted as the person pages away. The lease is not.
	desktop.unsubscribe(testProject)
	desktop.expectMessageOfType(MsgUnsubscribed)
	viewer.expectMessageOfType(MsgControlChanged)

	desktop.releaseControl(testProject)

	if msg := desktop.expectMessageOfType(MsgControlRevoked); msg.Reason != ReasonReleased {
		t.Errorf("the revocation gave reason %q, want %q", msg.Reason, ReasonReleased)
	}
	changed := viewer.expectMessageOfType(MsgControlChanged)
	if changed.Control.Controller != nil {
		t.Errorf("the project is still held by %+v after a release from nowhere",
			changed.Control.Controller)
	}
}

// TestUnsubscribingDoesNotCostAControllerItsLease is the other half of that
// decision, and the reason it is a decision rather than an oversight.
//
// In the workspace a panel is unmounted whenever its page is not the current
// one. If unmounting released the lease, paging away would be a way to lose the
// keyboard.
func TestUnsubscribingDoesNotCostAControllerItsLease(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	subscribeAll(t, testProject, desktop)
	desktop.becomeController(testProject)

	desktop.unsubscribe(testProject)
	desktop.expectMessageOfType(MsgUnsubscribed)

	// It watches again, and is still in charge - and, because the lease is not
	// something it has to ask for a second time, it can type straight away.
	subscribeAll(t, testProject, desktop)
	desktop.input(testProject, []byte("still mine\r"))
	desktop.waitForInputs(1)
}

// ---------------------------------------------------------------------------
// Handing it over: the four ways a transfer ends

func TestTheControllerHandsControlToTheClientThatAsked(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop", userAgent: "Mozilla/5.0 (Windows NT 10.0) Chrome/120.0 Safari/537.36"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet", userAgent: "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Safari/604.1"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	tablet.requestControl(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	desktop.expectMessageOfType(MsgControlChanged)

	desktop.transferControl(testProject, tablet.conn.clientID, true)

	// The new controller is told first, with a roster that already names it.
	granted := tablet.expectMessageOfType(MsgControlGranted)
	if granted.Reason != ReasonTransferred {
		t.Errorf("the grant gave reason %q, want %q", granted.Reason, ReasonTransferred)
	}
	if granted.Control.Controller == nil || granted.Control.Controller.ClientID != "c_tablet" {
		t.Errorf("the new controller's roster names %+v", granted.Control.Controller)
	}
	if len(granted.Control.Pending) != 0 {
		t.Errorf("the transfer left %+v queued", granted.Control.Pending)
	}

	// And then the broadcast, which is what tells the old controller.
	changed := desktop.expectMessageOfType(MsgControlChanged)
	if changed.Control.Controller == nil || changed.Control.Controller.ClientID != "c_tablet" {
		t.Errorf("the old controller was told the project is held by %+v",
			changed.Control.Controller)
	}

	// The keyboard moved with it.
	tablet.input(testProject, []byte("hello\r"))
	tablet.waitForInputs(1)
	desktop.input(testProject, []byte("no longer mine\r"))
	if msg := desktop.expectMessageOfType(MsgError); msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 1 {
		t.Fatalf("the runtime received %d inputs, want only the new controller's", len(inputs))
	}
}

func TestTheControllerCanDeclineARequestAndKeepControl(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	tablet.requestControl(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	desktop.expectMessageOfType(MsgControlChanged)

	desktop.transferControl(testProject, tablet.conn.clientID, false)

	denied := tablet.expectMessageOfType(MsgControlDenied)
	if denied.Reason != ReasonRejected {
		t.Errorf("the refusal gave reason %q, want %q", denied.Reason, ReasonRejected)
	}
	if len(denied.Control.Pending) != 0 {
		t.Errorf("the refused request is still queued: %+v", denied.Control.Pending)
	}
	if denied.Control.Controller == nil || denied.Control.Controller.ClientID != "c_desktop" {
		t.Errorf("the refusal names controller %+v, want the desktop to still hold it",
			denied.Control.Controller)
	}

	// Declining does not cost the controller anything, and the asker does not
	// get control by asking again with a different question.
	if msg := desktop.expectMessageOfType(MsgControlChanged); msg.Control.Controller == nil {
		t.Error("the controller was told it no longer holds the project")
	}
	desktop.input(testProject, []byte("still mine\r"))
	desktop.waitForInputs(1)
}

func TestATransferToAClientThatDidNotAskIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	stranger := connection(t, hub, rt, clientOptions{clientID: "c_strange"})

	subscribeAll(t, testProject, desktop, stranger)
	desktop.becomeController(testProject)
	stranger.expectMessageOfType(MsgControlChanged)

	// Nobody may be handed a keyboard they did not ask for: a client pushed into
	// being the controller is one whose next keystroke goes somewhere it did not
	// expect.
	desktop.transferControl(testProject, stranger.conn.clientID, true)

	msg := desktop.expectMessageOfType(MsgError)
	if msg.Code != CodeNotPending {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotPending)
	}
	stranger.expectSilence(50 * time.Millisecond)
}

func TestATransferFromAClientThatIsNotTheControllerIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})
	phone := connection(t, hub, rt, clientOptions{clientID: "c_phone1"})

	subscribeAll(t, testProject, desktop, tablet, phone)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	phone.expectMessageOfType(MsgControlChanged)

	tablet.requestControl(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	desktop.expectMessageOfType(MsgControlChanged)
	phone.expectMessageOfType(MsgControlChanged)

	// A queued client cannot promote itself, and cannot play the controller by
	// handing the project to somebody else.
	tablet.transferControl(testProject, phone.conn.clientID, true)

	msg := tablet.expectMessageOfType(MsgError)
	if msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	phone.expectSilence(50 * time.Millisecond)
	desktop.input(testProject, []byte("nobody moved me\r"))
	desktop.waitForInputs(1)
}

func TestATransferToYourselfIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{clientID: "c_desktop"})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()
	c.becomeController(testProject)

	// A client that sends its own identifier has confused a client id with a
	// connection id, and the honest answer is that the message is malformed -
	// not that the named client has not asked, which would be a lie.
	c.transferControl(testProject, "c_desktop", true)

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeBadMessage {
		t.Errorf("code = %q, want %q", msg.Code, CodeBadMessage)
	}
	if !c.holdsControl(testProject) {
		t.Error("a refused transfer cost the controller its lease")
	}
}

func TestATransferMustNameAClient(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{clientID: "c_desktop"})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()
	c.becomeController(testProject)

	for name, clientID := range map[string]string{
		"nothing":      "",
		"not an id":    "the tablet",
		"another kind": "p_0123456789abcdef0123",
	} {
		t.Run(name, func(t *testing.T) {
			c.send(map[string]any{
				"type": MsgControlAccept, "projectId": testProject, "clientId": clientID,
			})
			msg := c.expectMessageOfType(MsgError)
			if msg.Code != CodeBadMessage {
				t.Errorf("code = %q, want %q", msg.Code, CodeBadMessage)
			}
		})
	}
}

func TestATransferThatCarriesAPayloadIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{clientID: "c_desktop"})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()
	c.becomeController(testProject)

	// A transfer is a statement about who types next. It has no business
	// carrying bytes, and a message that could would be a second input path -
	// which is exactly the thing the authority exists to be the only one of.
	c.send(map[string]any{
		"type": MsgControlAccept, "projectId": testProject,
		"clientId": "c_bbbbbb", "data": "aGVsbG8=",
	})

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeBadMessage {
		t.Errorf("code = %q, want %q", msg.Code, CodeBadMessage)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Fatalf("a transfer carrying input reached the runtime: %+v", inputs)
	}
}

func TestWithdrawingARequestRemovesItFromEverybodySRoster(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	tablet.requestControl(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	desktop.expectMessageOfType(MsgControlChanged)

	// The tablet stops watching, which is what a person closing the tab or
	// navigating to another project does. A request left behind would put a name
	// in every roster for a client that has gone.
	tablet.unsubscribe(testProject)
	tablet.expectMessageOfType(MsgUnsubscribed)

	held := desktop.expectMessageOfType(MsgControlChanged)
	if len(held.Control.Pending) != 0 {
		t.Errorf("the withdrawn request is still queued: %+v", held.Control.Pending)
	}
	// And the controller can no longer hand anything to it.
	desktop.transferControl(testProject, tablet.conn.clientID, true)
	if msg := desktop.expectMessageOfType(MsgError); msg.Code != CodeNotPending {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotPending)
	}
}

// ---------------------------------------------------------------------------
// Disconnecting

// TestAControllersDisconnectIsVisibleToEverybodyElse is the first half of
// recovery, and the half that is easy to forget.
//
// The lease is not released when the controller's socket closes - it is held for
// them, because a disconnect is usually a reconnect that has not happened yet.
// But it is *suspended*, and the difference has to reach the other browsers: a
// tablet left showing "Controller: MacBook" while its owner's laptop is shut is
// a tablet whose owner cannot tell whether the terminal is free.
func TestAControllersDisconnectIsVisibleToEverybodyElse(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{controlGrace: time.Minute})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop", userAgent: "Mozilla/5.0 (Windows NT 10.0) Chrome/120.0 Safari/537.36"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet", userAgent: "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Safari/604.1"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	desktop.disconnect()

	changed := tablet.expectMessageOfType(MsgControlChanged)
	if !changed.Control.Suspended {
		t.Error("the lease is not reported as suspended while its controller is away")
	}
	if changed.Control.Controller == nil || changed.Control.Controller.ClientID != "c_desktop" {
		t.Errorf("the roster names %+v, want the absent controller still named",
			changed.Control.Controller)
	}
	if changed.Control.ExpiresAt == "" {
		t.Error("a suspended lease does not say when it lapses")
	}
	if changed.Control.Viewers != 0 {
		t.Errorf("the tablet is told %d others are watching, want 0", changed.Control.Viewers)
	}

	// And the project is not free. A grace period that anybody could walk into
	// by asking would be a grace period that means nothing.
	tablet.requestControl(testProject)
	denied := tablet.expectMessageOfType(MsgControlDenied)
	if denied.Reason != ReasonControllerExists {
		t.Errorf("the refusal gave reason %q, want %q", denied.Reason, ReasonControllerExists)
	}
	if denied.Message == "" {
		t.Error("the refusal does not say what happened")
	}
	if denied.Control.Controller == nil {
		t.Error("a refusal during the grace period reports the project as free")
	}
	if !denied.Control.Suspended {
		t.Error("a refusal during the grace period reports the lease as live")
	}
}

func TestTheControllerReconnectingResumesItsLease(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{controlGrace: time.Minute})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	desktop.disconnect()
	tablet.expectMessageOfType(MsgControlChanged)

	// The same browser session comes back - a reload, a laptop reopened, a phone
	// that changed networks. The identifier it kept in session storage is what
	// makes this a reconnect rather than a new device.
	again := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})

	resumed := tablet.expectMessageOfType(MsgControlChanged)
	if resumed.Control.Suspended {
		t.Error("the lease is still reported as suspended after its client came back")
	}
	if resumed.Control.ExpiresAt != "" {
		t.Errorf("a live lease still carries an expiry of %q", resumed.Control.ExpiresAt)
	}

	// It does not have to ask again - which is the whole point, because a reload
	// that cost control would make the feature unusable on a phone whose screen
	// locks.
	again.subscribe(testProject, 0, 0)
	again.expectFrame()
	again.input(testProject, []byte("still mine\r"))
	again.waitForInputs(1)
}

func TestALeaseThatLapsesIsReleasedForEverybody(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	// A grace period short enough to wait out, because what is being tested is
	// that the timer fires and that everybody hears about it.
	hub := newHub(t, rt, clientOptions{controlGrace: 40 * time.Millisecond})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	desktop.disconnect()
	suspended := tablet.expectMessageOfType(MsgControlChanged)
	if !suspended.Control.Suspended {
		t.Fatal("the lease was not suspended when its controller disconnected")
	}

	// The grace period ends. This is its own message rather than a roster
	// change, because it is the one event in the family that nobody caused.
	expired := tablet.expectMessageOfType(MsgControlExpired)
	if expired.Control.Controller != nil {
		t.Errorf("the project is still held by %+v after the grace period", expired.Control.Controller)
	}

	// And now it is free, which is what the grace period ending means: somebody
	// can have the terminal without going to find a person.
	tablet.requestControl(testProject)
	if granted := tablet.expectMessageOfType(MsgControlGranted); granted.Reason != ReasonAvailable {
		t.Errorf("the grant gave reason %q, want %q", granted.Reason, ReasonAvailable)
	}
}

func TestAViewersDisconnectChangesNothingAboutControl(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{controlGrace: time.Minute})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	tablet.disconnect()

	// A viewer held nothing, so nothing is suspended - but the controller is
	// told all the same, because the number of people watching it changed.
	changed := desktop.expectMessageOfType(MsgControlChanged)
	if changed.Control.Suspended {
		t.Error("a viewer's disconnect suspended the controller's lease")
	}
	if changed.Control.Controller == nil || changed.Control.Controller.ClientID != "c_desktop" {
		t.Errorf("the controller's own roster names %+v", changed.Control.Controller)
	}
	if changed.Control.Viewers != 0 {
		t.Errorf("the controller is told %d others are watching, want 0", changed.Control.Viewers)
	}

	desktop.input(testProject, []byte("unaffected\r"))
	desktop.waitForInputs(1)
}

func TestAViewersReconnectLeavesTheLeaseAlone(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{controlGrace: time.Minute})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})

	subscribeAll(t, testProject, desktop, tablet)
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)

	// The tablet's screen locks, or it changes networks, and the socket goes -
	// and then the same browser session comes back. Neither half may cost the
	// desktop the keyboard it is holding.
	tablet.disconnect()
	gone := desktop.expectMessageOfType(MsgControlChanged)
	if gone.Control.Suspended {
		t.Error("a viewer disconnecting suspended the controller's lease")
	}
	if gone.Control.Viewers != 0 {
		t.Errorf("the controller is told %d others are watching, want 0", gone.Control.Viewers)
	}

	again := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})
	again.subscribe(testProject, 0, 0)
	again.expectFrame()

	back := desktop.expectMessageOfType(MsgControlChanged)
	if back.Control.Suspended {
		t.Error("a viewer reconnecting suspended the controller's lease")
	}
	if back.Control.Viewers != 1 {
		t.Errorf("the controller is told %d others are watching, want 1", back.Control.Viewers)
	}
	if back.Control.Controller == nil || back.Control.Controller.ClientID != "c_desktop" {
		t.Errorf("the roster names %+v, want the desktop still holding it",
			back.Control.Controller)
	}

	// And the keyboard is still where it was.
	desktop.input(testProject, []byte("still mine\r"))
	desktop.waitForInputs(1)
	again.input(testProject, []byte("not mine\r"))
	if msg := again.expectMessageOfType(MsgError); msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 1 {
		t.Errorf("the runtime received %d inputs, want only the controller's", len(inputs))
	}
}

// ---------------------------------------------------------------------------
// Isolation

// TestControlOfOneProjectIsNotControlOfAnother is §三十四's isolation at the
// protocol boundary.
//
// A lease is per project. Holding one must not make a client the controller of
// anything else it is watching, and the roster of one project must never name
// the controller of another.
func TestControlOfOneProjectIsNotControlOfAnother(t *testing.T) {
	const otherProject = "p_ffffffffffffffffffff"
	rt := newFakeRuntime().add(testProject, 80, 24).add(otherProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})

	subscribeAll(t, testProject, desktop)
	desktop.becomeController(testProject)

	// The roster for the second project is the one the subscribe is answered
	// with, and it is the first thing to arrive about it: nothing has happened
	// to this project, so there is no other roster it could be.
	desktop.sendSubscribe(otherProject, 0, 0)
	other := desktop.expectMessageOfType(MsgControlChanged)
	if other.ProjectID != otherProject {
		t.Fatalf("the roster is for %q, want %q", other.ProjectID, otherProject)
	}
	if other.Control.Controller != nil {
		t.Errorf("taking one project made this client the controller of another: %+v",
			other.Control.Controller)
	}
	if other.Control.Viewers != 0 {
		t.Errorf("the second project reports %d viewers, want 0", other.Control.Viewers)
	}
	desktop.expectFrame()

	desktop.input(otherProject, []byte("wrong terminal\r"))
	if msg := desktop.expectMessageOfType(MsgError); msg.Code != CodeNotController {
		t.Errorf("code = %q, want %q", msg.Code, CodeNotController)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Fatalf("control of one project allowed typing into another: %+v", inputs)
	}
}

// ---------------------------------------------------------------------------
// Logging

// logSink collects what the server logged, so that a test can assert about it.
//
// It is guarded because slog handlers write from whichever goroutine produced
// the record while the test reads from its own.
type logSink struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines.Write(p)
}

func (s *logSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines.String()
}

// TestNeitherTypingNorOutputIsEverLogged is §三十四, which is the constraint
// this phase could most easily break without noticing.
//
// A control plane is a machine for deciding who is allowed to send bytes to a
// terminal, and the obvious way to debug one is to log the decision - including
// what was decided about. Refused input is the sharpest case: it is text a
// person typed, it is text they typed into something that told them not to, and
// it is exactly the text that would be sitting in a log the next morning.
//
// So this test types a marker into a terminal, has the terminal print a
// different marker, refuses a third marker, and asserts that none of the three
// is anywhere in what the server wrote.
func TestNeitherTypingNorOutputIsEverLogged(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	sink := &logSink{}
	hub, err := NewHub(rt, HubOptions{
		Logger:  slog.New(slog.NewTextHandler(sink, nil)),
		Timings: fastTimings(),
	})
	if err != nil {
		t.Fatalf("terminal.NewHub returned an error: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })

	const (
		typed   = "MARKER-TYPED-BY-THE-CONTROLLER"
		refused = "MARKER-TYPED-BY-A-VIEWER"
		output  = "MARKER-PRINTED-BY-THE-TERMINAL"
	)

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	viewer := connection(t, hub, rt, clientOptions{clientID: "c_viewer"})

	subscribeAll(t, testProject, desktop, viewer)
	desktop.becomeController(testProject)
	viewer.expectMessageOfType(MsgControlChanged)

	// Typed by the controller: accepted, and forwarded.
	desktop.input(testProject, []byte(typed))
	desktop.waitForInputs(1)

	// Typed by a viewer: refused at the authority, which is the path that has
	// the bytes in hand and a reason to mention them.
	viewer.input(testProject, []byte(refused))
	viewer.expectMessageOfType(MsgError)

	// Printed by the terminal: framed out to both of them.
	rt.publish(testProject, output)
	desktop.expectFrame()
	viewer.expectFrame()

	logged := sink.text()
	if logged == "" {
		t.Fatal("the server logged nothing at all, so this test proves nothing")
	}
	for name, secret := range map[string]string{
		"what the controller typed": typed,
		"what a viewer typed":       refused,
		"what the terminal printed": output,
	} {
		if strings.Contains(logged, secret) {
			t.Errorf("%s is in the log:\n%s", name, logged)
		}
	}

	// What is logged is the shape §三十四 allows: the identifier, the project,
	// and what happened.
	if !strings.Contains(logged, "c_desktop") {
		t.Errorf("the log does not name the client that connected:\n%s", logged)
	}
	if !strings.Contains(logged, testProject) {
		t.Errorf("the log does not name the project that was controlled:\n%s", logged)
	}
}

// logFields is what a fieldSink records, shared by every handler WithAttrs
// derives from it.
type logFields struct {
	mu     sync.Mutex
	names  []string
	events []string
}

// fieldSink is a slog.Handler that records the name of every attribute it is
// given, base attributes included, and the message of every record.
//
// A text dump would have to be parsed back into fields, and a test that parses
// its own output is testing the parser. This reads the attributes themselves,
// which is what the constraint is about.
//
// The three things the standard library puts on every record - the time, the
// level and the message - are not attributes and do not appear among the names.
// The message is the event, and it is the one field §三十四 names that a handler
// cannot vary, so it is recorded separately and asserted against the list of
// events this test means to have caused.
type fieldSink struct {
	shared *logFields
	base   []slog.Attr
}

func (s *fieldSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *fieldSink) Handle(_ context.Context, r slog.Record) error {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	for _, a := range s.base {
		s.shared.names = append(s.shared.names, a.Key)
	}
	r.Attrs(func(a slog.Attr) bool {
		s.shared.names = append(s.shared.names, a.Key)
		return true
	})
	s.shared.events = append(s.shared.events, r.Message)
	return nil
}

func (s *fieldSink) WithAttrs(attrs []slog.Attr) slog.Handler {
	base := make([]slog.Attr, 0, len(s.base)+len(attrs))
	base = append(base, s.base...)
	base = append(base, attrs...)
	return &fieldSink{shared: s.shared, base: base}
}

func (s *fieldSink) WithGroup(string) slog.Handler { return s }

func (s *fieldSink) recorded() (names, events []string) {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	return append([]string(nil), s.shared.names...), append([]string(nil), s.shared.events...)
}

// TestALogRecordCarriesOnlyTheFourFields is the other half of §三十四.
//
// The test above proves that nothing a person typed and nothing a terminal
// printed reaches a log. This one proves the shape of what does: every record
// the control plane writes carries the client and the project, and nothing
// else. It is the assertion that fails the day somebody adds a helpful count, a
// duration or an error string to a line that was already telling the story -
// which is how the four-field rule is broken in practice, not by deciding to
// log a keystroke.
//
// The paths it walks are the ones this phase added - a grant, a queue, a
// refusal, a declined handover, an accepted one, a release, a suspension, a
// resume and a lapse - plus the subscribe, unsubscribe and disconnect paths
// they sit on. It asserts that each of those events was actually logged, so a
// path that quietly stopped logging fails here rather than passing by absence.
//
// The two Debug records a broken socket produces are not walked: they need a
// socket that fails mid-write, and a test that manufactured one would be
// asserting about the double.
func TestALogRecordCarriesOnlyTheFourFields(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	sink := &fieldSink{shared: &logFields{}}
	hub, err := NewHub(rt, HubOptions{
		Logger:  slog.New(sink),
		Timings: fastTimings(),
		// Long enough to reconnect inside it, short enough to wait out.
		ControlGrace: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("terminal.NewHub returned an error: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })

	desktop := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	tablet := connection(t, hub, rt, clientOptions{clientID: "c_tablet"})
	subscribeAll(t, testProject, desktop, tablet)

	// Granted, then queued: the two answers a request can have.
	desktop.becomeController(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	tablet.requestControl(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	desktop.drainControl()

	// A viewer's keystroke, refused at the authority - the path that has a
	// person's bytes in hand and a reason to mention them.
	tablet.input(testProject, []byte("MARKER-NOT-MINE-TO-TYPE"))
	tablet.expectMessageOfType(MsgError)

	// Declined, which is the path that names the request it is turning down.
	desktop.transferControl(testProject, "c_tablet", false)
	tablet.expectMessageOfType(MsgControlDenied)
	tablet.drainControl()
	desktop.drainControl()

	// Asked again, and handed over.
	tablet.requestControl(testProject)
	tablet.expectMessageOfType(MsgControlChanged)
	desktop.drainControl()
	desktop.transferControl(testProject, "c_tablet", true)
	tablet.expectMessageOfType(MsgControlGranted)
	tablet.drainControl()
	desktop.drainControl()

	// Given up deliberately.
	tablet.releaseControl(testProject)
	tablet.drainControl()
	desktop.drainControl()

	// Taken again, then lost to a disconnect rather than to a person.
	desktop.becomeController(testProject)
	tablet.drainControl()
	desktop.disconnect()
	tablet.expectMessageOfType(MsgControlChanged)

	// The same client comes back inside the grace period, which is the lease
	// being resumed rather than renegotiated. The tablet is told, because the
	// roster it is holding says the controller is away.
	back := connection(t, hub, rt, clientOptions{clientID: "c_desktop"})
	if resumed := tablet.expectMessageOfType(MsgControlChanged); resumed.Control.Suspended {
		t.Fatal("the lease was still suspended after its client reconnected")
	}
	subscribeAll(t, testProject, back)
	// The tablet is told about the new audience too, and this test is not about
	// how many rosters that costs.
	tablet.drainControl()

	// And this time nobody comes back, so the grace period ends.
	back.disconnect()
	if gone := tablet.expectMessageOfType(MsgControlChanged); !gone.Control.Suspended {
		t.Fatal("the lease was not suspended when its client dropped the second time")
	}
	if expired := tablet.expectMessageOfType(MsgControlExpired); expired.Control.Controller != nil {
		t.Fatalf("the lease did not lapse, so the expiry path was never walked")
	}

	// Somebody takes the free project, and stops watching it.
	tablet.becomeController(testProject)
	tablet.unsubscribe(testProject)
	tablet.expectMessageOfType(MsgUnsubscribed)

	names, events := sink.recorded()
	if len(names) == 0 {
		t.Fatal("the server logged no attributes at all, so this test proves nothing")
	}
	for _, name := range names {
		if name != "clientId" && name != "projectId" {
			t.Errorf("a log record carries %q; §三十四 allows the client, the project, "+
				"the event and the time, and nothing else", name)
		}
	}

	seen := make(map[string]bool, len(events))
	for _, event := range events {
		seen[event] = true
	}
	for _, want := range []string{
		"terminal client connected",
		"terminal subscribed",
		"terminal unsubscribed",
		"terminal client disconnected",
		"input refused from a viewer",
		"control requested",
		"control granted",
		"control request refused",
		"control request declined",
		"control transferred",
		"control handed over",
		"control released",
		"control suspended while the controller is away",
		"control resumed after reconnect",
		"control expired",
	} {
		if !seen[want] {
			t.Errorf("%q was never logged, so whatever it carries went unchecked", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Small helpers this file's assertions are made of

// holdsControl reports whether a client still holds a project's lease,
// according to the server rather than according to the client.
func (c *client) holdsControl(projectID string) bool {
	c.t.Helper()
	return c.hub.authority.MayInput(projectID, c.conn.clientID).Allowed
}
