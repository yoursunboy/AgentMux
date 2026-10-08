package session

import (
	"bytes"
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests are about what a lifecycle request looks like while it is
// happening, and about the two ways a stop can find nothing to interrupt.
//
// The suite next door checks what a start, a stop and an exit report once they
// have happened. What is left over is the interval in the middle - a stop that
// is still in flight, a watcher that has been superseded by the agent that
// replaced it - and the second request for something the first one already did.
// Those are the states a dashboard draws and the states a retry lands in, and
// each of them is only reachable by driving the runtime and reading it while it
// works.

// agentTerminal returns a backend on a project's own socket, for a test that
// has to type into the terminal and read what is on it.
//
// It reaches the runtime the way paneProcesses does - through the project's
// socket rather than through the manager - so that what a test sees in the
// terminal is the terminal's own answer and not the runtime's account of it.
func agentTerminal(t *testing.T, runtimes *ProjectRuntimes, p *project.Project) *TmuxBackend {
	t.Helper()
	return NewTmuxBackend(TmuxOptions{
		SocketPath: runtimes.Sockets().Path(p.ID),
		Logger:     discardLogger(),
	})
}

// waitForPaneText waits until a session's screen contains want.
func waitForPaneText(t *testing.T, b *TmuxBackend, name, want string, wait time.Duration) {
	t.Helper()

	deadline := time.Now().Add(wait)
	var last []byte
	for time.Now().Before(deadline) {
		last = screenRows(t, b, name)
		if bytes.Contains(last, []byte(want)) {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("the pane %q never showed %q (the last capture was %d bytes)", name, want, len(last))
}

// statesOf renders the states a test observed, for a failure message.
func statesOf(statuses []AgentStatus) []AgentState {
	out := make([]AgentState, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, s.State)
	}
	return out
}

// TestAStopInFlightIsReportedAsStopping covers the state §九 of the phase brief
// draws as "Stopping...".
//
// A stop is not instantaneous: the interrupt is delivered, and the process is
// then watched until it goes. For as long as that takes the runtime knows two
// things that are true at once - somebody has asked the agent to stop, and the
// agent's process is still there - and the status has to say both. Reporting
// only the process table would tell a dashboard "running" for a stop that is
// already under way, and the dashboard would offer a Stop button for a stop
// that is already running. Reporting only the record would claim the agent had
// stopped while its process was still working.
//
// # Why the state is only observable when the interrupt is declined
//
// A program that takes the interrupt is gone in microseconds, and the interval
// between the request being recorded and the process being missed is shorter
// than any read. The declining shell holds the interval open for the whole
// grace, which is what makes it observable at all. Where the shell takes the
// interrupt the test skips, for the reason the declined-interrupt test next
// door skips: the behaviour is not wrong there, it is simply not reachable from
// here.
func TestAStopInFlightIsReportedAsStopping(t *testing.T) {
	ctx := context.Background()
	shell := requireProgram(t, "sh")

	p := agentTestProject(t, "p_agent_stopping_visible")
	spec := interruptIgnorer(t, shell, p.RuntimePath)
	// Long enough that the in-flight state is comfortably observable, short
	// enough that a test which has to wait it out is still a fast one.
	const grace = 3 * time.Second
	m, _ := agentTestManagerOn(t, uniqueSocketDir(t), newFakeStore(),
		&fakeAgent{spec: spec}, grace, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	started, err := m.StartAgent(ctx, p.ID, AgentLaunch{})
	if err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}

	// A reader follows the agent while the stop is in flight. This is what a
	// dashboard does, and it is the only way to see a state that exists only
	// while the stop is happening.
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	var mu sync.Mutex
	var seen []AgentStatus
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for readCtx.Err() == nil {
			status, err := m.Agent(readCtx, p.ID)
			if err == nil {
				mu.Lock()
				seen = append(seen, status)
				mu.Unlock()
			}
			select {
			case <-readCtx.Done():
				return
			case <-time.After(testAgentPoll):
			}
		}
	}()

	stopped := make(chan error, 1)
	go func() {
		_, err := m.StopAgent(ctx, p.ID)
		stopped <- err
	}()

	var stopErr error
	select {
	case stopErr = <-stopped:
	case <-time.After(grace + testAgentStopGrace):
		t.Fatal("the stop never returned")
	}
	stopReading()
	<-readerDone

	if !processIsRunning(t, started.PID, spec.Executable) {
		t.Skip("this shell did not survive the interrupt, so a stop in flight cannot be observed here")
	}
	if !IsCode(stopErr, CodeAgentStopTimeout) {
		t.Fatalf("StopAgent returned %v; want a %s error", stopErr, CodeAgentStopTimeout)
	}

	mu.Lock()
	defer mu.Unlock()
	var inFlight *AgentStatus
	for i := range seen {
		if seen[i].State == AgentStopping {
			inFlight = &seen[i]
			break
		}
	}
	if inFlight == nil {
		t.Fatalf("the agent never reported %q while a stop was in flight; the states read were %v",
			AgentStopping, statesOf(seen))
	}
	if !inFlight.Running {
		t.Error("the agent's process is still there during a stop, and the status does not report it running")
	}
	if !inFlight.Requested {
		t.Error("a stop is in flight and the status does not record that one was asked for")
	}
	if inFlight.PID != started.PID {
		t.Errorf("the in-flight stop reports pid %d, want the process it is stopping, %d",
			inFlight.PID, started.PID)
	}
	if inFlight.Message != "" {
		t.Errorf("a stop in flight carries the message %q; a stop nothing has refused has nothing to explain",
			inFlight.Message)
	}
}

// gateRecorder is an exit observer that holds the first exit it is told about
// until the test lets it go.
//
// The hold is the whole point: it puts the first watcher inside its report at
// the moment a replacement agent is started, which is the one moment where the
// watcher that is reporting and the watcher that is watching are two different
// things.
//
// The held exit goes to `entered` and only the exits after it go to `exits`, so
// the two are disjoint. That division is what makes the replacement test read
// correctly: it waits on `exits` for the exit that happened *after* it opened
// the gate, and recording the held one there as well would put the agent being
// replaced in front of the replacement and the await would hand back the wrong
// process.
type gateRecorder struct {
	entered chan AgentExit
	release chan struct{}
	exits   *exitRecorder

	mu   sync.Mutex
	held bool
}

func newGateRecorder() *gateRecorder {
	return &gateRecorder{
		entered: make(chan AgentExit, 1),
		release: make(chan struct{}),
		exits:   newExitRecorder(),
	}
}

func (r *gateRecorder) AgentExited(_ context.Context, exit AgentExit) {
	r.mu.Lock()
	hold := !r.held
	r.held = true
	r.mu.Unlock()

	if !hold {
		r.exits.AgentExited(context.Background(), exit)
		return
	}
	select {
	case r.entered <- exit:
	default:
	}
	<-r.release
}

// TestTheAgentThatReplacesAnotherIsStillWatched is the regression test for a
// watcher that cancels its successor.
//
// Watching an agent ends by tearing the watch down, and tearing it down cancels
// the context the watcher runs on. That is right when the watcher is still the
// runtime's - and wrong when it is not, which is exactly what a replacement
// makes it. Reporting an exit can block for as long as the layer that follows
// attempts is busy, and a start that arrives while it is blocked installs a new
// watcher and cancels the old one's context. The old watcher then resumes, is
// cancelled, and - without the guard - cancels the *runtime's* watch, which by
// then is the new one's.
//
// What that costs is the new agent's exit: nothing observes it, so nothing
// reports it, so the attempt it belongs to stays RUNNING for as long as the
// server does. The test is therefore the observation itself - the replacement
// agent is ended, and its exit has to arrive.
//
// A start is what replaces the agent here rather than the restart endpoint,
// because the runtime has no restart: restart is a stop and a start composed by
// the layer that owns attempts, and the composition this test is about is the
// one in this package.
func TestTheAgentThatReplacesAnotherIsStillWatched(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_replaced_watched")
	// The first agent ends on its own, quickly, so that its watcher reaches the
	// report the test is going to hold. The second runs until it is signalled,
	// so that it is still there when the held watcher resumes - a second agent
	// that ended early would be reported before there was anything to cancel.
	provider := &fakeAgent{spec: sleep.spec(1)}
	gate := newGateRecorder()
	m, runtimes := agentTestManagerObserving(t, uniqueSocketDir(t), newFakeStore(),
		provider, testAgentStopGrace, gate, p)

	terminal := agentTerminal(t, runtimes, p)
	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	// What the shell calls itself, so the test can tell when the pane is back
	// at a prompt and a command typed at it will reach the shell.
	shellName := paneCommand(t, terminal, p.SessionName())

	if _, err := m.StartAgent(ctx, p.ID, AgentLaunch{}); err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}

	// The first agent ends, and its watcher is now inside the report.
	select {
	case <-gate.entered:
	case <-time.After(testAgentStopGrace):
		t.Fatal("the first agent's exit was never reported")
	}

	// A command typed while the previous one is still exiting goes to that
	// process and is lost, so the pane is waited back to its shell first.
	waitForPaneCommand(t, terminal, p.SessionName(), shellName)

	// The replacement. It is this call that cancels the held watcher's context.
	provider.spec = sleep.spec(300)
	replacement, err := m.StartAgent(ctx, p.ID, AgentLaunch{})
	if err != nil {
		t.Fatalf("the replacement StartAgent returned an error: %v", err)
	}
	if replacement.State != AgentRunning {
		t.Fatalf("the replacement agent is %q, want %q", replacement.State, AgentRunning)
	}
	if replacement.PID == 0 {
		t.Fatal("the replacement agent has no pid")
	}

	close(gate.release)

	// The superseded watcher has nothing left to do but decide whether it is
	// still the runtime's. Give it the moment that takes, so that what follows
	// measures whether it took the new watcher down rather than racing it.
	time.Sleep(200 * time.Millisecond)

	if err := signalProcess(t, replacement.PID, syscall.SIGTERM); err != nil {
		t.Fatalf("could not signal the replacement agent: %v", err)
	}

	// The exit waited for here is the replacement's: the gate routed the agent
	// being replaced to `entered` when the test took it above.
	exit := gate.exits.await(t, testAgentStopGrace)
	if exit.PID != replacement.PID {
		t.Errorf("the reported exit names pid %d, want the replacement's %d", exit.PID, replacement.PID)
	}
	if exit.Asked {
		t.Error("nothing asked the replacement agent to stop, so the exit must not say anything did")
	}

	status, err := m.Agent(ctx, p.ID)
	if err != nil {
		t.Fatalf("Agent returned an error: %v", err)
	}
	if status.State != AgentExited {
		t.Errorf("the replacement agent is %q after it was signalled, want %q", status.State, AgentExited)
	}
}

// TestAStartCancelsTheWatchItReplaces pins the cancel that a start owes the
// agent it is replacing.
//
// The test above is the behaviour, driven through a real terminal; this is the
// one line of it a machine without `sleep` can still check, and the line is
// where the bug was. Replacing the record takes the previous agent's watch with
// it, so unless that watch is cancelled on the way out nothing can reach the
// watcher holding it - and a watcher nobody can cancel wakes up, finds its
// context alive, concludes it is still the runtime's, and untracks the watch
// the new agent is being given. The new agent is then watched by nobody, and
// its exit is never reported.
func TestAStartCancelsTheWatchItReplaces(t *testing.T) {
	rt := &runtime{projectID: "p_watch_is_replaced", session: "amx-p_watch_is_replaced"}

	previous, cancelPrevious := context.WithCancel(context.Background())
	defer cancelPrevious()
	rt.agent = agentState{
		spec:    AgentSpec{Type: "testagent", Executable: "/usr/bin/sleep"},
		state:   AgentRunning,
		pid:     4242,
		watch:   cancelPrevious,
		watched: true,
	}

	rt.setAgentLaunching(AgentSpec{Type: "testagent", Executable: "/usr/bin/sleep"}, time.Now())

	if previous.Err() == nil {
		t.Error("the watch of the agent being replaced was left alive: the watcher holding it cannot be told it is no longer the runtime's, and will untrack the start that is about to install its successor")
	}
	if rt.agent.watch != nil {
		t.Error("the record still carries a watch after a launch replaced it")
	}
	if rt.agent.state != AgentStarting {
		t.Errorf("the record says %q after a launch, want %q", rt.agent.state, AgentStarting)
	}
}

// TestAStopInFlightSurvivesTheWatcherTick pins the state §九 draws as
// "Stopping...".
//
// The watcher ticks several times a second and each tick says the agent's
// process is still there. That is true, and it is also half of what a stop in
// flight means - the other half is that somebody has asked, which only the
// record knows. A tick that wrote RUNNING over the request would make a stop
// under way and an agent nobody has touched the same reading, and the state a
// dashboard would nearly always draw during a stop is the one that offers to
// start one.
func TestAStopInFlightSurvivesTheWatcherTick(t *testing.T) {
	rt := &runtime{projectID: "p_stopping_is_sticky", session: "amx-p_stopping_is_sticky"}
	rt.agent = agentState{
		spec:  AgentSpec{Type: "testagent", Executable: "/usr/bin/sleep"},
		state: AgentRunning,
		pid:   4242,
	}

	if !rt.noteAgentStopRequested(time.Now()) {
		t.Fatal("a running agent refused a stop request")
	}
	rt.noteAgentSeen(processRef{PID: 4242})

	if rt.agent.state != AgentStopping {
		t.Errorf("a watcher tick during a stop left the record at %q, want %q", rt.agent.state, AgentStopping)
	}
	if rt.agent.pid != 4242 {
		t.Errorf("the tick did not refresh the pid: the record holds %d, want 4242", rt.agent.pid)
	}

	// And the stickiness is not permanence: a stop whose grace runs out is
	// reported as a stop that was declined, which is a running agent again.
	rt.noteAgentInterruptIgnored(time.Now())
	if rt.agent.state != AgentRunning {
		t.Errorf("an interrupt that was not taken left the record at %q, want %q", rt.agent.state, AgentRunning)
	}
}

// TestASecondStopDoesNotInterruptTheTerminal covers the half of stop's
// idempotence that a status cannot show.
//
// Asking to stop an agent that has already stopped is not an error and is the
// state the caller wanted, and the test next door covers that answer. What it
// cannot cover is the interrupt. A second stop has nothing to interrupt, and a
// stop that delivered one anyway would put a Ctrl-C into a terminal whose agent
// is gone - which is a terminal a person may well be typing at, and the byte
// would land in whatever they were doing.
//
// So the check is on the terminal itself. A marker is typed at the prompt and
// the pane is captured either side of the second stop: a delivered interrupt
// echoes as ^C in the scrollback, and nothing else in this sequence produces
// one.
func TestASecondStopDoesNotInterruptTheTerminal(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_stop_twice")
	spec := sleep.spec(300)
	m, runtimes := agentTestManager(t, newFakeStore(), &fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	if _, err := m.StartAgent(ctx, p.ID, AgentLaunch{}); err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}
	first, err := m.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("the first StopAgent returned an error: %v", err)
	}
	if first.State != AgentStopped {
		t.Fatalf("the agent is %q after the first stop, want %q", first.State, AgentStopped)
	}

	terminal := agentTerminal(t, runtimes, p)
	if err := terminal.Launch(ctx, p.SessionName(), "printf 'amx-second-stop\\n'"); err != nil {
		t.Fatalf("could not type into the terminal: %v", err)
	}
	waitForPaneText(t, terminal, p.SessionName(), "amx-second-stop", outputWait)
	before := screenRows(t, terminal, p.SessionName())

	second, err := m.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("the second StopAgent returned an error: %v", err)
	}
	if second.State != AgentStopped {
		t.Errorf("the second stop reports %q, want %q", second.State, AgentStopped)
	}
	if second.Running {
		t.Error("an agent is reported running after a stop that had nothing to stop")
	}

	after := screenRows(t, terminal, p.SessionName())
	if added := bytes.Count(after, []byte("^C")) - bytes.Count(before, []byte("^C")); added != 0 {
		t.Errorf("a stop with nothing to stop put %d interrupts into the terminal; it must type nothing", added)
	}
}

// TestStopAfterTheAgentLeftOnItsOwnTypesNothing is the same claim for the other
// way an agent is already gone.
//
// An agent that ended without being asked is reported as EXITED, and a stop
// arriving afterwards has to recognise that there is nothing to interrupt. The
// record must not be rewritten either: an exit nothing asked for is a different
// event from a stop, and a stop that stamped "requested" on it would tell the
// history that somebody stopped an agent that in fact ended by itself.
func TestStopAfterTheAgentLeftOnItsOwnTypesNothing(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_stop_after_exit")
	spec := sleep.spec(1)
	m, runtimes := agentTestManager(t, newFakeStore(), &fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	if _, err := m.StartAgent(ctx, p.ID, AgentLaunch{}); err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}
	exited := waitForAgentState(t, m, p.ID, AgentExited, testAgentStopGrace)
	if exited.Requested {
		t.Fatal("nothing asked this agent to stop, so the exit must not say anything did")
	}

	terminal := agentTerminal(t, runtimes, p)
	if err := terminal.Launch(ctx, p.SessionName(), "printf 'amx-after-exit\\n'"); err != nil {
		t.Fatalf("could not type into the terminal: %v", err)
	}
	waitForPaneText(t, terminal, p.SessionName(), "amx-after-exit", outputWait)
	before := screenRows(t, terminal, p.SessionName())

	after, err := m.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StopAgent after the agent left on its own returned an error: %v", err)
	}
	if after.State != AgentExited {
		t.Errorf("the agent is reported %q after a stop that had nothing to stop, want %q",
			after.State, AgentExited)
	}
	if after.Requested {
		t.Error("a stop that found nothing to stop recorded that a stop was asked for")
	}

	captured := screenRows(t, terminal, p.SessionName())
	if added := bytes.Count(captured, []byte("^C")) - bytes.Count(before, []byte("^C")); added != 0 {
		t.Errorf("a stop with nothing to stop put %d interrupts into the terminal; it must type nothing", added)
	}
}
