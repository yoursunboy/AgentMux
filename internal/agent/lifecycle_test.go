package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/task"
)

// These tests are the lifecycle: starting, stopping and restarting an agent, and
// the states each of them is allowed to leave behind.
//
// The runtime is the fake in service_test.go, and what it records is the whole
// subject - whether a second process was launched, whether the terminal was torn
// down, which pid an exit belongs to. The three things that must never happen are
// named in the phase brief and each has a test here: two agents in one runtime, a
// restart that quietly becomes a runtime restart, and an attempt that stays
// RUNNING after the process it recorded has gone.

// ---------------------------------------------------------------------------
// 1. Restart, when it works
// ---------------------------------------------------------------------------

// TestRestartReplacesTheAgentAndKeepsTheRuntime is the whole of §五 in one test.
//
// A restart is an agent operation. The process that was running is asked to stop
// and is confirmed gone, a new one is launched in the same terminal, and the
// runtime is neither stopped nor recreated - which is the claim the page's
// Restart button rests on, because a restart that tore the terminal down would
// take the scrollback with it and every client watching that session would be
// watching a session that no longer exists.
func TestRestartReplacesTheAgentAndKeepsTheRuntime(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	if _, err := h.service.Start(context.Background(), startInput(p, tk)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	// Counted after the first start, because bringing the terminal up is that
	// call's business and this test is about what the restart does on top of it.
	startedBefore, stoppedBefore, _, _ := h.runtimes.counts()
	firstPID := h.runtimes.agentPID(p.ID)

	result, err := h.service.Restart(context.Background(), RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err != nil {
		t.Fatalf("Restart returned an error: %v", err)
	}

	if !result.Agent.Running {
		t.Errorf("the agent is %q after a restart, want a running one", result.Agent.State)
	}
	if result.Agent.PID == firstPID {
		t.Errorf("the agent is still pid %d; nothing was replaced", firstPID)
	}
	if h.runtimes.agentPID(p.ID) != result.Agent.PID {
		t.Errorf("the runtime reports pid %d and the restart reported %d",
			h.runtimes.agentPID(p.ID), result.Agent.PID)
	}

	// The order is the point: the old process has to be gone before the new one
	// is launched, or a declined interrupt leaves two Claudes in one terminal.
	// The fake records every call, so the sequence is checkable rather than
	// assumed.
	if got, want := h.runtimes.sequence(), []string{"start", "stop", "start"}; !equalStrings(got, want) {
		t.Errorf("the runtime was asked to %v; want %v", got, want)
	}

	if started, stopped, _, _ := h.runtimes.counts(); started != startedBefore || stopped != stoppedBefore {
		t.Errorf("a restart stopped or started the runtime (started=%d stopped=%d); "+
			"a restart operates on the agent", started, stopped)
	}
	rt, err := h.runtimes.Runtime(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("reading the runtime failed: %v", err)
	}
	if rt.State != session.StateRunning {
		t.Errorf("the runtime is %q after a restart, want %q", rt.State, session.StateRunning)
	}
	if got := project.SessionNameFor(p.ID); rt.Session != got {
		t.Errorf("the runtime is named %q after a restart, want %q: the session is not recreated",
			rt.Session, got)
	}
}

// TestRestartTakesANewAttemptAndRetiresTheOldOne is §七.
//
// An attempt is the record of one agent's run. A restart that carried the same id
// across both processes would make the second agent's events indistinguishable
// from the first's, and there would be no way to tell from the log where one run
// ended and the next began.
func TestRestartTakesANewAttemptAndRetiresTheOldOne(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	first, err := h.service.Start(context.Background(), startInput(p, tk))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	firstID := first.Session.ID

	result, err := h.service.Restart(context.Background(), RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err != nil {
		t.Fatalf("Restart returned an error: %v", err)
	}

	if result.Session == nil {
		t.Fatal("the restart recorded no attempt for the new agent")
	}
	if result.Session.ID == firstID {
		t.Errorf("the restart reused attempt %s; a restart begins a new one", firstID)
	}
	if result.Session.Status != task.StatusSessionRunning {
		t.Errorf("the new attempt is %q, want %q", result.Session.Status, task.StatusSessionRunning)
	}

	// The attempt that was replaced is reported, and it is the one that was
	// running - not a second row invented for the occasion.
	if result.Retired == nil {
		t.Fatal("the restart did not report the attempt it retired")
	}
	if result.Retired.ID != firstID {
		t.Errorf("the restart retired %s, want %s", result.Retired.ID, firstID)
	}
	if result.Retired.Status != task.StatusSessionCancelled {
		t.Errorf("the retired attempt is %q, want %q: it was asked to stop",
			result.Retired.Status, task.StatusSessionCancelled)
	}

	// Both rows are still there, and the task they belong to is untouched. A
	// restart is not a task operation: the work is the same work, and whether it
	// is still being attempted is what an attempt records.
	attempts := h.sessionsOf(tk.ID)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts are recorded, want 2: the retired one and the new one", len(attempts))
	}
	byID := map[string]string{}
	for _, a := range attempts {
		byID[a.ID] = a.Status
	}
	if byID[firstID] != task.StatusSessionCancelled {
		t.Errorf("the retired attempt reads %q, want %q", byID[firstID], task.StatusSessionCancelled)
	}
	if byID[result.Session.ID] != task.StatusSessionRunning {
		t.Errorf("the new attempt reads %q, want %q", byID[result.Session.ID], task.StatusSessionRunning)
	}

	stored, err := h.sessions.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("reading the task failed: %v", err)
	}
	if stored.Status != tk.Status {
		t.Errorf("the restart moved the task from %q to %q; a restart does not touch it",
			tk.Status, stored.Status)
	}
}

// TestRestartReadsTheLaunchConfigurationAgain is §二十一 and §二十二.
//
// A restart regenerates the command it launches with rather than replaying the
// one that was used before. That is not a detail of tidiness: the permission mode
// is a property of the project that a person can change while an agent is
// running, and a restart that copied the old command would put the agent back
// into a mode its owner had already left.
func TestRestartReadsTheLaunchConfigurationAgain(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	ctx := context.Background()

	if _, err := h.projects.SetPermissionMode(ctx, p.ID, claude.PermissionManual); err != nil {
		t.Fatalf("SetPermissionMode returned an error: %v", err)
	}
	if _, err := h.service.Start(ctx, startInput(p, nil)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	before, ok := h.runtimes.lastLaunch()
	if !ok {
		t.Fatal("the runtime was never asked to launch anything")
	}
	if before.PermissionMode != string(claude.PermissionManual) {
		t.Fatalf("the first launch carried %q, want %q", before.PermissionMode, claude.PermissionManual)
	}

	// Somebody changes the mode while the agent is running, and then restarts it.
	if _, err := h.projects.SetPermissionMode(ctx, p.ID, claude.PermissionBypass); err != nil {
		t.Fatalf("SetPermissionMode returned an error: %v", err)
	}
	if _, err := h.service.Restart(ctx, RestartInput{ProjectID: p.ID}); err != nil {
		t.Fatalf("Restart returned an error: %v", err)
	}

	after, ok := h.runtimes.lastLaunch()
	if !ok {
		t.Fatal("the restart launched nothing")
	}
	if after.PermissionMode != string(claude.PermissionBypass) {
		t.Errorf("the restarted launch carried %q, want %q: the configuration is read again, "+
			"not copied from the launch it replaces", after.PermissionMode, claude.PermissionBypass)
	}
	if after.SessionID == before.SessionID {
		t.Errorf("the restarted launch reused session id %q; each launch is a new session",
			before.SessionID)
	}

	// The document is the other half of the configuration, and it is written
	// again for the new launch rather than left as it was. A bypass launch has to
	// carry the key that tells Claude its dangerous-mode confirmation has already
	// been given - that is the whole of the 7.5.3 fix, and a restart that used a
	// stale document would put Claude back on the prompt it was fixed off.
	runtimeID := project.SessionNameFor(p.ID)
	_, document := h.settingsFor(runtimeID)
	if !strings.Contains(string(document), "skipDangerousModePermissionPrompt") {
		t.Errorf("the restarted document does not carry the bypass confirmation:\n%s", document)
	}
}

// TestRestartWithoutARunningAgentIsAStart covers the half that has nothing to do.
//
// A project whose terminal is up and whose agent has exited is a project an owner
// may well press Restart on - the button is how they get Claude back - and the
// call has to mean what it says rather than fail because there was nothing to
// stop.
func TestRestartWithoutARunningAgentIsAStart(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	ctx := context.Background()

	// A terminal that is up, with no agent in it.
	h.runtimes.setState(p.ID, session.StateRunning)

	result, err := h.service.Restart(ctx, RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err != nil {
		t.Fatalf("Restart returned an error: %v", err)
	}
	if !result.Agent.Running {
		t.Errorf("the agent is %q after restarting a project that had none", result.Agent.State)
	}
	if result.Session == nil || result.Session.Status != task.StatusSessionRunning {
		t.Errorf("the restart recorded %+v, want a running attempt", result.Session)
	}
	if result.Retired != nil {
		t.Errorf("the restart retired %s, but there was no agent to replace", result.Retired.ID)
	}
	if _, _, agentStarts, agentStops := h.runtimes.counts(); agentStarts != 1 || agentStops != 0 {
		t.Errorf("the runtime saw %d starts and %d stops, want 1 and 0: "+
			"nothing was running to interrupt", agentStarts, agentStops)
	}
	if started, _, _, _ := h.runtimes.counts(); started != 0 {
		t.Error("the restart started the runtime; the terminal was already up")
	}
}

// ---------------------------------------------------------------------------
// 2. Restart, when it must not proceed
// ---------------------------------------------------------------------------

// TestRestartStopsRatherThanLaunchingASecondAgent is the refusal §八 asks for.
//
// The old agent was asked to stop and did not. Everything after that point would
// be a second Claude in a terminal that already has one - so nothing after that
// point happens. The old agent is still running, the old attempt is still open,
// and the caller is told which of those two ways the stop failed.
func TestRestartStopsRatherThanLaunchingASecondAgent(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	ctx := context.Background()

	if _, err := h.service.Start(ctx, startInput(p, tk)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	_, _, agentStarts, _ := h.runtimes.counts()
	h.runtimes.stopAgentKept = true

	result, err := h.service.Restart(ctx, RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err == nil {
		t.Fatal("Restart reported success for an agent the interrupt did not stop")
	}
	if !session.IsCode(err, session.CodeAgentStopTimeout) {
		t.Errorf("Restart returned %v; want a %s error", err, session.CodeAgentStopTimeout)
	}
	if !result.Agent.Running {
		t.Error("the agent is reported as stopped, but the interrupt was declined")
	}
	if _, _, now, _ := h.runtimes.counts(); now != agentStarts {
		t.Errorf("the runtime was asked to launch %d agents; want the %d that were already there",
			now, agentStarts)
	}
	if got := h.runtimes.agentPID(p.ID); got == 0 {
		t.Error("the old agent was cleared from the runtime even though it never stopped")
	}

	// The attempt it was running is untouched, and no new one was recorded: a
	// restart that did not happen must not look like one that did.
	attempts := h.sessionsOf(tk.ID)
	if len(attempts) != 1 || attempts[0].Status != task.StatusSessionRunning {
		t.Errorf("the attempts are %+v; want the one that is still running", attempts)
	}
	if result.Retired != nil {
		t.Errorf("the failed restart retired %s; nothing was retired", result.Retired.ID)
	}
}

// TestRestartCannotBringUpATerminal covers §六's other direction.
//
// A restart does not create a runtime, because a call that quietly created one
// would make "restart the agent" and "start the terminal" the same request with
// no way to ask for only the second. The refusal names the reason, and nothing is
// launched.
func TestRestartCannotBringUpATerminal(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	// Nothing has started this project's runtime, so it is STOPPED.
	result, err := h.service.Restart(context.Background(), RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err == nil {
		t.Fatal("Restart succeeded for a project whose terminal is not running")
	}
	// The code is the runtime layer's, deliberately: "there is no terminal" is
	// the runtime's own fact and the HTTP layer already maps it to 409 for every
	// other endpoint that needs one.
	if CodeOf(err) != session.CodeNotRunning {
		t.Errorf("Restart returned %v; want a %s error", err, session.CodeNotRunning)
	}
	if started, _, agentStarts, _ := h.runtimes.counts(); started != 0 || agentStarts != 0 {
		t.Errorf("the refused restart started the runtime (%d) or an agent (%d)", started, agentStarts)
	}
	if result.Session != nil {
		t.Errorf("the refused restart recorded attempt %s", result.Session.ID)
	}
	if attempts := h.sessionsOf(tk.ID); len(attempts) != 0 {
		t.Errorf("the refused restart recorded %d attempts; want none", len(attempts))
	}
}

// TestARestartThatCannotStartKeepsBothAttempts is §九.
//
// The agent was stopped and the new one never came up. That is a real outcome and
// it has to be legible afterwards: the attempt that was retired stays retired as
// cancelled, the attempt the failed start created is failed, and neither is
// deleted. A caller reading the history has to be able to see that there were two
// runs and what became of each.
func TestARestartThatCannotStartKeepsBothAttempts(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	ctx := context.Background()

	first, err := h.service.Start(ctx, startInput(p, tk))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	h.runtimes.failAgent = errors.New("the command was typed and no process appeared")

	result, err := h.service.Restart(ctx, RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err == nil {
		t.Fatal("Restart reported success for an agent that never started")
	}
	if result.Retired == nil || result.Retired.ID != first.Session.ID {
		t.Errorf("the failed restart reported %+v; want the retired attempt %s",
			result.Retired, first.Session.ID)
	}
	if result.Retired.Status != task.StatusSessionCancelled {
		t.Errorf("the retired attempt is %q, want %q", result.Retired.Status, task.StatusSessionCancelled)
	}

	attempts := h.sessionsOf(tk.ID)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts are recorded, want 2: neither run is deleted", len(attempts))
	}
	statuses := map[string]string{}
	for _, a := range attempts {
		statuses[a.ID] = a.Status
	}
	if statuses[first.Session.ID] != task.StatusSessionCancelled {
		t.Errorf("the retired attempt reads %q, want %q",
			statuses[first.Session.ID], task.StatusSessionCancelled)
	}
	var newID string
	for id := range statuses {
		if id != first.Session.ID {
			newID = id
		}
	}
	if statuses[newID] != task.StatusSessionFailed {
		t.Errorf("the attempt the failed start created reads %q, want %q",
			statuses[newID], task.StatusSessionFailed)
	}

	// The terminal is still up and still has no agent in it, which is the state a
	// stop leaves - not a state anything invented.
	if _, bound := h.service.Run(p.ID); bound {
		t.Error("the failed restart left a binding for an agent that never started")
	}
}

// ---------------------------------------------------------------------------
// 3. Restart after the server that started the agent is gone
// ---------------------------------------------------------------------------

// TestRestartClosesAnAttemptThisServerNeverBound is the adopted case.
//
// It is the same shape as the exit path: a server restarts, reattaches to a
// terminal that is already running an agent, and holds no binding for it - the
// attempt was created by the process that went away. Restarting then has to close
// that attempt by looking it up, because nothing in memory will ever close it and
// a row left RUNNING for a process that has been replaced is a row that lies.
func TestRestartClosesAnAttemptThisServerNeverBound(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	ctx := context.Background()

	// The agent is started by a service that then goes away, exactly as a server
	// restart leaves things: the process is running, the attempt is in the log,
	// and the replacement process holds no binding for either.
	if _, err := h.service.Start(ctx, startInput(p, tk)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := h.service.Close(ctx); err != nil {
		t.Fatalf("closing the first coordinator failed: %v", err)
	}
	adopting, err := NewService(Options{
		Runtimes:       h.runtimes,
		Adapters:       h.adapters,
		Sessions:       h.sessions,
		Settings:       h.settings,
		LaunchSettings: h.projects,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewService returned an error: %v", err)
	}
	t.Cleanup(func() { _ = adopting.Close(context.Background()) })

	if _, bound := adopting.Run(p.ID); bound {
		t.Fatal("the replacement coordinator is bound to an agent it never started")
	}

	result, err := adopting.Restart(ctx, RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err != nil {
		t.Fatalf("Restart returned an error: %v", err)
	}
	if result.Retired == nil {
		t.Fatal("the restart did not close the attempt the previous server left open")
	}
	if result.Retired.Status != task.StatusSessionCancelled {
		t.Errorf("the adopted attempt is %q, want %q",
			result.Retired.Status, task.StatusSessionCancelled)
	}

	attempts := h.sessionsOf(tk.ID)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts are recorded, want 2", len(attempts))
	}
	open := 0
	for _, a := range attempts {
		if a.Status == task.StatusSessionRunning {
			open++
		}
	}
	if open != 1 {
		t.Errorf("%d attempts are RUNNING after the restart, want exactly 1", open)
	}
}

// ---------------------------------------------------------------------------
// 4. An exit that arrives too late
// ---------------------------------------------------------------------------

// TestAnExitFromAReplacedProcessDoesNotCloseTheNewAttempt is the hazard a restart
// is the only way to reach.
//
// The watcher following the old process notices it end at the moment the restart
// is between stopping and starting, so its report queues on the project lock the
// restart is holding and is delivered after the new binding exists. Without
// something to compare, it would find a binding for that project and close it -
// closing an attempt that had only just been opened, and leaving the new agent
// unobserved.
//
// The pid is what makes the two tellable apart, and this is the test that the
// comparison is made.
func TestAnExitFromAReplacedProcessDoesNotCloseTheNewAttempt(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	ctx := context.Background()

	first, err := h.service.Start(ctx, startInput(p, tk))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	oldPID := h.runtimes.agentPID(p.ID)

	result, err := h.service.Restart(ctx, RestartInput{ProjectID: p.ID, TaskID: tk.ID})
	if err != nil {
		t.Fatalf("Restart returned an error: %v", err)
	}
	if result.Agent.PID == oldPID {
		t.Fatalf("the restart did not replace the process, so there is nothing late to deliver")
	}

	// The old process ends, and its watcher reports it now - after the restart.
	h.service.AgentExited(ctx, session.AgentExit{
		ProjectID: p.ID,
		RuntimeID: project.SessionNameFor(p.ID),
		AgentType: claude.Type,
		PID:       oldPID,
		Asked:     true,
		At:        fixedNow(),
	})

	live, err := h.sessions.GetSession(ctx, result.Session.ID)
	if err != nil {
		t.Fatalf("reading the new attempt failed: %v", err)
	}
	if live.Status != task.StatusSessionRunning {
		t.Errorf("the new attempt is %q after the replaced process was reported gone; want %q",
			live.Status, task.StatusSessionRunning)
	}
	if _, bound := h.service.Run(p.ID); !bound {
		t.Error("the restart's binding was released by a report about the process it replaced")
	}
	// The retired attempt keeps the status the restart gave it rather than being
	// rewritten by the late report.
	retired, err := h.sessions.GetSession(ctx, first.Session.ID)
	if err != nil {
		t.Fatalf("reading the retired attempt failed: %v", err)
	}
	if retired.Status != task.StatusSessionCancelled {
		t.Errorf("the retired attempt is %q, want %q", retired.Status, task.StatusSessionCancelled)
	}
}

// TestAnExitFromTheBoundProcessStillClosesTheAttempt is the other half of the
// guard, and the one that keeps it honest.
//
// A comparison that ignored every exit would pass the test above and break the
// thing it protects: an agent that dies on its own would leave its attempt
// RUNNING, which is the defect AgentExitObserver was added to fix. The exit that
// names the bound process has to go through.
func TestAnExitFromTheBoundProcessStillClosesTheAttempt(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	ctx := context.Background()

	started, err := h.service.Start(ctx, startInput(p, tk))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	h.service.AgentExited(ctx, session.AgentExit{
		ProjectID: p.ID,
		RuntimeID: project.SessionNameFor(p.ID),
		AgentType: claude.Type,
		PID:       started.Agent.PID,
		Asked:     false,
		At:        fixedNow(),
	})

	closed, err := h.sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("reading the attempt failed: %v", err)
	}
	if closed.Status != task.StatusSessionFailed {
		t.Errorf("the attempt is %q after its process ended with nothing asking it to; want %q",
			closed.Status, task.StatusSessionFailed)
	}
	if _, bound := h.service.Run(p.ID); bound {
		t.Error("the binding survived the exit of the process it was for")
	}
}

// ---------------------------------------------------------------------------
// 5. Concurrency
// ---------------------------------------------------------------------------

// TestConcurrentStartsAndRestartsLeaveOneAgent is §十.
//
// Two requests arriving together must not produce two Claudes. The mechanism is
// the one already in the service - the per-project stripe lock every operation
// takes - and what this checks is that taking it in Restart is enough: the calls
// serialise, each sees the state the previous one left, and the runtime is never
// asked to launch while an agent it does not know about is running.
func TestConcurrentStartsAndRestartsLeaveOneAgent(t *testing.T) {
	h := newHarness(t, Options{})
	h.runtimes.pidAsync = true
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	ctx := context.Background()

	if _, err := h.service.Start(ctx, startInput(p, tk)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	const callers = 4
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = h.service.Restart(ctx, RestartInput{ProjectID: p.ID, TaskID: tk.ID})
				return
			}
			_, errs[i] = h.service.Start(ctx, startInput(p, tk))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d returned an error: %v", i, err)
		}
	}

	if got := h.runtimes.agentPID(p.ID); got == 0 {
		t.Fatal("no agent is running after the calls; the last one to arrive left nothing")
	}
	if _, bound := h.service.Run(p.ID); !bound {
		t.Error("nothing is bound after the calls, so the agent that is running is unobserved")
	}

	// Every launch was preceded by a stop once an agent existed, which is the
	// property that keeps the count at one: a second launch with the first still
	// running is the arrangement this tests for.
	seq := h.runtimes.sequence()
	if len(seq) == 0 || seq[0] != "start" {
		t.Fatalf("the runtime saw %v; want it to have started first", seq)
	}
	for i, call := range seq {
		if i == 0 {
			continue
		}
		if call == "start" && seq[i-1] != "stop" {
			t.Errorf("the runtime saw %v: a launch followed a launch", seq)
		}
	}

	// Exactly one attempt is open, whatever the callers did between them.
	open := 0
	for _, a := range h.sessionsOf(tk.ID) {
		if a.Status == task.StatusSessionRunning {
			open++
		}
	}
	if open != 1 {
		t.Errorf("%d attempts are RUNNING, want exactly 1", open)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// equalStrings reports whether two string slices are the same, element by
// element.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
