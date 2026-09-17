package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests drive the agent half of the runtime against real processes and a
// real tmux server.
//
// The agent they start is not Claude Code. It is /bin/sleep, chosen because it
// is a process like any other: it has an executable, a working directory and a
// lifetime, which is the whole of what the runtime reasons about. That is the
// point of the design under test - the runtime knows an agent by its process and
// by nothing else - so a suite that used Claude Code to check it would be paying
// for a proof it does not need, and one that used a fake process tree would be
// asserting that the fake behaves as written.
//
// What these tests therefore cannot cover is Claude Code itself, and that is
// covered separately, opt-in, by cmd/server's end-to-end test. See
// docs/CLAUDE_RUNTIME.md.

// testAgentPoll is how often the runtime looks at the process table here.
//
// It is much shorter than the production default so that a lifecycle test takes
// a fraction of a second rather than a few. Every wait below is bounded by a
// generous deadline and returns as soon as its condition holds, so a short poll
// makes the suite fast without making it flaky.
const testAgentPoll = 25 * time.Millisecond

// testAgentStartTimeout bounds the wait for a started agent to appear.
const testAgentStartTimeout = 20 * time.Second

// testAgentStopGrace bounds the wait for an interrupted agent to go.
const testAgentStopGrace = 15 * time.Second

// fakeAgent is an AgentProvider over a fixed spec.
type fakeAgent struct {
	spec AgentSpec
	err  error
}

func (f fakeAgent) Spec(context.Context) (AgentSpec, error) { return f.spec, f.err }

// agentProgram is a real executable the tests start as the agent.
type agentProgram struct {
	// Path is the canonical path, which is what the runtime matches a process
	// against and therefore what an AgentSpec must carry.
	Path string
	// Name is the file's base name, for building shell commands.
	Name string
}

// requireProgram resolves a real program on PATH, skipping the test where there
// is none.
//
// A skip rather than a failure: these are POSIX assumptions, and the suite is
// also run cross-compiled on a Windows host that has no /bin at all.
func requireProgram(t *testing.T, name string) agentProgram {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s program is available, so no agent process can be started: %v", name, err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return agentProgram{Path: path, Name: filepath.Base(path)}
}

// requireSleep resolves the program the tests use as an agent.
func requireSleep(t *testing.T) agentProgram {
	t.Helper()
	return requireProgram(t, "sleep")
}

// requireBlocker resolves a program that occupies a terminal indefinitely
// without being the agent. cat with no arguments reads its input and waits,
// which is what a user's own command looks like from the outside.
func requireBlocker(t *testing.T) agentProgram {
	t.Helper()
	return requireProgram(t, "cat")
}

// spec builds an AgentSpec that starts this program for the given number of
// seconds.
func (a agentProgram) spec(seconds int) AgentSpec {
	return AgentSpec{
		Type:       "testagent",
		Version:    "1.0.0",
		Command:    shellQuote(a.Path) + " " + strconv.Itoa(seconds),
		Executable: a.Path,
	}
}

// specRunning builds a spec whose command leaves the project directory before
// starting, for the test that the runtime notices where an agent really is.
func (a agentProgram) specRunning(dir string, seconds int) AgentSpec {
	spec := a.spec(seconds)
	spec.Command = "cd " + shellQuote(dir) + " && " + spec.Command
	return spec
}

// shellQuote renders a path as a single shell word.
//
// It restates the launcher's quoting in four lines rather than importing it.
// The session package must not know that Claude exists - that separation is part
// of what these tests check holds - and a test-only import to borrow a quoting
// function would be the first thread of the dependency this design refuses.
func shellQuote(s string) string {
	if s == "" {
		return ""
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// agentTestManager builds a manager that can host an agent, over a socket
// directory of its own.
func agentTestManager(t *testing.T, store RuntimeStore, provider AgentProvider,
	projects ...*project.Project) (*Manager, *ProjectRuntimes) {
	t.Helper()
	return agentTestManagerOn(t, uniqueSocketDir(t), store, provider, testAgentStopGrace, projects...)
}

// agentTestManagerOn builds the same manager over a given socket directory, for
// the restart test, which needs a second manager to find the first one's work.
//
// The stop grace is a parameter because it is the one timing a test has a reason
// to shorten: a stop that is going to be declined waits out the whole grace
// before it says so, and a test about what a declined stop reports should not
// spend fifteen seconds learning it.
func agentTestManagerOn(t *testing.T, socketDir string, store RuntimeStore, provider AgentProvider,
	stopGrace time.Duration, projects ...*project.Project) (*Manager, *ProjectRuntimes) {
	t.Helper()

	runtimes := testRuntimes(t, socketDir)
	m, err := NewManager(ManagerOptions{
		Backends:          runtimes,
		Sockets:           runtimes.Sockets(),
		Projects:          newFakeProjects(projects...),
		Store:             store,
		Agent:             provider,
		AgentPoll:         testAgentPoll,
		AgentStartTimeout: testAgentStartTimeout,
		AgentStopGrace:    stopGrace,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, runtimes
}

// agentTestProject builds a project whose runtime path is a real temporary
// directory, so that the working directory an agent is checked against exists
// and is not the AgentMux source tree.
func agentTestProject(t *testing.T, label string) *project.Project {
	t.Helper()
	p := testProject(label)
	p.RuntimePath = t.TempDir()
	return p
}

// paneProcesses returns every process in a project's pane tree that is running
// the given executable.
//
// It reaches the terminal through the project's own socket rather than through
// the manager, and reads /proc itself. A test that verified the manager's view
// using the manager's code would pass for a manager that had the same mistake in
// both places, and the mistake this design is most exposed to is exactly that
// one: believing a process is there when it is not.
func paneProcesses(t *testing.T, runtimes *ProjectRuntimes, p *project.Project, executable string) []processRef {
	t.Helper()

	b := NewTmuxBackend(TmuxOptions{
		SocketPath: runtimes.Sockets().Path(p.ID),
		Logger:     discardLogger(),
	})
	pane, err := b.PaneProcess(context.Background(), p.SessionName())
	if err != nil {
		t.Fatalf("could not read the pane process of %q: %v", p.SessionName(), err)
	}

	scan, err := scanProcessTree()
	if err != nil {
		t.Fatalf("could not read the process table: %v", err)
	}
	target := cleanExecutablePath(executable)

	var out []processRef
	for _, pid := range scan.descendants(pane.PID) {
		ref, ok := scan.describe(pid)
		if !ok {
			continue
		}
		if ref.Executable == target {
			out = append(out, ref)
		}
	}
	return out
}

// processIsRunning reports whether a pid is still a process running the given
// executable.
//
// It is what a test asks where the terminal is gone and the process table is the
// only thing left: a destroyed pane cannot be read, but a process that outlived
// it is still in /proc.
func processIsRunning(t *testing.T, pid int, executable string) bool {
	t.Helper()
	if pid <= 0 {
		t.Fatalf("a test asked whether pid %d is running", pid)
	}
	scan, err := scanProcessTree()
	if err != nil {
		t.Fatalf("could not read the process table: %v", err)
	}
	ref, ok := scan.describe(pid)
	return ok && ref.Executable == cleanExecutablePath(executable)
}

// waitForAgentState polls the manager's own view until it reports the wanted
// state, and returns it.
func waitForAgentState(t *testing.T, m *Manager, projectID string, want AgentState, wait time.Duration) AgentStatus {
	t.Helper()

	deadline := time.Now().Add(wait)
	var last AgentStatus
	for time.Now().Before(deadline) {
		status, err := m.Agent(context.Background(), projectID)
		if err != nil {
			t.Fatalf("Agent returned an error: %v", err)
		}
		if status.State == want {
			return status
		}
		last = status
		time.Sleep(testAgentPoll)
	}
	t.Fatalf("the agent of %s is %q after %s, want %q (message %q)",
		projectID, last.State, wait, want, last.Message)
	return AgentStatus{}
}

// TestAgentStartRunsInTheProjectDirectory is the core claim of the phase: a
// command typed into a project's terminal starts a process, that process is
// found by identity rather than by name, and it is in the project's directory
// because it was measured to be there and not because it was asked to be.
func TestAgentStartRunsInTheProjectDirectory(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_start")
	spec := sleep.spec(300)
	m, runtimes := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}

	status, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}

	if status.State != AgentRunning {
		t.Errorf("the agent is %q, want %q", status.State, AgentRunning)
	}
	if !status.Running {
		t.Error("the agent is RUNNING but Running is false")
	}
	if status.Type != spec.Type {
		t.Errorf("the agent type is %q, want %q", status.Type, spec.Type)
	}
	if status.Executable != spec.Executable {
		t.Errorf("the agent executable is %q, want %q", status.Executable, spec.Executable)
	}
	if status.PID <= 0 {
		t.Fatalf("the agent has pid %d, so no process was found", status.PID)
	}

	want := canonicalPath(p.RuntimePath)
	if status.Dir != want {
		t.Errorf("the agent's directory is %q, want the project's %q", status.Dir, want)
	}
	if status.StartedAt == nil {
		t.Error("the agent's start time was not reported; it is read from the kernel")
	} else if age := time.Since(*status.StartedAt); age < 0 || age > time.Minute {
		t.Errorf("the agent's start time is %s ago, which is not this process's lifetime", age)
	}

	// The same facts, read from the process table by the test rather than
	// reported by the manager.
	found := paneProcesses(t, runtimes, p, spec.Executable)
	if len(found) != 1 {
		t.Fatalf("the pane holds %d processes running %s, want exactly 1", len(found), spec.Executable)
	}
	if found[0].PID != status.PID {
		t.Errorf("the manager reports pid %d and the process table says %d", status.PID, found[0].PID)
	}
	if found[0].Dir != want {
		t.Errorf("the running process is in %q, want the project's %q", found[0].Dir, want)
	}
}

// TestAgentStartIsIdempotent covers the arrangement the design exists to
// prevent: two agents in one project. A second start adopts the first, and the
// process table is checked so that "adopted" cannot mean "started again".
func TestAgentStartIsIdempotent(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_twice")
	spec := sleep.spec(300)
	m, runtimes := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	first, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("the first StartAgent returned an error: %v", err)
	}

	second, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("the second StartAgent returned an error: %v", err)
	}
	if second.PID != first.PID {
		t.Errorf("the second start reports pid %d, want the first agent's %d", second.PID, first.PID)
	}

	// A third call, because a second could have been answered from a record that
	// was written before anything was typed.
	if _, err := m.StartAgent(ctx, p.ID); err != nil {
		t.Fatalf("the third StartAgent returned an error: %v", err)
	}
	if found := paneProcesses(t, runtimes, p, spec.Executable); len(found) != 1 {
		t.Errorf("the pane holds %d agents after three starts, want exactly 1", len(found))
	}
}

// TestAgentStartRefusesAnAgentThatLeavesTheProject is the check that makes "the
// agent runs in the project's directory" a measurement rather than a promise.
//
// The command leaves the directory before starting the agent, which is what a
// launcher with a wrapper script, an inherited cd, or a mis-resolved path would
// do by accident. The runtime has to notice, refuse, and - because it is a
// coding agent in a directory nobody chose - stop it rather than leave it.
func TestAgentStartRefusesAnAgentThatLeavesTheProject(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	elsewhere := t.TempDir()
	p := agentTestProject(t, "p_agent_elsewhere")
	spec := sleep.specRunning(elsewhere, 300)
	m, runtimes := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}

	_, err := m.StartAgent(ctx, p.ID)
	if err == nil {
		t.Fatal("an agent that started outside the project's directory was accepted")
	}
	if !IsCode(err, CodeAgentWrongDirectory) {
		t.Fatalf("the error is %v, want code %q", err, CodeAgentWrongDirectory)
	}
	if !strings.Contains(err.Error(), canonicalPath(elsewhere)) {
		t.Errorf("the error does not say where the agent actually started: %v", err)
	}

	status, statusErr := m.Agent(ctx, p.ID)
	if statusErr != nil {
		t.Fatalf("Agent returned an error: %v", statusErr)
	}
	if status.State != AgentFailed {
		t.Errorf("the agent is %q after a refused start, want %q", status.State, AgentFailed)
	}
	if status.Running {
		t.Error("the agent is reported running after its start was refused")
	}

	// It was interrupted, not abandoned. The check is the process table, not the
	// manager's opinion of it.
	deadline := time.Now().Add(testAgentStopGrace)
	for time.Now().Before(deadline) {
		if len(paneProcesses(t, runtimes, p, spec.Executable)) == 0 {
			return
		}
		time.Sleep(testAgentPoll)
	}
	t.Errorf("the agent is still running in %s after its start was refused", elsewhere)
}

// TestAgentStartRefusesABusyTerminal covers the other way a launch goes wrong.
// A command typed at a terminal goes to whatever is in the foreground, so a
// runtime that typed one into a program would be typing at that program.
//
// The program holding the terminal is deliberately not the agent. A terminal
// already running the agent's own executable is the case StartAgent adopts
// rather than refuses - that is how an agent somebody started by hand, or one
// inherited from a previous server, is picked up - and this test is about the
// other case.
func TestAgentStartRefusesABusyTerminal(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)
	blocker := requireBlocker(t)

	idle := agentTestProject(t, "p_agent_idle")
	busy := agentTestProject(t, "p_agent_busy")
	spec := sleep.spec(300)
	m, _ := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, idle, busy)

	for _, p := range []*project.Project{idle, busy} {
		if _, err := m.Start(ctx, p.ID); err != nil {
			t.Fatalf("could not start the runtime for %s: %v", p.ID, err)
		}
	}

	// The shell the sessions run is read from the session rather than assumed.
	// The idle check compares the pane's foreground process against the
	// configured shell, so a test that guessed the wrong name would be testing
	// the refusal for the wrong reason.
	rt, err := m.requireRunning(ctx, idle.ID)
	if err != nil {
		t.Fatalf("the runtime for %s is not running: %v", idle.ID, err)
	}
	pane, err := m.paneProcess(ctx, rt)
	if err != nil {
		t.Fatalf("could not read the pane process: %v", err)
	}
	m.shell = pane.Command

	// An idle terminal is accepted, which is what makes the refusal below a
	// statement about a busy one.
	if _, err := m.StartAgent(ctx, idle.ID); err != nil {
		t.Fatalf("StartAgent refused an idle terminal: %v", err)
	}

	// Now make the other project's terminal busy with a program that is not the
	// agent. It is typed directly, so this is the state a user's own command
	// would leave the terminal in.
	if err := m.Launch(ctx, busy.ID, blocker.Path); err != nil {
		t.Fatalf("could not type into the terminal of %s: %v", busy.ID, err)
	}
	waitForPaneCommand(t, paneBackend(t, m, busy), busy.SessionName(), blocker.Name)

	_, err = m.StartAgent(ctx, busy.ID)
	if err == nil {
		t.Fatal("an agent was started into a terminal running another program")
	}
	if !IsCode(err, CodeAgentTerminalBusy) {
		t.Fatalf("the error is %v, want code %q", err, CodeAgentTerminalBusy)
	}
	if !strings.Contains(err.Error(), blocker.Name) {
		t.Errorf("the error does not name the program holding the terminal: %v", err)
	}

	// Nothing was typed, so the terminal is still running what it was.
	if got := paneCommand(t, paneBackend(t, m, busy), busy.SessionName()); got != blocker.Name {
		t.Errorf("the terminal is now running %q, want %q: the refused launch typed something", got, blocker.Name)
	}
}

// paneBackend is a handle on a project's terminal that does not go through the
// manager, so that a test can look at what the terminal is really running.
func paneBackend(t *testing.T, m *Manager, p *project.Project) *TmuxBackend {
	t.Helper()
	return NewTmuxBackend(TmuxOptions{
		SocketPath: m.sockets.Path(p.ID),
		Logger:     discardLogger(),
	})
}

// TestAgentCannotStartWithoutATerminal pins the model that an agent has nowhere
// to run without a runtime: the refusal is explicit, and it is not a queue.
func TestAgentCannotStartWithoutATerminal(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_no_runtime")
	m, _ := agentTestManager(t, newFakeStore(), fakeAgent{spec: sleep.spec(300)}, p)

	_, err := m.StartAgent(ctx, p.ID)
	if err == nil {
		t.Fatal("an agent was started in a project whose runtime was never started")
	}
	if !IsCode(err, CodeNotRunning) {
		t.Errorf("the error is %v, want code %q", err, CodeNotRunning)
	}

	// And the state it reports is the honest one: an agent could be started, and
	// none is running. These are two different facts and a client needs both.
	status, err := m.Agent(ctx, p.ID)
	if err != nil {
		t.Fatalf("Agent returned an error: %v", err)
	}
	if !status.Available {
		t.Errorf("the agent is reported unavailable before its runtime exists: %s", status.Message)
	}
	if status.Running {
		t.Error("an agent is reported running in a project with no runtime")
	}
}

// TestAgentWithoutAProviderIsUnavailable checks the case the server logs about
// at startup: no agent program on this machine. Every agent endpoint has to say
// so rather than reporting a stopped agent, which would read as "start it".
func TestAgentWithoutAProviderIsUnavailable(t *testing.T) {
	ctx := context.Background()

	p := agentTestProject(t, "p_agent_none")
	m, _ := agentTestManager(t, newFakeStore(), nil, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	if _, err := m.StartAgent(ctx, p.ID); !IsCode(err, CodeAgentUnavailable) {
		t.Errorf("StartAgent returned %v, want code %q", err, CodeAgentUnavailable)
	}
	if _, err := m.StopAgent(ctx, p.ID); !IsCode(err, CodeAgentUnavailable) {
		t.Errorf("StopAgent returned %v, want code %q", err, CodeAgentUnavailable)
	}

	status, err := m.Agent(ctx, p.ID)
	if err != nil {
		t.Fatalf("Agent returned an error: %v", err)
	}
	if status.Available {
		t.Error("an agent is reported available on a server with no agent program")
	}
	if status.Message == "" {
		t.Error("an unavailable agent must say why")
	}
}

// TestAgentProviderFailureIsReportedNotSwallowed checks that a launcher that
// cannot resolve its program is a refusal with a reason, and not an empty agent
// that a client would render as "stopped".
func TestAgentProviderFailureIsReportedNotSwallowed(t *testing.T) {
	ctx := context.Background()

	p := agentTestProject(t, "p_agent_broken")
	broken := fakeAgent{err: errors.New("claude was not found on PATH")}
	m, _ := agentTestManager(t, newFakeStore(), broken, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	_, err := m.StartAgent(ctx, p.ID)
	if !IsCode(err, CodeAgentUnavailable) {
		t.Fatalf("StartAgent returned %v, want code %q", err, CodeAgentUnavailable)
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("the error does not carry the launcher's reason: %v", err)
	}

	status, err := m.Agent(ctx, p.ID)
	if err != nil {
		t.Fatalf("Agent returned an error: %v", err)
	}
	if status.Available {
		t.Error("an agent is reported available while its launcher cannot resolve it")
	}
	if !strings.Contains(status.Message, "not found on PATH") {
		t.Errorf("the status message is %q, want the launcher's reason", status.Message)
	}
}

// TestAgentStopInterruptsTheAgentAndKeepsTheTerminal covers both halves of the
// phase's model at once: the agent is interrupted, and the runtime it ran in is
// untouched. A stop that ended the terminal would make the scrollback of what
// the agent did unavailable, which is the point of hosting it in tmux.
func TestAgentStopInterruptsTheAgentAndKeepsTheTerminal(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_stop")
	spec := sleep.spec(300)
	m, runtimes := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	started, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}

	stopped, err := m.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StopAgent returned an error: %v", err)
	}
	if stopped.State != AgentStopped {
		t.Errorf("the agent is %q after a stop, want %q", stopped.State, AgentStopped)
	}
	if stopped.Running {
		t.Error("the agent is reported running after a stop")
	}
	if !stopped.Requested {
		t.Error("the agent does not record that AgentMux asked it to stop")
	}
	if stopped.PID != 0 {
		t.Errorf("the agent has pid %d after stopping, want it cleared", stopped.PID)
	}

	if found := paneProcesses(t, runtimes, p, spec.Executable); len(found) != 0 {
		t.Errorf("%d agents are still running after StopAgent", len(found))
	}

	// The terminal survived, with its shell back at a prompt.
	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.State != StateRunning {
		t.Errorf("the runtime is %q after the agent stopped, want %q", rt.State, StateRunning)
	}
	if !rt.SessionAlive {
		t.Error("the session did not survive the agent stopping")
	}
	if rt.Agent == nil || rt.Agent.State != AgentStopped {
		t.Errorf("the runtime reports its agent as %+v, want a stopped one", rt.Agent)
	}

	// The agent can be started again in the same terminal, which is what makes
	// this a lifecycle rather than a one-shot.
	restarted, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("the agent could not be started again after a stop: %v", err)
	}
	if restarted.PID == started.PID {
		t.Errorf("the restarted agent has the same pid %d; nothing was started", restarted.PID)
	}
	if _, err := m.StopAgent(ctx, p.ID); err != nil {
		t.Fatalf("the restarted agent could not be stopped: %v", err)
	}
}

// interruptIgnorer writes a program that declines the interrupt a stop delivers,
// and returns a spec that starts it.
//
// The stop is a Ctrl-C typed into the terminal, which becomes SIGINT for
// whatever holds the terminal, and a program can decline it. Real Claude Code
// does: it holds its terminal in raw mode, so the byte never becomes a signal at
// all, and its interface reads it as a keystroke. This reproduces the part of
// that which the runtime can see - a process that is still there afterwards -
// without needing a program that has a terminal mode.
func interruptIgnorer(t *testing.T, shell agentProgram, dir string) AgentSpec {
	t.Helper()
	script := filepath.Join(dir, "ignore-interrupt.sh")
	const body = "#!/bin/sh\n" +
		"# Decline the interrupt. The sleep children inherit the ignored\n" +
		"# disposition across exec, so the whole process group declines it.\n" +
		"trap '' INT\n" +
		"while : ; do sleep 0.2 ; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("could not write the agent program %s: %v", script, err)
	}
	return AgentSpec{
		Type:       "testagent",
		Version:    "1.0.0",
		Command:    shellQuote(shell.Path) + " " + shellQuote(script),
		Executable: shell.Path,
	}
}

// TestAgentThatDeclinesTheInterruptIsReportedAsStillRunning covers the answer a
// stop can have that is not "stopped".
//
// The interruption is delivered to the terminal and the program holding it
// decides what to do with it. When it declines, the honest report has two parts:
// a stop was asked for, and the agent is still there. The record of the request
// has to survive the observation that the process is running - those are two
// facts and not one - or a client is told that nobody asked, which is the
// opposite of what happened.
func TestAgentThatDeclinesTheInterruptIsReportedAsStillRunning(t *testing.T) {
	ctx := context.Background()
	shell := requireProgram(t, "sh")

	p := agentTestProject(t, "p_agent_interrupt_declined")
	spec := interruptIgnorer(t, shell, p.RuntimePath)
	// The grace is shortened because this stop is going to be declined, and the
	// runtime waits the whole of it before it concludes that.
	m, runtimes := agentTestManagerOn(t, uniqueSocketDir(t), newFakeStore(),
		fakeAgent{spec: spec}, 2*time.Second, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	started, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}
	if !started.Running {
		t.Fatalf("the agent did not start: %+v", started)
	}

	declined, err := m.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StopAgent returned an error rather than an answer: %v", err)
	}

	if !processIsRunning(t, started.PID, spec.Executable) {
		t.Skip("this shell did not survive the interrupt, so a declined stop cannot be exercised here")
	}

	if !declined.Running {
		t.Errorf("the agent is still running and the runtime reports %q", declined.State)
	}
	if declined.State != AgentRunning {
		t.Errorf("the agent declined the interrupt and the runtime reports %q, want %q",
			declined.State, AgentRunning)
	}
	if !declined.Requested {
		t.Error("a stop was asked for and the status does not record that it was")
	}
	if declined.Message == "" {
		t.Error("the agent declined the interrupt and the status has nothing to say about it")
	}
	if declined.PID != started.PID {
		t.Errorf("the reported pid changed from %d to %d while the process never ended",
			started.PID, declined.PID)
	}

	// The terminal is untouched, so the scrollback survives a stop that did not
	// take, which is the whole reason a stop does not destroy it.
	if found := paneProcesses(t, runtimes, p, spec.Executable); len(found) != 1 {
		t.Errorf("%d agents are running after a declined stop, want the one that declined it", len(found))
	}
	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.State != StateRunning || !rt.SessionAlive {
		t.Errorf("the runtime is %q (session alive=%v) after a declined stop, want a running one",
			rt.State, rt.SessionAlive)
	}

	// A second stop is answered the same way rather than differently, so a
	// client that retries is told the same truth twice.
	again, err := m.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("the second StopAgent returned an error: %v", err)
	}
	if !again.Running || !again.Requested {
		t.Errorf("the second stop reports running=%v requested=%v, want both true", again.Running, again.Requested)
	}
}

// TestAgentStopWithNothingRunningIsNotAnError covers the idempotence of stop,
// which a client that reloaded a page depends on.
func TestAgentStopWithNothingRunningIsNotAnError(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_stop_idle")
	m, _ := agentTestManager(t, newFakeStore(), fakeAgent{spec: sleep.spec(300)}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	status, err := m.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StopAgent on a project with no agent returned an error: %v", err)
	}
	if status.State != AgentStopped {
		t.Errorf("the agent is %q, want %q", status.State, AgentStopped)
	}
	if status.Running {
		t.Error("an agent is reported running after being stopped when none was")
	}
}

// TestAgentThatEndsOnItsOwnIsReportedAsExited covers the state that exists
// because AgentMux cannot know why an agent ended: the shell inside the terminal
// is the process that reaps it, so its exit status is known to the shell and to
// nothing else. What is reported is the fact - it is gone, and nothing here
// asked it to go - and not a cause.
func TestAgentThatEndsOnItsOwnIsReportedAsExited(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_exit")
	// Long enough that the start observes it and short enough that the test does
	// not wait for it.
	spec := sleep.spec(1)
	m, _ := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	started, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}
	if started.State != AgentRunning {
		t.Fatalf("the agent is %q straight after starting, want %q", started.State, AgentRunning)
	}

	// The watcher notices, and it has to notice by itself: nothing here calls
	// Stop, and nothing here sets a state. Reading the status re-observes the
	// process table, but a runtime that only ever re-observed would go on
	// reporting the agent from its record, so reaching EXITED means the watcher
	// decided it.
	status := waitForAgentState(t, m, p.ID, AgentExited, testAgentStopGrace)
	if status.Requested {
		t.Error("an agent that ended on its own records that AgentMux asked it to stop")
	}
	if status.Running {
		t.Error("the agent is reported running after it ended")
	}
	if status.Message == "" {
		t.Error("an unexplained exit must say that it was not requested")
	}
	if status.ExitedAt == nil {
		t.Error("the moment the agent was found gone was not recorded")
	}
	if status.PID != 0 {
		t.Errorf("the agent still reports pid %d after it ended", status.PID)
	}

	// The runtime is still RUNNING. The agent is a program inside the terminal,
	// not the terminal.
	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.State != StateRunning {
		t.Errorf("the runtime is %q after its agent exited, want %q", rt.State, StateRunning)
	}
}

// TestAgentSurvivesAServerRestart is the persistence property, and it is the one
// that makes the runtime worth having: the agent is not this server's child, so
// a server that goes away and comes back finds it still running.
//
// The new manager has never seen the agent. It is found by looking at the
// process table, and the start time it reports is the kernel's, not the moment
// it was rediscovered.
func TestAgentSurvivesAServerRestart(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_restart")
	spec := sleep.spec(300)
	store := newFakeStore()
	socketDir := uniqueSocketDir(t)

	first, runtimes := agentTestManagerOn(t, socketDir, store, fakeAgent{spec: spec}, testAgentStopGrace, p)
	if _, err := first.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	before, err := first.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("could not close the first manager: %v", err)
	}

	// The agent is still there, and the terminal it is in is still there, with
	// nothing of AgentMux running.
	if found := paneProcesses(t, runtimes, p, spec.Executable); len(found) != 1 {
		t.Fatalf("the agent did not survive the server stopping: %d processes found", len(found))
	}

	second, _ := agentTestManagerOn(t, socketDir, store, fakeAgent{spec: spec}, testAgentStopGrace, p)
	if _, err := second.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}

	after, err := second.Agent(ctx, p.ID)
	if err != nil {
		t.Fatalf("Agent returned an error after the restart: %v", err)
	}
	if after.State != AgentRunning {
		t.Fatalf("the agent is %q after the restart, want %q (message %q)",
			after.State, AgentRunning, after.Message)
	}
	if after.PID != before.PID {
		t.Errorf("the agent has pid %d after the restart, want the original %d", after.PID, before.PID)
	}
	// The start time is the kernel's rather than AgentMux's: it comes from the
	// process's own start ticks, so a server that has just met the process
	// reports when the process started and not when it was noticed.
	//
	// It is compared with a tolerance because the derivation rests on
	// /proc/uptime, which the kernel reports in steps of ten milliseconds. The
	// boot time is derived once per process, so two managers in this one agree
	// exactly; a restart that really replaced the process would derive it again
	// from the same coarse source and could land one step away.
	if after.StartedAt == nil || before.StartedAt == nil {
		t.Fatal("the agent's start time was not reported on one side of the restart")
	}
	if delta := after.StartedAt.Sub(*before.StartedAt); delta < -20*time.Millisecond || delta > 20*time.Millisecond {
		t.Errorf("the start time moved by %s across the restart: %s then %s",
			delta, before.StartedAt, after.StartedAt)
	}

	// And it is stopped through the new server, in the terminal the old one
	// created.
	stopped, err := second.StopAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StopAgent returned an error after the restart: %v", err)
	}
	if stopped.State != AgentStopped {
		t.Errorf("the agent is %q, want %q", stopped.State, AgentStopped)
	}
	if found := paneProcesses(t, runtimes, p, spec.Executable); len(found) != 0 {
		t.Errorf("%d agents survived being stopped through the restarted server", len(found))
	}
}

// TestAgentsAreIsolatedBetweenProjects checks that one project's agent is not
// another's. Each runtime is its own tmux server on its own socket, so this is
// also a statement about the sockets: a search that walked the whole machine
// instead of one pane's tree would find the other project's agent and report
// every project as running one.
func TestAgentsAreIsolatedBetweenProjects(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	a := agentTestProject(t, "p_agent_iso_a")
	b := agentTestProject(t, "p_agent_iso_b")
	spec := sleep.spec(300)
	m, runtimes := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, a, b)

	for _, p := range []*project.Project{a, b} {
		if _, err := m.Start(ctx, p.ID); err != nil {
			t.Fatalf("could not start the runtime for %s: %v", p.ID, err)
		}
	}

	startedA, err := m.StartAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error for the first project: %v", err)
	}

	// B has a runtime and no agent, and both of those are reported.
	statusB, err := m.Agent(ctx, b.ID)
	if err != nil {
		t.Fatalf("Agent returned an error for the second project: %v", err)
	}
	if statusB.Running {
		t.Error("the second project reports an agent running when only the first has one")
	}
	if statusB.PID != 0 {
		t.Errorf("the second project reports pid %d for an agent it does not have", statusB.PID)
	}
	if !statusB.Available {
		t.Errorf("the second project reports no agent is available: %s", statusB.Message)
	}
	rtB, err := m.Runtime(ctx, b.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error for the second project: %v", err)
	}
	if rtB.State != StateRunning {
		t.Errorf("the second project's runtime is %q, want %q", rtB.State, StateRunning)
	}

	// The process tables say the same thing.
	if found := paneProcesses(t, runtimes, b, spec.Executable); len(found) != 0 {
		t.Errorf("the second project's terminal holds %d agents, want none", len(found))
	}

	startedB, err := m.StartAgent(ctx, b.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error for the second project: %v", err)
	}
	if startedB.PID == startedA.PID {
		t.Errorf("both projects report pid %d; they are the same process", startedA.PID)
	}

	// Stopping one leaves the other alone, which is what isolation has to mean
	// to be worth anything.
	if _, err := m.StopAgent(ctx, a.ID); err != nil {
		t.Fatalf("StopAgent returned an error: %v", err)
	}
	stillB, err := m.Agent(ctx, b.ID)
	if err != nil {
		t.Fatalf("Agent returned an error for the second project: %v", err)
	}
	if !stillB.Running || stillB.PID != startedB.PID {
		t.Errorf("the second project's agent is %+v after the first was stopped, want pid %d running",
			stillB, startedB.PID)
	}
}

// TestAgentIsReportedInsideTheRuntime checks the nesting a client renders: the
// agent travels with the runtime it belongs to, so a panel does not have to make
// two requests that can disagree.
func TestAgentIsReportedInsideTheRuntime(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_nested")
	spec := sleep.spec(300)
	m, _ := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}

	// Before the agent starts, the runtime says an agent could be started and
	// none is running. A server with Claude Code installed must not report that
	// no agent is configured.
	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.Agent == nil {
		t.Fatal("the runtime does not describe its agent at all")
	}
	if !rt.Agent.Available {
		t.Errorf("the runtime reports no agent available on a server that has one: %s", rt.Agent.Message)
	}
	if rt.Agent.Type != spec.Type {
		t.Errorf("the runtime reports agent type %q, want %q", rt.Agent.Type, spec.Type)
	}
	if rt.Agent.Running {
		t.Error("the runtime reports an agent running before one was started")
	}
	if rt.Agent.State != AgentStopped {
		t.Errorf("the runtime reports its agent as %q, want %q", rt.Agent.State, AgentStopped)
	}

	if _, err := m.StartAgent(ctx, p.ID); err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}
	rt, err = m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.Agent == nil || !rt.Agent.Running {
		t.Fatalf("the runtime reports its agent as %+v after one was started", rt.Agent)
	}
	if rt.Agent.PID <= 0 {
		t.Errorf("the runtime's agent has pid %d", rt.Agent.PID)
	}
}

// TestAgentIsGoneAfterItsRuntimeIsDestroyed checks that destroying a terminal
// takes its agent with it, and that the state left behind says so rather than
// reporting an agent that no longer has anywhere to be.
func TestAgentIsGoneAfterItsRuntimeIsDestroyed(t *testing.T) {
	ctx := context.Background()
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_destroy")
	spec := sleep.spec(300)
	m, _ := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	started, err := m.StartAgent(ctx, p.ID)
	if err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	// The pane went with the terminal, so the process table is what is left to
	// ask. Destroying a session ends the programs running in it, and this checks
	// that the agent was really one of them rather than a process left behind
	// with no terminal to be seen in.
	deadline := time.Now().Add(testAgentStopGrace)
	for time.Now().Before(deadline) && processIsRunning(t, started.PID, spec.Executable) {
		time.Sleep(testAgentPoll)
	}
	if processIsRunning(t, started.PID, spec.Executable) {
		t.Errorf("the agent (pid %d) outlived the terminal it was running in", started.PID)
	}

	// A project with no runtime answers for its agent without a terminal to ask.
	status, err := m.Agent(ctx, p.ID)
	if err != nil {
		t.Fatalf("Agent returned an error for a project with no runtime: %v", err)
	}
	if status.Running {
		t.Error("an agent is reported running after its terminal was destroyed")
	}
	if !status.Available {
		t.Errorf("no agent is reported available after its terminal was destroyed: %s", status.Message)
	}
	if status.State != AgentStopped {
		t.Errorf("the agent is %q with no terminal, want %q", status.State, AgentStopped)
	}
}

// TestFindProcessRunningMatchesOnlyTheRealExecutable is the unit-level statement
// of the measurement the whole design rests on.
//
// Measured on a real Claude Code install, tmux reports the pane's foreground
// process as "2.1.274" - the file name of the versioned binary the launcher
// symlinks to - and not "claude". A runtime that recognised its agent by name
// would therefore report a running agent as absent. Identity is the executable
// path, and this checks that nothing else matches.
func TestFindProcessRunningMatchesOnlyTheRealExecutable(t *testing.T) {
	sleep := requireSleep(t)

	p := agentTestProject(t, "p_agent_identity")
	spec := sleep.spec(300)
	m, runtimes := agentTestManager(t, newFakeStore(), fakeAgent{spec: spec}, p)

	ctx := context.Background()
	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("could not start the runtime: %v", err)
	}
	if _, err := m.StartAgent(ctx, p.ID); err != nil {
		t.Fatalf("StartAgent returned an error: %v", err)
	}

	rt, err := m.requireRunning(ctx, p.ID)
	if err != nil {
		t.Fatalf("the runtime is not running: %v", err)
	}
	pane, err := m.paneProcess(ctx, rt)
	if err != nil {
		t.Fatalf("could not read the pane process: %v", err)
	}
	if pane.Command == "" {
		t.Fatal("the pane reported no foreground process")
	}

	// The foreground process's name is not the program's name, which is the
	// reason identity is used. Asserting the general fact here means a machine
	// where they happen to agree does not hide the case where they do not.
	t.Logf("the pane's foreground process is reported as %q while the agent is %s",
		pane.Command, spec.Executable)

	if _, found, err := findProcessRunning(pane.PID, spec.Executable); err != nil || !found {
		t.Errorf("the agent was not found by its executable: found=%v err=%v", found, err)
	}
	for _, wrong := range []string{
		filepath.Base(spec.Executable),
		pane.Command,
		spec.Executable + "-not-this",
		"",
	} {
		if _, found, _ := findProcessRunning(pane.PID, wrong); found {
			t.Errorf("a process matched the executable %q, which is not what the agent runs", wrong)
		}
	}
	if found := paneProcesses(t, runtimes, p, spec.Executable); len(found) != 1 {
		t.Errorf("the pane holds %d matching processes, want 1", len(found))
	}
}
