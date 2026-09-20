package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// This file is the runtime's half of the agent support: what it needs in order
// to start a coding agent inside a project's terminal, and how it tells whether
// that agent is still there.
//
// It deliberately knows nothing about Claude. It is handed an AgentSpec - a
// command line and the executable that command starts - and it works from
// those. Which program that is, what version, and whether it is authenticated
// are the agent package's business, and an adapter at the wiring layer makes
// the one translation between them.
//
// # How an agent's state is decided
//
// By looking at processes. The runtime asks its backend for the pane's process
// tree, then looks for a process in it whose executable is the one the spec
// names. That is a fact the kernel reports, and it is the same fact before and
// after a server restart.
//
// Nothing here reads the terminal. A runtime that decided "the agent has
// finished" by recognising a prompt in the output would be a runtime that
// breaks the first time the prompt changes, and it is the mistake this phase is
// written to avoid.

// AgentState is the lifecycle state of the agent inside one project's runtime.
//
// It is separate from State, and deliberately so: a runtime is RUNNING whenever
// its terminal exists, whether or not an agent is in it. Claude exiting leaves
// the runtime RUNNING and the agent STOPPED, which is the model Phase 3 asks
// for - the terminal survives the program, because that is the whole point of
// hosting it in tmux.
type AgentState string

// Agent states.
const (
	// AgentStopped means no agent process is running. It covers both "never
	// started" and "AgentMux interrupted it", which the Requested field on
	// AgentStatus separates.
	AgentStopped AgentState = "STOPPED"

	// AgentStarting means a start was requested and the process has not been
	// observed yet.
	AgentStarting AgentState = "STARTING"

	// AgentRunning means the agent process was observed in the runtime's pane.
	AgentRunning AgentState = "RUNNING"

	// AgentStopping means a stop was requested and the process has not gone
	// yet.
	AgentStopping AgentState = "STOPPING"

	// AgentExited means the agent went away without AgentMux asking it to.
	//
	// It is not called "crashed" because AgentMux cannot tell a crash from a
	// Ctrl-C typed straight into the terminal by a person: both are the process
	// ending, and the shell that reaps it is the only thing that ever knew its
	// exit status. Reporting it as a crash would be inventing a cause.
	AgentExited AgentState = "EXITED"

	// AgentFailed means AgentMux tried to start the agent and it did not come
	// up, or it came up somewhere it must not run.
	AgentFailed AgentState = "FAILED"
)

// AgentSpec is what the runtime needs in order to start an agent and to
// recognise it once it is running.
type AgentSpec struct {
	// Type identifies the agent, for example "claude".
	Type string

	// Version is the agent program's version, for diagnostics. The runtime
	// never makes a decision from it.
	Version string

	// Command is the single line typed into the runtime's shell to start the
	// agent. It must be one line: it is delivered as terminal input.
	Command string

	// Executable is the canonical path of the program that command starts.
	//
	// It is the only thing used to recognise a running agent, and it is why the
	// name of the process is not used. Measured on the native Claude Code
	// install, tmux reports the pane's foreground process as "2.1.274" - the
	// basename of the versioned binary the launcher symlinks to - so a runtime
	// looking for a process called "claude" would report a running agent as
	// absent.
	Executable string
}

// AgentLaunch is how a caller configures one launch.
//
// It is what AgentMux wants the agent to be told, and it is the whole of what
// the runtime passes through: the runtime does not read these values, does not
// store them, and does not decide them. It hands them to the provider, which
// turns them into arguments, and types the result into the pane.
//
// The zero value means "launch it the way it would launch with nothing
// configured", which is what a caller that has nothing to say should pass.
type AgentLaunch struct {
	// SessionID is the session id the caller has chosen for this launch.
	//
	// Empty means the agent picks its own, which is the behaviour this build had
	// before anything chose one. It matters that the choice is the caller's: a
	// session id AgentMux chose is what lets an observation be attributed
	// without guesswork, and a session id the agent chose can only be discovered
	// by watching it work.
	SessionID string

	// SettingsPath is a file the agent should read its configuration from.
	//
	// Empty means it reads whatever it would otherwise read. The runtime treats
	// it as an opaque path: what belongs in the file is the agent's business,
	// and a runtime that knew would be a second place the agent's configuration
	// format is written down.
	SettingsPath string
}

// AgentProvider resolves the agent a runtime may host.
//
// It is an interface so that the runtime does not depend on any particular
// agent, and so that the suite can exercise the whole lifecycle - launch,
// observe, interrupt, notice the exit - without a Claude Code installation.
type AgentProvider interface {
	// Spec returns what is needed to start the agent. It returns an error when
	// no agent can be started here, and the error explains why.
	//
	// The launch is a parameter rather than provider state because the command
	// line depends on it and a provider holding "what to do next" would be
	// shared, unordered, and wrong the moment two projects started at once. A
	// provider that cannot honour a launch must refuse it rather than ignore
	// it: an agent started without the arguments the caller asked for looks
	// exactly like one that was started correctly, and is not.
	Spec(ctx context.Context, launch AgentLaunch) (AgentSpec, error)
}

// ProcessInspector is implemented by a backend whose sessions run in a process
// tree the runtime can look at.
//
// It is a separate interface rather than a method on Backend because it is a
// capability, not a requirement: a backend that cannot be inspected can still
// host a terminal, it just cannot host an observable agent. Asking for it by
// type assertion keeps that honest - the runtime says "this backend cannot
// report an agent" instead of silently reporting no agent.
type ProcessInspector interface {
	// PaneProcess reports what a session's terminal is running now.
	PaneProcess(ctx context.Context, name string) (PaneProcess, error)
}

// PaneProcess describes the process a session's terminal is running.
type PaneProcess struct {
	// PID is the process the terminal was created with - the shell, for a
	// runtime AgentMux created. An agent started by typing a command into that
	// shell is one of its descendants.
	PID int `json:"pid"`

	// Command is the name of the process currently in the foreground. It is
	// used only to check that the shell is at a prompt before a command is
	// typed at it, and never to identify the agent.
	Command string `json:"command"`

	// Dir is the directory that process is in, as the runtime reports it.
	Dir string `json:"dir,omitempty"`
}

// AgentStatus is the state of the agent inside one project's runtime.
type AgentStatus struct {
	Type string `json:"type"`

	// Available reports whether an agent can be started here at all. It is
	// false when no agent program could be resolved, and Message says why.
	Available bool `json:"available"`

	State   AgentState `json:"state"`
	Running bool       `json:"running"`

	// Version is the agent program's version, when it is known.
	Version string `json:"version,omitempty"`

	// Executable is the program the runtime launches and recognises.
	Executable string `json:"executable,omitempty"`

	// PID is the observed process id, zero when no agent is running.
	PID int `json:"pid,omitempty"`

	// Dir is the working directory the agent process is actually in, read from
	// the process rather than assumed from the request. It is how the claim
	// that the agent runs in the project's directory is checked instead of
	// asserted.
	Dir string `json:"dir,omitempty"`

	// StartedAt is when the agent process started, from the kernel's own clock.
	// It survives a server restart: an adopted runtime reports the agent's real
	// start time rather than the moment it was noticed.
	StartedAt *time.Time `json:"startedAt,omitempty"`

	// ExitedAt is when the agent was last observed to be gone.
	ExitedAt *time.Time `json:"exitedAt,omitempty"`

	// Requested reports whether AgentMux asked the agent to stop. False with a
	// non-running state means it went away on its own. There is no exit code
	// beside it, and there cannot be one: the shell inside the terminal is the
	// process that reaps the agent, so its exit status is known to the shell
	// and to nothing else, and reading it would mean reading the terminal.
	Requested bool `json:"requested,omitempty"`

	// Message explains a state that needs explaining, and is empty when there
	// is nothing to say.
	Message string `json:"message,omitempty"`
}

// agentState is a runtime's record of the agent it hosts.
type agentState struct {
	spec    AgentSpec
	state   AgentState
	pid     int
	dir     string
	since   time.Time
	ended   time.Time
	reason  string
	asked   bool
	watch   context.CancelFunc
	watched bool
}

// Agent reports the agent inside a project's runtime.
//
// It never fails because no agent is running: "not running" is the answer, not
// an error. It fails only when the project or its runtime cannot be resolved at
// all.
func (m *Manager) Agent(ctx context.Context, projectID string) (AgentStatus, error) {
	if _, err := m.project(ctx, projectID); err != nil {
		return AgentStatus{}, err
	}
	rt, err := m.lookup(projectID)
	if err != nil {
		return AgentStatus{}, err
	}
	if rt == nil {
		return m.absentAgentStatus(ctx), nil
	}
	return m.agentStatus(ctx, rt), nil
}

// StartAgent starts the coding agent inside a project's running runtime.
//
// It is idempotent. An agent that is already running is reported as it is and
// nothing is typed: two agents in one project is the one arrangement this
// design exists to prevent, and a second `claude` typed at a shell that is
// already running one would produce exactly that. An adopted agent is *not*
// launched with the given launch - it is already running with whatever it was
// started with - and a caller that needs the launch to have taken effect has to
// stop the agent and start it again.
//
// It requires the runtime to be RUNNING and does not start one: bringing a
// terminal up is the runtime's own Start, and a manager that quietly started
// one would make "start the agent" and "start the terminal" the same request
// with no way to ask for only the second.
//
// The agent is started by typing a command into the runtime's shell rather than
// executed by AgentMux. That is what makes it visible, interruptible, and part
// of the terminal's scrollback, and it is why the agent outlives the server:
// its parent is the shell inside tmux, not this process.
func (m *Manager) StartAgent(ctx context.Context, projectID string, launch AgentLaunch) (AgentStatus, error) {
	if m.agent == nil {
		return AgentStatus{}, newError(CodeAgentUnavailable,
			"this build cannot host a coding agent")
	}

	rt, err := m.requireRunning(ctx, projectID)
	if err != nil {
		return AgentStatus{}, err
	}
	p, err := m.project(ctx, projectID)
	if err != nil {
		return AgentStatus{}, err
	}
	runtimePath, err := m.runtimePath(p)
	if err != nil {
		return AgentStatus{}, err
	}
	spec, err := m.agent.Spec(ctx, launch)
	if err != nil {
		// The provider's message is carried through rather than replaced. Its
		// contract is that the error explains why no agent can be started, and
		// the reasons it gives are the actionable ones - the CLI is not on
		// PATH, or the path it is configured with cannot be run. "no coding
		// agent can be started" on its own tells a user nothing they can do.
		return AgentStatus{}, wrapError(err, CodeAgentUnavailable,
			"no coding agent can be started: %v", err)
	}

	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	pane, err := m.paneProcess(ctx, rt)
	if err != nil {
		return AgentStatus{}, err
	}

	// Already running. Adopting it rather than launching a second one is what
	// makes this call idempotent, and it is also how a runtime inherited from a
	// previous server process is picked up: the agent was never this process's
	// child, so nothing has to be restored - it only has to be recognised.
	if ref, found, err := findProcessRunning(pane.PID, spec.Executable); err == nil && found {
		m.adoptAgent(rt, spec, ref)
		return m.agentStatus(ctx, rt), nil
	}

	// The agent must start in the project's directory, so the terminal must be
	// there before it does. A terminal attached to the wrong directory is worse
	// than no terminal, and the moment to notice is before the agent runs.
	if err := m.requirePaneInProject(pane, runtimePath); err != nil {
		return AgentStatus{}, err
	}

	// A command typed into a terminal goes to whatever is in the foreground. If
	// that is not the shell, the bytes would land inside another program, so
	// the launch is refused rather than made and hoped for.
	if err := m.requirePaneIdle(pane); err != nil {
		return AgentStatus{}, err
	}

	rt.setAgentLaunching(spec, m.now())
	backend, err := m.backendFor(projectID)
	if err != nil {
		return AgentStatus{}, err
	}
	if err := backend.Launch(ctx, rt.session, spec.Command); err != nil {
		rt.failAgent(err.Error(), m.now())
		return AgentStatus{}, wrapError(err, CodeAgentLaunchFailed,
			"could not start %s in project %s", spec.Type, projectID)
	}

	ref, found := m.waitForAgent(ctx, rt, spec, pane.PID)
	if !found {
		rt.failAgent("the agent was started but no such process appeared", m.now())
		return AgentStatus{}, newError(CodeAgentLaunchFailed,
			"%s was started in project %s but never appeared as a process", spec.Type, projectID)
	}

	// Where the agent actually is, read from the agent. This is the check that
	// makes "the agent runs in the project's directory" a measurement.
	if want := canonicalPath(runtimePath); ref.Dir != want {
		// It is running somewhere it must not be. Leaving it there would be
		// leaving a coding agent in a directory nobody chose, so it is
		// interrupted rather than reported and abandoned.
		_ = backend.Stop(ctx, rt.session)
		rt.failAgent(fmt.Sprintf("the agent started in %s, not %s", ref.Dir, want), m.now())
		return AgentStatus{}, newError(CodeAgentWrongDirectory,
			"%s started in %s rather than the project's directory %s", spec.Type, ref.Dir, want)
	}

	m.trackAgent(rt, spec, ref)

	status := m.agentStatus(ctx, rt)
	m.log.Info("agent started",
		"projectId", projectID, "session", rt.session, "agent", spec.Type, "pid", ref.PID)
	return status, nil
}

// StopAgent interrupts a project's agent.
//
// It is the terminal equivalent of Ctrl-C: the agent is interrupted, and the
// runtime, its shell and its scrollback are left alone. Nothing is escalated to
// a signal the agent cannot handle - an agent killed outright is an agent whose
// work is lost, and "stop the agent" does not mean that.
//
// If the interrupt does not end the agent the status says so rather than
// reporting a stop that did not happen.
func (m *Manager) StopAgent(ctx context.Context, projectID string) (AgentStatus, error) {
	if m.agent == nil {
		return AgentStatus{}, newError(CodeAgentUnavailable, "this build cannot host a coding agent")
	}
	rt, err := m.requireRunning(ctx, projectID)
	if err != nil {
		return AgentStatus{}, err
	}
	// Nothing is being launched here - this is asking what is running - so the
	// launch is the zero value: it names no session and no settings, and a
	// provider that wanted one to answer would be answering a different
	// question.
	spec, err := m.agent.Spec(ctx, AgentLaunch{})
	if err != nil {
		// Same as StartAgent: the provider's reason is the one a user can act
		// on, so it is carried rather than replaced.
		return AgentStatus{}, wrapError(err, CodeAgentUnavailable,
			"no coding agent can be stopped: %v", err)
	}

	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	pane, err := m.paneProcess(ctx, rt)
	if err != nil {
		return AgentStatus{}, err
	}
	ref, found, err := findProcessRunning(pane.PID, spec.Executable)
	if err != nil {
		return AgentStatus{}, wrapError(err, CodeBackendFailure,
			"could not tell whether %s is running in project %s", spec.Type, projectID)
	}
	if !found {
		rt.noteAgentGone(m.now())
		return m.agentStatus(ctx, rt), nil
	}

	rt.noteAgentStopRequested(m.now())
	backend, err := m.backendFor(projectID)
	if err != nil {
		return AgentStatus{}, err
	}
	if err := backend.Stop(ctx, rt.session); err != nil && !isNoSuchSession(err) {
		rt.failAgent(err.Error(), m.now())
		return AgentStatus{}, wrapError(err, CodeAgentStopFailed,
			"could not interrupt %s in project %s", spec.Type, projectID)
	}

	deadline := m.now().Add(m.agentStopGrace)
	for m.now().Before(deadline) {
		if _, found, err := findProcessRunning(pane.PID, spec.Executable); err == nil && !found {
			m.untrackAgent(rt)
			rt.noteAgentStopped(m.now())
			m.log.Info("agent stopped",
				"projectId", projectID, "session", rt.session, "agent", spec.Type, "pid", ref.PID)
			return m.agentStatus(ctx, rt), nil
		}
		if !sleepContext(ctx, m.agentPoll) {
			break
		}
	}

	// Still there. The interrupt was delivered and the agent did not take it,
	// which is a real answer and is reported as one.
	rt.noteAgentInterruptIgnored(m.now())
	return m.agentStatus(ctx, rt), nil
}

// agentStatus builds the API view of a project's agent.
func (m *Manager) agentStatus(ctx context.Context, rt *runtime) AgentStatus {
	spec, state, pid, dir, since, ended, reason, asked := rt.agentSnapshot()

	// Availability is the provider's answer and not the record's. A runtime that
	// has never hosted an agent has no recorded spec, and reading that as "no
	// agent is configured" would tell a client this server cannot host one - on
	// the very panel that exists to offer it - on a machine where Claude Code is
	// installed and working.
	//
	// When there is a record, its spec wins: it is what the running process was
	// actually started with, and a provider that has since resolved a different
	// binary must not change how an agent already running is recognised.
	unavailable := ""
	if spec.Executable == "" {
		spec, unavailable = m.agentAvailability(ctx)
	}

	status := AgentStatus{
		Type:      spec.Type,
		Available: unavailable == "",
		State:     state,
		Running:   state == AgentRunning,
		Version:   spec.Version,
		Message:   reason,
	}
	if status.Type == "" {
		status.Type = agentTypeUnknown
	}
	if unavailable != "" {
		status.Message = unavailable
	}
	if spec.Executable != "" {
		status.Executable = spec.Executable
	}

	// The process is asked again here rather than trusted from the record. The
	// record is what AgentMux last decided; the process table is what is true
	// now, and after a server restart those are different things.
	if status.Available {
		if ref, found, err := m.observeAgent(ctx, rt, spec); err == nil && found {
			status.State = AgentRunning
			status.Running = true
			status.PID = ref.PID
			status.Dir = ref.Dir
			status.StartedAt = timePtr(ref.Started)

			// The process being there is the observation; a reason recorded
			// beside it is why it is still there. Clearing both here would
			// report a stop the agent declined as a stop nobody asked for, and
			// the difference between those two is the whole reason the request
			// is recorded at all. An agent nothing has asked to stop has
			// nothing to explain, so only then is the reason dropped.
			status.Requested = asked
			if !asked {
				status.Message = ""
			}
			return status
		}
	}

	if pid != 0 {
		status.PID = pid
	}
	status.Dir = dir
	status.Requested = asked
	if !since.IsZero() {
		status.StartedAt = timePtr(since)
	}
	if !ended.IsZero() {
		status.ExitedAt = timePtr(ended)
	}
	return status
}

// agentAvailability reports the agent this server would start, and why it cannot
// start one when it cannot.
//
// It is the single place availability is decided, so that a project whose
// runtime has never been started, one whose runtime is running with no agent in
// it, and one whose runtime is running an agent all answer the same question the
// same way.
func (m *Manager) agentAvailability(ctx context.Context) (AgentSpec, string) {
	if m.agent == nil {
		return AgentSpec{}, "this build cannot host a coding agent"
	}
	// The zero launch again: this answers "can one be started here, and what
	// would it be", and the arguments a particular launch carries do not change
	// which agent is installed or where it lives.
	spec, err := m.agent.Spec(ctx, AgentLaunch{})
	if err != nil {
		return AgentSpec{}, err.Error()
	}
	return spec, ""
}

// absentAgentStatus describes a project that has no runtime at all.
func (m *Manager) absentAgentStatus(ctx context.Context) AgentStatus {
	spec, unavailable := m.agentAvailability(ctx)

	status := AgentStatus{Type: agentTypeUnknown, State: AgentStopped, Available: unavailable == ""}
	if unavailable != "" {
		status.Message = unavailable
		return status
	}
	status.Type = spec.Type
	status.Version = spec.Version
	status.Executable = spec.Executable
	return status
}

// paneProcess asks a runtime's backend what its terminal is running.
//
// A backend that cannot be inspected is reported as such. Returning an empty
// PaneProcess instead would make "I cannot see" look like "nothing is there",
// and the runtime would report every agent as stopped on such a backend.
func (m *Manager) paneProcess(ctx context.Context, rt *runtime) (PaneProcess, error) {
	backend, err := m.backendFor(rt.projectID)
	if err != nil {
		return PaneProcess{}, err
	}
	inspector, ok := backend.(ProcessInspector)
	if !ok {
		return PaneProcess{}, newError(CodeAgentUnavailable,
			"the %s backend cannot report the process a session is running, so it cannot host an agent",
			m.backendName)
	}
	pane, err := inspector.PaneProcess(ctx, rt.session)
	if err != nil {
		if isNoSuchSession(err) {
			return PaneProcess{}, newError(CodeNotRunning,
				"the terminal runtime for project %s has no live session", rt.projectID)
		}
		return PaneProcess{}, wrapError(err, CodeBackendFailure,
			"could not read the process of session %q", rt.session)
	}
	if pane.PID <= 0 {
		return PaneProcess{}, newError(CodeBackendFailure,
			"session %q reported no process", rt.session)
	}
	return pane, nil
}

// observeAgent looks for the agent's process in a runtime's pane.
func (m *Manager) observeAgent(ctx context.Context, rt *runtime, spec AgentSpec) (processRef, bool, error) {
	if strings.TrimSpace(spec.Executable) == "" {
		return processRef{}, false, nil
	}
	pane, err := m.paneProcess(ctx, rt)
	if err != nil {
		return processRef{}, false, err
	}
	ref, found, err := findProcessRunning(pane.PID, spec.Executable)
	if err != nil {
		return processRef{}, false, err
	}
	return ref, found, nil
}

// waitForAgent waits until the agent's process appears.
//
// The wait is bounded because the failure it bounds is real: a command typed at
// a shell can fail to start anything - the program may be missing, or the shell
// may not be at a prompt after all - and a start that waited forever would be a
// request that never returns.
func (m *Manager) waitForAgent(ctx context.Context, rt *runtime, spec AgentSpec, panePID int) (processRef, bool) {
	deadline := m.now().Add(m.agentStartTimeout)
	for {
		if ref, found, err := findProcessRunning(panePID, spec.Executable); err == nil && found {
			return ref, true
		}
		if !m.now().Before(deadline) {
			return processRef{}, false
		}
		if !sleepContext(ctx, m.agentPoll) {
			return processRef{}, false
		}
	}
}

// requirePaneInProject refuses to start an agent in a terminal that is not in
// the project's own directory.
func (m *Manager) requirePaneInProject(pane PaneProcess, runtimePath string) error {
	want := canonicalPath(runtimePath)
	if pane.Dir == "" {
		// Nothing to compare against. The post-launch check reads the agent's
		// own directory, which is the stronger of the two, so this is not a
		// reason to refuse.
		return nil
	}
	if canonicalPath(pane.Dir) == want {
		return nil
	}
	return newError(CodeAgentWrongDirectory,
		"the terminal for this project is in %s, not in the project's directory %s", pane.Dir, want)
}

// requirePaneIdle refuses to start an agent while something else has the
// terminal in the foreground.
func (m *Manager) requirePaneIdle(pane PaneProcess) error {
	shell := strings.TrimSpace(m.shell)
	if shell == "" || pane.Command == "" {
		// The shell this runtime uses was not configured, so there is no name
		// to compare against. Refusing on a guess would block a legitimate
		// start; the launch is allowed and the appearance of the agent's
		// process is what decides whether it worked.
		return nil
	}
	if pane.Command == filepath.Base(shell) || pane.Command == shell {
		return nil
	}
	return newError(CodeAgentTerminalBusy,
		"the terminal is running %s, not the shell, so a command typed at it would go to that program", pane.Command)
}

// trackAgent records a running agent and starts watching for its exit.
func (m *Manager) trackAgent(rt *runtime, spec AgentSpec, ref processRef) {
	m.untrackAgent(rt)

	ctx, cancel := context.WithCancel(m.ctx)
	rt.mu.Lock()
	rt.agent = agentState{
		spec:    spec,
		state:   AgentRunning,
		pid:     ref.PID,
		dir:     ref.Dir,
		since:   ref.Started,
		watch:   cancel,
		watched: true,
	}
	rt.mu.Unlock()

	m.wg.Add(1)
	go m.watchAgent(ctx, rt, spec, ref.PID)
}

// adoptAgent records an agent that was already running.
//
// It is how a runtime inherited from a previous server process gets its
// watcher back. Nothing is relaunched: the agent is not this process's child
// and does not need to be, it only needs to be recognised.
func (m *Manager) adoptAgent(rt *runtime, spec AgentSpec, ref processRef) {
	rt.mu.Lock()
	already := rt.agent.watched && rt.agent.state == AgentRunning && rt.agent.pid == ref.PID
	rt.mu.Unlock()
	if already {
		return
	}
	m.trackAgent(rt, spec, ref)
}

// untrackAgent stops watching a runtime's agent.
func (m *Manager) untrackAgent(rt *runtime) {
	rt.mu.Lock()
	cancel := rt.agent.watch
	rt.agent.watch = nil
	rt.agent.watched = false
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// watchAgent notices when a running agent goes away.
//
// It polls the process table. It does not read the terminal, and it does not
// conclude anything from a failed look: a process table that cannot be read is
// a reason to wait, not a reason to declare the agent finished.
func (m *Manager) watchAgent(ctx context.Context, rt *runtime, spec AgentSpec, pid int) {
	defer m.wg.Done()

	ticker := time.NewTicker(m.agentPoll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		ref, found, err := m.observeAgent(ctx, rt, spec)
		if err != nil {
			continue
		}
		if !found {
			// It ended. Whether that was a crash or somebody pressing Ctrl-C in
			// the terminal is not something this side can tell, so the record
			// says only whether AgentMux had asked it to stop.
			m.untrackAgent(rt)
			rt.noteAgentEnded(m.now())
			m.log.Info("agent is no longer running",
				"projectId", rt.projectID, "session", rt.session, "agent", spec.Type, "pid", pid)
			return
		}
		rt.noteAgentSeen(ref)
	}
}

// canonicalPath renders a directory for comparison.
//
// Symlinks are resolved because the two sides of a comparison come from
// different places - one from the configuration, one from the kernel - and a
// path that runs through a symlink is the same directory while being a
// different string. A path that cannot be resolved is returned cleaned, which
// is the best that can be said about it.
func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// timePtr is the pointer a JSON field needs to be omitted when zero.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// isNoSuchSession reports whether an error means the session is not there.
//
// Two sentinels say it, and a caller that checked only one would treat a
// terminal that has simply ended as a failure to deliver an interrupt.
func isNoSuchSession(err error) bool {
	return errors.Is(err, ErrNoSuchSession) || IsCode(err, CodeNotRunning)
}

// agentTypeUnknown is reported before an agent has been resolved.
const agentTypeUnknown = "none"
