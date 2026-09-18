package terminal

import (
	"bytes"
	"testing"
	"time"
)

// The tests for the things Phase 6 claims that are not about one message or one
// rule: how many devices a project can carry at once, how long a lease survives
// its holder being away, and what a server that has just started knows about any
// of it.
//
// They live apart from control_test.go because they are about the shape of the
// whole thing rather than about a seam in it. The scale test below is §三十五 of
// the phase directive and the restart test is §三十三; both were written as
// acceptance criteria rather than as bug reports, which is why what they assert
// is a property rather than an example.

// TestTheGracePeriodDefaultsToThirtySeconds pins the one number in this phase
// that is a judgement rather than a rule.
//
// §八 of the directive gives thirty seconds as an example - "例如：30秒" - and the
// point of testing it is not that thirty is magic. It is that this value is what
// stands between a person whose train went into a tunnel and the loss of their
// terminal to whoever asks first, and a default that drifted to nothing would
// take that away without failing any other test. Thirty is what the product
// documentation says, so thirty is what it has to be.
func TestTheGracePeriodDefaultsToThirtySeconds(t *testing.T) {
	if DefaultControlGrace != 30*time.Second {
		t.Fatalf("DefaultControlGrace = %s, want 30s: it is what docs/MULTI_DEVICE.md "+
			"promises a person whose connection blinks", DefaultControlGrace)
	}

	rt := newFakeRuntime().add(testProject, 80, 24)

	// The constant is only half of it. A hub that was never told a grace has to
	// end up with that one, and a deployment that stated its own has to keep it.
	// Both are read off the authority rather than off the option, because the
	// option is what was passed in and the authority is what will be used.
	byDefault := newHub(t, rt, clientOptions{})
	if byDefault.authority.grace != DefaultControlGrace {
		t.Errorf("a hub built with no grace holds a lease for %s, want %s",
			byDefault.authority.grace, DefaultControlGrace)
	}

	stated := newHub(t, rt, clientOptions{controlGrace: 3 * time.Second})
	if stated.authority.grace != 3*time.Second {
		t.Errorf("a hub built with a three-second grace holds a lease for %s, want 3s",
			stated.authority.grace)
	}
}

// TestOneControllerAndTenViewersShareOneTerminal is §三十五: one project, one
// controller, ten people watching.
//
// # What is asserted, and what is not
//
// The directive asks for websocket, memory and latency to be observed at this
// size. Latency and memory are not asserted here, and the reason is that a fake
// runtime cannot answer them: whatever this test measured would be the speed of
// a channel send against the speed of the machine it ran on. Those numbers come
// from the browser suite and from a person watching a real deployment.
//
// What is asserted is the shape of the fan-out, which is where a control plane
// actually breaks at this size: every watcher is sent the output, only one
// device in eleven may write, a refusal goes to the device that earned it rather
// than to all of them, and the geometry belongs to the terminal rather than to
// the eleven browsers that each have an opinion about it.
func TestOneControllerAndTenViewersShareOneTerminal(t *testing.T) {
	const viewers = 10

	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})

	// One controller and ten viewers, each a different client - a phone, a
	// tablet, a laptop, and seven more tabs. The identifier is what makes them
	// distinct; the device label is what the others are shown.
	desktop := connection(t, hub, rt, clientOptions{
		clientID:  "c_controller",
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 Safari/537.36",
	})
	watching := make([]*client, 0, viewers)
	for i := 0; i < viewers; i++ {
		watching = append(watching, connection(t, hub, rt, clientOptions{
			clientID:  "c_viewer" + string(rune('a'+i)),
			userAgent: "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Safari/604.1",
		}))
	}

	subscribeAll(t, testProject, append([]*client{desktop}, watching...)...)

	// Asked for by hand rather than through the helper, because the count the
	// controller is given is one of the things being asserted here and the
	// helper consumes the message that carries it.
	desktop.requestControl(testProject)
	granted := desktop.expectMessageOfType(MsgControlGranted)
	if granted.Control.Controller == nil || granted.Control.Controller.ClientID != "c_controller" {
		t.Fatalf("the grant names controller %+v, want the device that asked",
			granted.Control.Controller)
	}
	// The controller counts all ten; each viewer counts the other nine.
	if granted.Control.Viewers != viewers {
		t.Errorf("the controller is told %d others are watching, want %d",
			granted.Control.Viewers, viewers)
	}
	desktop.expectMessageOfType(MsgControlChanged)

	for _, c := range watching {
		roster := c.expectMessageOfType(MsgControlChanged)
		if roster.Control.Controller == nil || roster.Control.Controller.ClientID != "c_controller" {
			t.Fatalf("a viewer was told the controller is %+v, want c_controller",
				roster.Control.Controller)
		}
		// Ten viewers and one controller: each viewer counts the other nine.
		if roster.Control.Viewers != viewers-1 {
			t.Errorf("a viewer is told %d others are watching, want %d",
				roster.Control.Viewers, viewers-1)
		}
	}

	// Output reaches everybody, which is the whole point of watching.
	rt.publish(testProject, "OUTPUT-FOR-ELEVEN")
	for i, c := range append([]*client{desktop}, watching...) {
		frame := c.expectFrame()
		if !bytes.Contains(frame.Payload, []byte("OUTPUT-FOR-ELEVEN")) {
			t.Fatalf("device %d was sent %q, want the output everybody else got", i, frame.Payload)
		}
	}

	// One device in eleven may write. The ten are refused, and each refusal is
	// addressed to the device that earned it rather than broadcast - otherwise
	// ten people typing would produce a hundred error messages.
	desktop.input(testProject, []byte("whoami\r"))
	for i, c := range watching {
		c.input(testProject, []byte("rm -rf /\r"))
		msg := c.expectMessageOfType(MsgError)
		if msg.Code != CodeNotController || msg.About != MsgInput {
			t.Fatalf("viewer %d was refused with %q about %q, want %q about %q",
				i, msg.Code, msg.About, CodeNotController, MsgInput)
		}
		// The refusal went to one socket. Any other device receiving one here
		// would be the fan-out that turns eleven devices into a hundred and
		// twenty-one conversations.
		if i == 0 {
			watching[1].expectSilence(50 * time.Millisecond)
		}
	}
	inputs := rt.recordedInputs()
	if len(inputs) != 1 {
		t.Fatalf("the runtime received %d inputs, want only the controller's one: %+v",
			len(inputs), inputs)
	}
	if string(inputs[0].data) != "whoami\r" {
		t.Errorf("the runtime received %q, want the controller's keystrokes", inputs[0].data)
	}

	// The size belongs to the terminal. The controller sets it; everybody is
	// told, because a viewer drawing its own idea of the size would draw a
	// terminal that does not match the one the program is drawing for.
	desktop.resize(testProject, 120, 40)
	for i, c := range append([]*client{desktop}, watching...) {
		msg := c.expectMessageOfType(MsgResized)
		if msg.Cols != 120 || msg.Rows != 40 {
			t.Fatalf("device %d was told the terminal is %dx%d, want 120x40",
				i, msg.Cols, msg.Rows)
		}
	}
	watching[0].resize(testProject, 40, 100)
	if msg := watching[0].expectMessageOfType(MsgError); msg.About != MsgResize {
		t.Errorf("a viewer's resize was refused about %q, want %q", msg.About, MsgResize)
	}
	if resizes := rt.recordedResizes(); len(resizes) != 1 {
		t.Errorf("the runtime was resized %d times, want only the controller's one: %+v",
			len(resizes), resizes)
	}
}

// TestALeaseDoesNotOutliveTheServerThatGrantedIt is §三十三.
//
// A lease is a fact about a running process and is deliberately not written
// anywhere - §十九 of the directive asks for that, and the reason is that the
// alternative is a database that has to be reconciled against reality every time
// the server starts. So a server that restarts knows nothing, and the thing that
// has to be true is what it does with that ignorance: it must not hand the
// terminal to whoever happened to be watching when it came up.
//
// The runtime is the other half. The session, the program inside it and the
// screen it has painted all outlive the restart, and none of that is affected by
// a lease having been forgotten.
func TestALeaseDoesNotOutliveTheServerThatGrantedIt(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	rt.publish(testProject, "WORK-IN-PROGRESS")

	// The server before the restart.
	before := newHub(t, rt, clientOptions{})
	oldDesktop := connection(t, before, rt, clientOptions{clientID: "c_desktop"})
	oldTablet := connection(t, before, rt, clientOptions{clientID: "c_tablet"})
	subscribeAll(t, testProject, oldDesktop, oldTablet)
	oldDesktop.becomeController(testProject)
	oldTablet.expectMessageOfType(MsgControlChanged)
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Fatalf("setting the test up typed something into the terminal: %+v", inputs)
	}

	// The restart. Closing the hub ends every socket and stops every grace
	// timer, which is what a process going away does.
	if err := before.Close(); err != nil {
		t.Fatalf("closing the first hub: %v", err)
	}

	// The server after it. Same runtime, same sockets directory, same projects -
	// and no memory of who was typing.
	after := newHub(t, rt, clientOptions{})

	// The same two devices come back, which is what a browser does on its own.
	desktop := connection(t, after, rt, clientOptions{clientID: "c_desktop"})
	desktop.sendSubscribe(testProject, 0, 0)
	roster := desktop.expectMessageOfType(MsgControlChanged)
	if roster.Control.Controller != nil {
		t.Errorf("a restarted server names %+v as the controller, want nobody",
			roster.Control.Controller)
	}
	if roster.Control.Viewers != 0 {
		t.Errorf("the first device back is told %d others are watching, want 0",
			roster.Control.Viewers)
	}
	// And the screen it was away from is still there, because the session never
	// stopped.
	if frame := desktop.expectFrame(); !bytes.Contains(frame.Payload, []byte("WORK-IN-PROGRESS")) {
		t.Errorf("the restarted server sent %q, want the work that was on screen", frame.Payload)
	}

	tablet := connection(t, after, rt, clientOptions{clientID: "c_tablet"})
	tablet.sendSubscribe(testProject, 0, 0)
	if back := tablet.expectMessageOfType(MsgControlChanged); back.Control.Controller != nil {
		t.Errorf("the second device back was told the controller is %+v, want nobody",
			back.Control.Controller)
	}
	tablet.expectFrame()
	desktop.expectMessageOfType(MsgControlChanged)

	// Neither of them is made the controller for having been there. The device
	// that held the lease before the restart is not remembered, and the device
	// that was only watching is not promoted to fill the gap.
	tablet.expectSilence(50 * time.Millisecond)
	desktop.expectSilence(50 * time.Millisecond)

	// The terminal is free, so the first device to ask for it gets it - which is
	// the ordinary rule and not a special case for a restart.
	tablet.requestControl(testProject)
	granted := tablet.expectMessageOfType(MsgControlGranted)
	if granted.Reason != ReasonAvailable {
		t.Errorf("the grant gave reason %q, want %q", granted.Reason, ReasonAvailable)
	}
	if granted.Control.Controller == nil || granted.Control.Controller.ClientID != "c_tablet" {
		t.Errorf("the grant names controller %+v, want the device that asked",
			granted.Control.Controller)
	}
	tablet.expectMessageOfType(MsgControlChanged)
	desktop.expectMessageOfType(MsgControlChanged)
}
