package httpapi

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
)

// This file covers the agent half of the API: the three endpoints under
// /api/projects/{id}/runtime/agent, and the status codes their failures carry.
//
// What is deliberately not here is the agent lifecycle itself - launching,
// recognising the process, noticing an exit, declining an interrupt. That is
// tested in the session package against real tmux, where a real process can be
// started and a real process table read, and in cmd/server against the real
// Claude Code CLI. Asserting it here would mean re-implementing the process
// table in a double and testing the double.
//
// So the doubles in this file answer the two questions the handlers actually
// ask - what the runtime says, and what the terminal is running - and nothing
// else.

// pinnedAgent is an AgentProvider that answers with what the test chose.
type pinnedAgent struct {
	spec session.AgentSpec
	err  error
}

func (a pinnedAgent) Spec(context.Context, session.AgentLaunch) (session.AgentSpec, error) {
	return a.spec, a.err
}

// testAgentSpec is a spec that names an executable no machine has.
//
// It is a real absolute path rather than a bare name, because the runtime
// matches a running process against it and a relative path would be matched
// against nothing. Nothing is at it, so no process is ever recognised, which is
// how a test reaches the "started and never appeared" branch without starting
// anything.
var testAgentSpec = session.AgentSpec{
	Type:       "claude",
	Version:    "9.9.9",
	Command:    "'/nonexistent/agentmux-test/claude'",
	Executable: "/nonexistent/agentmux-test/claude",
}

// pinnedClaude is an AgentResolver that reports what the test chose.
//
// It is on the server, not on the manager, so it is the capability report that
// varies here; the manager's own provider is pinnedAgent.
type pinnedClaude struct{ inst claude.Installation }

func (p pinnedClaude) Resolve(context.Context) claude.Installation { return p.inst }

// agentStatusResponse is the body of the three agent endpoints.
type agentStatusResponse struct {
	Agent session.AgentStatus `json:"agent"`
}

// callAgent performs one of the three agent calls and fails on a status the
// test did not expect.
func (h *harness) callAgent(t *testing.T, method, projectID, action string, want int) session.AgentStatus {
	t.Helper()
	path := "/api/projects/" + projectID + "/runtime/agent"
	if action != "" {
		path += "/" + action
	}
	recorder := h.call(method, path, "")
	if recorder.Code != want {
		t.Fatalf("%s %s: status = %d, want %d; body was %s",
			method, path, recorder.Code, want, recorder.Body.String())
	}
	return decode[agentStatusResponse](t, recorder).Agent
}

// ---------------------------------------------------------------------------
// Reading the agent
// ---------------------------------------------------------------------------

// TestAgentOnAProjectWithNoRuntimeIsStoppedNotMissing pins the shape of the
// answer a client polls before it has started anything.
//
// The runtime does not exist and no agent was ever started, and the honest
// answer is still an agent description: "there is nothing" is a state, not a
// missing resource. A client that had to tell 404 from STOPPED would need to
// know whether a runtime had ever been created just to decide what to render.
func TestAgentOnAProjectWithNoRuntimeIsStoppedNotMissing(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")

	agent := h.callAgent(t, http.MethodGet, projectID, "", http.StatusOK)

	if agent.State != session.AgentStopped {
		t.Errorf("state = %q, want %q", agent.State, session.AgentStopped)
	}
	if agent.Running {
		t.Error("no runtime exists and the agent is reported as running")
	}
	// The agent can be started here, and saying so is what lets a UI offer the
	// button on a project whose runtime has not been started yet.
	if !agent.Available {
		t.Errorf("available = false with message %q, want true: a spec resolves", agent.Message)
	}
	if agent.Type != testAgentSpec.Type {
		t.Errorf("type = %q, want %q", agent.Type, testAgentSpec.Type)
	}
	if agent.Version != testAgentSpec.Version {
		t.Errorf("version = %q, want %q", agent.Version, testAgentSpec.Version)
	}
}

// TestAgentOnAProjectThatCannotBeFoundIsNotAnAgentState is the other half of
// the distinction above: an unknown project is a 404, and a project with no
// agent is a 200. Collapsing them would make a typo in an id look like a
// stopped agent.
func TestAgentOnAProjectThatCannotBeFoundIsNotAnAgentState(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	recorder := h.call(http.MethodGet, "/api/projects/p_0000000000000000000a/runtime/agent", "")
	h.wantError(t, recorder, http.StatusNotFound, "project_not_found")
}

// TestAgentReportsUnavailableWhenTheServerHasNoAgentProgram covers the machine
// with no Claude Code on it.
//
// The endpoint answers rather than failing, because "this server cannot host an
// agent" is exactly what a client needs to know, and it arrives as a state with
// a message a user can act on rather than as a 500. It is a 200 here because
// the read succeeded: what it read is that there is no agent.
func TestAgentReportsUnavailableWhenTheServerHasNoAgentProgram(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")

	agent := h.callAgent(t, http.MethodGet, projectID, "", http.StatusOK)

	if agent.Available {
		t.Error("available = true on a server with no agent configured")
	}
	if agent.Message == "" {
		t.Error("the agent is reported unavailable with no explanation")
	}
	if agent.Running {
		t.Error("the agent is reported as running on a server that cannot start one")
	}
}

// TestAgentEndpointsAreRefusedWhereRuntimesCannotRun is the Windows-native
// server: the runtime itself is unavailable, and every agent call says so
// rather than reporting a stopped agent on a machine that will never host one.
func TestAgentEndpointsAreRefusedWhereRuntimesCannotRun(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}})
	projectID, _ := h.registerProject(t, "checkout-service")

	base := "/api/projects/" + projectID + "/runtime/agent"
	h.wantError(t, h.call(http.MethodGet, base, ""),
		http.StatusServiceUnavailable, session.CodeUnavailable)
	h.wantError(t, h.call(http.MethodPost, base+"/start", ""),
		http.StatusServiceUnavailable, session.CodeUnavailable)
	h.wantError(t, h.call(http.MethodPost, base+"/stop", ""),
		http.StatusServiceUnavailable, session.CodeUnavailable)
}

// ---------------------------------------------------------------------------
// Starting the agent
// ---------------------------------------------------------------------------

// TestStartingAnAgentOnAStoppedProjectBringsTheRuntimeUp is the phase's
// replacement for the rule that an agent needs a terminal to be in.
//
// The rule has not been dropped, it has moved. An agent with no terminal is
// still a process nobody can see, interrupt, or read - so the call does not
// launch one into nothing. It brings the terminal up first and then launches
// into it, which is what makes one request enough to go from a stopped project
// to a running agent.
//
// The launch itself fails here, because the pinned agent names an executable no
// machine has - and the failure takes the terminal back with it, which is the
// other half of the same rule. So the evidence is the *sequence*: a session was
// created, and then it was stopped. A project that had simply been refused
// would show neither.
func TestStartingAnAgentOnAStoppedProjectBringsTheRuntimeUp(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")

	if created := h.backend.createdCount(); created != 0 {
		t.Fatalf("the project already has %d runtime(s); want none before the call", created)
	}

	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start", "")
	if recorder.Code == http.StatusConflict {
		t.Fatalf("a stopped runtime was refused instead of started: %s", recorder.Body)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 from a launch whose agent never appeared: %s",
			recorder.Code, recorder.Body)
	}

	if created := h.backend.createdCount(); created != 1 {
		t.Errorf("the runtime was created %d time(s); want once: "+
			"an agent asked for on a stopped project starts its terminal", created)
	}
	if after := h.runtimeState(t, projectID); after.State != session.StateStopped {
		t.Errorf("runtime state after a failed launch = %s; want STOPPED: "+
			"a runtime this call started is taken back when the launch fails", after.State)
	}
}

// TestStartingAnAgentWithNoCoordinatorSaysSo is the branch a server built
// without one takes.
//
// There is deliberately no fallback that launches the agent anyway: an agent
// running with nothing observing it behaves exactly like an agent that has
// nothing to say, and telling those two apart is the whole of what this phase
// added.
func TestStartingAnAgentWithNoCoordinatorSaysSo(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{spec: testAgentSpec},
		runtimeAvailable: true,
		withoutAgents:    true,
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)

	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start", "")
	h.wantError(t, recorder, http.StatusServiceUnavailable, CodeInternal)
}

// TestStartingAnAgentOnAServerThatCannotHostOneIsUnavailable covers the status
// code a client acts on, which is why it is asserted here rather than only in
// the manager.
//
// A structural inability to host an agent is a 503, not a 500: the message says
// what to do about it, and a client that saw 500 would report a bug where the
// honest answer is "not on this machine".
func TestStartingAnAgentOnAServerThatCannotHostOneIsUnavailable(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)

	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start", "")
	h.wantError(t, recorder, http.StatusServiceUnavailable, session.CodeAgentUnavailable)
}

// TestStartingAnAgentRefusesATerminalInAnotherDirectory is the safety rule.
//
// A terminal attached to the wrong directory is worse than no terminal: an
// agent started there would read and write files in a directory nobody chose.
// The refusal happens before anything is typed, so nothing has to be undone.
func TestStartingAnAgentRefusesATerminalInAnotherDirectory(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{spec: testAgentSpec},
		runtimeAvailable: true,
		shell:            "/bin/bash",
		pane:             session.PaneProcess{PID: 4242, Command: "bash", Dir: filepath.Join(t.TempDir(), "elsewhere")},
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)

	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start", "")
	body := h.wantError(t, recorder, http.StatusUnprocessableEntity, session.CodeAgentWrongDirectory)

	// The message is the whole point of the refusal: a user reading it has to be
	// able to see which two directories disagreed.
	if !strings.Contains(body.Error.Message, "elsewhere") {
		t.Errorf("the refusal does not name the directory the terminal is in: %q", body.Error.Message)
	}
}

// TestStartingAnAgentRefusesATerminalRunningSomethingElse is the other
// pre-launch check.
//
// The command is delivered as terminal input, so it goes to whatever has the
// foreground. Typing `claude` at a terminal running an editor would put those
// bytes inside the editor; the launch is refused rather than made and hoped
// for.
func TestStartingAnAgentRefusesATerminalRunningSomethingElse(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{spec: testAgentSpec},
		runtimeAvailable: true,
		shell:            "/bin/bash",
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)

	// The pane is in the right directory, so the only thing wrong with it is
	// what it is running.
	h.backend.setPane(session.PaneProcess{
		PID:     4242,
		Command: "vim",
		Dir:     h.projectRuntimePath(t, projectID),
	})

	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start", "")
	body := h.wantError(t, recorder, http.StatusConflict, session.CodeAgentTerminalBusy)
	if !strings.Contains(body.Error.Message, "vim") {
		t.Errorf("the refusal does not name the program holding the terminal: %q", body.Error.Message)
	}
}

// TestStartingAnAgentThatNeverAppearsReportsALaunchFailure covers the wait.
//
// The command is typed and no such process ever turns up. The runtime does not
// guess that it worked, and it does not read the terminal to find out: it waits
// for the process it named, and reports a failure when the process is not
// there.
func TestStartingAnAgentThatNeverAppearsReportsALaunchFailure(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:             pinnedAgent{spec: testAgentSpec},
		runtimeAvailable:  true,
		shell:             "/bin/bash",
		agentStartTimeout: 300 * time.Millisecond,
		agentPoll:         25 * time.Millisecond,
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)
	h.backend.setPane(session.PaneProcess{
		PID:     4242,
		Command: "bash",
		Dir:     h.projectRuntimePath(t, projectID),
	})

	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start", "")
	h.wantError(t, recorder, http.StatusInternalServerError, session.CodeAgentLaunchFailed)

	// The command really was typed. Without this the test would pass on a
	// handler that refused earlier for some other reason.
	sessionName := project.SessionNameFor(projectID)
	h.backend.mu.Lock()
	launched := append([]string(nil), h.backend.launched[sessionName]...)
	h.backend.mu.Unlock()
	if len(launched) != 1 || launched[0] != testAgentSpec.Command {
		t.Errorf("the runtime typed %q, want exactly [%q]", launched, testAgentSpec.Command)
	}
}

// TestStartingAnAgentWhenTheAgentProgramIsMissingIsUnavailable covers the
// provider failing rather than the spec being empty: Claude Code is configured
// but cannot be resolved, and the message is the launcher's.
func TestStartingAnAgentWhenTheAgentProgramIsMissingIsUnavailable(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{err: errors.New("claude was not found on PATH; set terminal.claudeBinary to its full path")},
		runtimeAvailable: true,
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)

	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start", "")
	body := h.wantError(t, recorder, http.StatusServiceUnavailable, session.CodeAgentUnavailable)

	// The reason has to survive: "not found on PATH" and "this build has no
	// agent support" are the same code and different fixes.
	if !strings.Contains(body.Error.Message, "PATH") {
		t.Errorf("the refusal lost the launcher's reason: %q", body.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// Stopping the agent
// ---------------------------------------------------------------------------

// requireProcTable skips a test whose path reads the process table.
//
// "Is the agent still running" is answered from /proc, which is the only honest
// answer available and is Linux-only. In production that is not a limitation:
// a Windows-native server has no runtime at all, so nothing reaches this code
// there, and the 503 from requireRuntime is what such a server returns. The
// harness pins a runtime as available on every platform so the rest of the
// manager's rules can be tested anywhere - which is why the two tests that walk
// all the way to the process table say so instead of failing on Windows.
func requireProcTable(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("this test reads /proc, which is Linux-only; GOOS = %s", runtime.GOOS)
	}
}

// TestStoppingAnAgentWithNoneRunningIsNotAnError pins idempotence.
//
// A user who presses Stop twice, or whose click races an agent that already
// exited, has not made a mistake, and the answer they get is the same state
// they would have got had it worked.
func TestStoppingAnAgentWithNoneRunningIsNotAnError(t *testing.T) {
	requireProcTable(t)
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{spec: testAgentSpec},
		runtimeAvailable: true,
		shell:            "/bin/bash",
		agentStopGrace:   100 * time.Millisecond,
		agentPoll:        25 * time.Millisecond,
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)
	h.backend.setPane(session.PaneProcess{
		PID:     4242,
		Command: "bash",
		Dir:     h.projectRuntimePath(t, projectID),
	})

	agent := h.callAgent(t, http.MethodPost, projectID, "stop", http.StatusOK)
	if agent.Running {
		t.Error("stopping a nonexistent agent reported one as running")
	}
	if agent.State != session.AgentStopped {
		t.Errorf("state = %q, want %q", agent.State, session.AgentStopped)
	}
}

// TestStoppingTheAgentLeavesTheRuntimeAlone is Stop-is-not-Destroy, at the API
// layer where a client can actually get it wrong.
//
// An interrupted agent leaves its terminal, its scrollback and its shell
// exactly where they were. A client that expected stop to end the session would
// destroy work the user can still see.
func TestStoppingTheAgentLeavesTheRuntimeAlone(t *testing.T) {
	requireProcTable(t)
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{spec: testAgentSpec},
		runtimeAvailable: true,
		shell:            "/bin/bash",
		agentStopGrace:   100 * time.Millisecond,
		agentPoll:        25 * time.Millisecond,
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startRuntime(t, projectID)
	h.backend.setPane(session.PaneProcess{
		PID:     4242,
		Command: "bash",
		Dir:     h.projectRuntimePath(t, projectID),
	})

	h.callAgent(t, http.MethodPost, projectID, "stop", http.StatusOK)

	recorder := h.call(http.MethodGet, "/api/projects/"+projectID+"/runtime", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reading the runtime after a stop: status = %d, body was %s",
			recorder.Code, recorder.Body.String())
	}
	rt := decode[runtimeResponse](t, recorder).Runtime
	if rt.State != session.StateRunning {
		t.Errorf("runtime state = %q after the agent was stopped, want %q",
			rt.State, session.StateRunning)
	}
	if !rt.SessionAlive {
		t.Error("stopping the agent ended the session; the terminal and its scrollback are gone")
	}
}

// ---------------------------------------------------------------------------
// The capability report
// ---------------------------------------------------------------------------

// TestTheCapabilityReportNamesTheClaudeBinaryThatWouldRun pins the field a UI
// reads to decide whether to offer a Start button.
//
// `features.claudeRuntime` needs the terminal as well as a resolved agent, so
// the two cases are asserted side by side: the same resolved Claude Code on a
// machine that can host a terminal, and on one that cannot.
func TestTheCapabilityReportNamesTheClaudeBinaryThatWouldRun(t *testing.T) {
	installed := claude.Installation{
		Type:      "claude",
		Available: true,
		Version:   "2.1.274",
		Binary:    "claude",
		Path:      "/home/you/.local/share/claude/versions/2.1.274",
		Command:   "'/home/you/.local/share/claude/versions/2.1.274'",
	}

	hostable := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, claude: &installed})
	info := decode[serverInfoResponse](t, hostable.call(http.MethodGet, "/api/server", ""))
	if !info.Features["terminal"] {
		t.Fatal("the terminal is unavailable on a host that can run runtimes; the rest of this test means nothing")
	}
	if !info.Features["claudeRuntime"] {
		t.Error("features.claudeRuntime is false with a resolved Claude Code and a usable terminal")
	}
	if info.Claude == nil {
		t.Fatal("the resolved Claude Code is not reported at all")
	}
	if info.Claude.Path != installed.Path || info.Claude.Command != installed.Command {
		t.Errorf("claude = %+v, want path %q and command %q",
			*info.Claude, installed.Path, installed.Command)
	}

	// The same installation where no terminal can run: the agent cannot be
	// started, whatever is installed, because there is nowhere to start it.
	notHostable := newHarnessOpts(t, harnessOptions{claude: &installed})
	info = decode[serverInfoResponse](t, notHostable.call(http.MethodGet, "/api/server", ""))
	if info.Features["claudeRuntime"] {
		t.Error("features.claudeRuntime is true on a server that cannot host a terminal")
	}
}

// TestTheCapabilityReportLeavesClaudeOutWhenNothingResolves is the third case:
// a server with no Claude Code on it says so by omission, and a client that
// reads the field as a promise is not given one.
func TestTheCapabilityReportLeavesClaudeOutWhenNothingResolves(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	info := decode[serverInfoResponse](t, h.call(http.MethodGet, "/api/server", ""))
	if info.Claude != nil {
		t.Errorf("claude = %+v, want the field absent on a server with no agent configured", *info.Claude)
	}
	if info.Features["claudeRuntime"] {
		t.Error("features.claudeRuntime is true with no agent configured")
	}
}

// ---------------------------------------------------------------------------
// Error codes
// ---------------------------------------------------------------------------

// TestAgentErrorCodesCarryTheStatusAClientActsOn pins the mapping directly.
//
// The codes are a vocabulary the manager speaks and the status is what a client
// does about it, and the two are decided in different packages by different
// reasoning. A code that fell through to the default would answer 500 - "this
// server is broken" - for conditions that are not brokenness: no Claude Code
// installed is 503, a busy terminal is 409, an agent in the wrong directory is
// 422. None of those is the caller's bug and none is the server's.
func TestAgentErrorCodesCarryTheStatusAClientActsOn(t *testing.T) {
	cases := []struct {
		code string
		want int
		why  string
	}{
		{session.CodeAgentUnavailable, http.StatusServiceUnavailable,
			"there is no agent to host here; the message says how to fix it"},
		{session.CodeAgentTerminalBusy, http.StatusConflict,
			"the terminal is in a state that makes the call meaningless, and states change"},
		{session.CodeAgentWrongDirectory, http.StatusUnprocessableEntity,
			"the request is fine and the environment cannot satisfy it"},
		{session.CodeAgentLaunchFailed, http.StatusInternalServerError,
			"the server said it would start something and did not"},
		{session.CodeAgentStopFailed, http.StatusInternalServerError,
			"the server said it would interrupt something and could not"},
	}
	for _, tc := range cases {
		if got := statusForCode(tc.code); got != tc.want {
			t.Errorf("statusForCode(%q) = %d, want %d: %s", tc.code, got, tc.want, tc.why)
		}
	}
}
