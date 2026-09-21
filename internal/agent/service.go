package agent

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/idgen"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/task"
)

// This file is the chain: everything that has to happen, in the order it has to
// happen in, for an agent to be started and observed.
//
// # The order, and why it is that order
//
//	ensure the runtime     a terminal has to exist before anything can run in it
//	notice an agent         an agent already running is not started twice
//	choose a session id     the id has to exist before the process does
//	create the attempt      the record exists before the thing it records
//	attach the receiver     the endpoint has to be listening before Claude is told where it is
//	write the settings      the document names the endpoint, so the endpoint comes first
//	bind                    the attempt is attached to its runtime, and RUNNING
//	launch                  the command line names both the id and the document
//
// Three of those are easy to get wrong and each one has cost something already.
// The settings document names the adapter's port, so an adapter started after it
// would produce a document pointing at nothing - and because an undelivered hook
// is *silent*, that failure looks exactly like an agent that had nothing to say.
// The session id has to be chosen before the process starts, because once it is
// running its id is a fact rather than a decision. And the bind has to happen
// before the launch: Claude's first hook fires while the launch is still waiting
// for its process, so a launch that came first would put an event into the log
// naming a runtime that nothing had yet said belonged to the attempt.
//
// # Undoing
//
// Every step can fail, and a step that fails has to take back the ones before
// it. What it must not take back is anything it did not do: a runtime that was
// already running when the call arrived is left running, because the caller
// asked for an agent and not for a terminal to be torn down. §Ten of the phase
// brief asks for no orphaned runtime and no dangling attempt, and the reading
// taken here is that both mean "nothing left behind by *this* call".

// cleanupTimeout bounds the work of taking back a failed start.
//
// It is separate from the request's own context because the request may already
// be over - the caller may have hung up - and the tidying still has to happen.
// Undoing is bounded anyway; this is what stops a runtime that will not stop
// from holding the handler open after the caller has gone.
const cleanupTimeout = 20 * time.Second

// lockStripes is how many locks are used to serialise operations per project.
//
// A fixed array rather than a map keyed by project id: a map would grow with
// every id a caller named, and the ids come from outside. Two projects that
// happen to share a stripe serialise against each other, which costs a wait and
// never a correctness.
const lockStripes = 16

// Attachment is one observed runtime, re-exported for callers that would
// otherwise import two packages to describe one thing.
type Attachment = claude.Attachment

// RuntimeOperator is the runtime manager as this package uses it.
//
// It is a consumer-declared interface, like every other cross-package boundary
// in this build: the runtime manager does not know it is being used this way,
// and this package does not depend on any part of it that it does not call.
type RuntimeOperator interface {
	// Runtime reports a project's runtime. A project whose runtime has never
	// been started is reported STOPPED rather than as an error.
	Runtime(ctx context.Context, projectID string) (*session.Runtime, error)

	// Start brings a project's runtime up. It is idempotent.
	Start(ctx context.Context, projectID string) (*session.Runtime, error)

	// Stop ends a runtime's work and keeps its scrollback.
	Stop(ctx context.Context, projectID string) (*session.Runtime, error)

	// StartAgent launches the coding agent in a running runtime.
	StartAgent(ctx context.Context, projectID string, launch session.AgentLaunch) (session.AgentStatus, error)

	// StopAgent interrupts the coding agent and leaves the runtime alone.
	StopAgent(ctx context.Context, projectID string) (session.AgentStatus, error)
}

// AdapterOperator is the adapter manager as this package uses it.
type AdapterOperator interface {
	Attach(ctx context.Context, in claude.Attachment) (claude.Attachment, error)
	Detach(ctx context.Context, runtimeID string) error
	HookSettings(runtimeID string) ([]byte, error)
	Subscribe(ctx context.Context, runtimeID string) (<-chan claude.Event, error)
}

// SessionOperator is the task model as this package uses it.
type SessionOperator interface {
	// GetTask is how the chain checks that a task belongs to the project the
	// agent is being started in. It is not decoration: the task service checks
	// that a task exists, and nothing checks that it is *this* project's, so a
	// request naming another project's task would bind this runtime to that
	// attempt and write the history under the other project. See taskBelongsTo.
	GetTask(ctx context.Context, id string) (*task.Task, error)

	CreateSession(ctx context.Context, in task.CreateSessionInput) (*task.AgentSession, error)
	AttachSessionRuntime(ctx context.Context, sessionID, runtimeID string) (*task.AgentSession, error)
	UpdateSessionStatus(ctx context.Context, id, to string) (*task.AgentSession, error)
}

// Options configures a Service. Runtimes, Adapters, Sessions and Settings are
// required.
type Options struct {
	Runtimes RuntimeOperator
	Adapters AdapterOperator
	Sessions SessionOperator
	Settings SettingsWriter

	// Logger receives diagnostics. Nil means slog.Default.
	Logger *slog.Logger

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time

	// NewSessionID mints the Claude session id for one launch. Nil mints a
	// version-4 UUID.
	//
	// It is injectable so that a test can dictate the id and assert what reaches
	// the command line and the adapter, which is the one value in this chain
	// that is chosen rather than measured.
	NewSessionID func() (string, error)
}

// Service starts coding agents, observes them, and records the attempt.
//
// It is safe for concurrent use. Operations on one project are serialised
// against each other; operations on different projects run at the same time.
type Service struct {
	runtimes RuntimeOperator
	adapters AdapterOperator
	sessions SessionOperator
	settings SettingsWriter
	log      *slog.Logger
	now      func() time.Time
	newID    func() (string, error)

	// locks serialises the chain per project stripe. It is held across a process
	// launch, so it is deliberately not the lock that guards runs.
	locks [lockStripes]sync.Mutex

	// mu guards runs. It is never held across anything that blocks.
	mu   sync.Mutex
	runs map[string]*Run

	// watching tracks the goroutines following attached adapters, so that Close
	// can wait for them rather than leaving one writing to a closing service.
	wg sync.WaitGroup
}

// NewService builds a Service.
func NewService(o Options) (*Service, error) {
	switch {
	case o.Runtimes == nil:
		return nil, newError(CodeInvalidInput, "agent: a runtime operator is required")
	case o.Adapters == nil:
		return nil, newError(CodeInvalidInput, "agent: an adapter operator is required")
	case o.Sessions == nil:
		return nil, newError(CodeInvalidInput, "agent: a session operator is required")
	case o.Settings == nil:
		return nil, newError(CodeInvalidInput, "agent: a settings writer is required")
	}

	s := &Service{
		runtimes: o.Runtimes,
		adapters: o.Adapters,
		sessions: o.Sessions,
		settings: o.Settings,
		log:      o.Logger,
		now:      o.Now,
		newID:    o.NewSessionID,
		runs:     make(map[string]*Run),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = idgen.UUIDv4
	}
	return s, nil
}

// lockFor returns the lock for a project's stripe.
func (s *Service) lockFor(projectID string) *sync.Mutex {
	var h uint32 = 2166136261
	for i := 0; i < len(projectID); i++ {
		h ^= uint32(projectID[i])
		h *= 16777619
	}
	return &s.locks[h%lockStripes]
}

// Start launches an agent in a project's runtime and records the attempt.
//
// It brings the runtime up when it is not running, which is what makes one call
// enough to go from a stopped project to an observed session. It does not launch
// a second agent into a runtime that already has one: the existing process is
// reported, and `Adopted` says that nothing was launched.
//
// A TaskID asks for the attempt to be recorded, and it is refused when an agent
// is already running that AgentMux did not start. That refusal is the honest
// answer rather than a limitation: such a process has a session id AgentMux
// never chose and no hook configuration pointing anywhere, so there is nothing
// to observe and an attempt recorded against it would be a row with no evidence
// behind it. Stopping the agent and starting it again is what fixes it, and the
// error says so.
func (s *Service) Start(ctx context.Context, in StartInput) (Result, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return Result{}, newError(CodeInvalidInput, "an agent must be started in a named project").
			withDetail("field", "projectId")
	}
	taskID := strings.TrimSpace(in.TaskID)

	lock := s.lockFor(projectID)
	lock.Lock()
	defer lock.Unlock()

	runtimeID := project.SessionNameFor(projectID)

	// 1. The terminal has to exist first.
	rt, err := s.runtimes.Runtime(ctx, projectID)
	if err != nil {
		return Result{}, err
	}
	u := &undo{service: s, projectID: projectID, runtimeID: runtimeID}
	if rt.State != session.StateRunning {
		if rt, err = s.runtimes.Start(ctx, projectID); err != nil {
			return Result{}, err
		}
		u.startedRuntime = true
	}

	// 2. An agent that is already there is not started twice.
	if rt.Agent != nil && rt.Agent.Running {
		return s.adopt(projectID, taskID, *rt.Agent)
	}

	// 2b. Anything still bound for this project is stale: the check above just
	// established that no agent is running, so a binding that survives can only
	// be an attempt whose process went away without a `SessionEnd` hook to say
	// so - a kill, a crash, a terminal nobody typed into. Left alone it would
	// stay RUNNING forever, and the binding for the attempt about to start would
	// replace it with nothing recording that it ended.
	if _, stale := s.Run(projectID); stale {
		s.end(ctx, projectID, OutcomeFailed)
	}

	// 3. The id the whole correlation rests on, chosen before anything runs.
	sessionID, err := s.newID()
	if err != nil {
		u.run(ctx)
		return Result{}, wrapError(err, CodeInvalidInput,
			"could not choose a session id for the agent in project %s", projectID)
	}

	// 4. The attempt, before the thing it is an attempt at.
	var attempt *task.AgentSession
	if taskID != "" {
		if err := s.taskBelongsTo(ctx, taskID, projectID); err != nil {
			u.run(ctx)
			return Result{}, err
		}
		attempt, err = s.sessions.CreateSession(ctx, task.CreateSessionInput{TaskID: taskID})
		if err != nil {
			u.run(ctx)
			return Result{}, err
		}
		u.attemptID = attempt.ID
	}

	// 5. The receiver, so the document written next names a live port.
	agentSessionID := ""
	if attempt != nil {
		agentSessionID = attempt.ID
	}
	if _, err := s.adapters.Attach(ctx, claude.Attachment{
		ProjectID:      projectID,
		RuntimeID:      runtimeID,
		AgentSessionID: agentSessionID,
		SessionID:      sessionID,
	}); err != nil {
		u.run(ctx)
		return Result{}, err
	}
	u.attached = true

	// 6. The configuration that tells Claude where the receiver is.
	document, err := s.adapters.HookSettings(runtimeID)
	if err != nil {
		u.run(ctx)
		return Result{}, err
	}
	settingsPath, err := s.settings.Write(runtimeID, document)
	if err != nil {
		u.run(ctx)
		return Result{}, err
	}
	u.wroteSettings = true

	// 7. The attempt, attached to the runtime and moved to RUNNING *before* the
	// agent is launched.
	//
	// The order is the whole point of this step. `UpdateSessionStatus` is what
	// writes the event that binds the runtime to the attempt, and Claude's first
	// hook fires while the launch below is still waiting for its process - so a
	// launch that came first would let `agent.started` reach the event log
	// before anything said which attempt the runtime belonged to. Every
	// projection of that event would then have nowhere to put it.
	//
	// The cost is that an attempt reads RUNNING for the moment between here and
	// the launch returning, which is a few hundred milliseconds of a state that
	// is about to be true. The alternative was a first event nobody could
	// attribute, permanently.
	if attempt != nil {
		if attempt, err = s.sessions.AttachSessionRuntime(ctx, attempt.ID, runtimeID); err != nil {
			u.run(ctx)
			return Result{}, err
		}
		if attempt, err = s.sessions.UpdateSessionStatus(ctx, attempt.ID, task.StatusSessionRunning); err != nil {
			u.run(ctx)
			return Result{}, err
		}
	}

	// 8. The launch, carrying the id and the document.
	status, err := s.runtimes.StartAgent(ctx, projectID, session.AgentLaunch{
		SessionID:    sessionID,
		SettingsPath: settingsPath,
	})
	if err != nil {
		u.run(ctx)
		return Result{}, err
	}
	u.launched = true

	run := &Run{
		ProjectID:      projectID,
		RuntimeID:      runtimeID,
		AgentSessionID: agentSessionID,
		SessionID:      sessionID,
		StartedAt:      s.now().UTC(),
	}
	s.remember(run)
	s.watch(runtimeID)

	s.log.Info("claude agent bound to a runtime",
		"projectId", projectID,
		"runtimeId", runtimeID,
		"agentSessionId", agentSessionID,
		"runtimeStarted", u.startedRuntime)

	return Result{
		Agent:          status,
		Run:            run,
		Session:        attempt,
		RuntimeStarted: u.startedRuntime,
	}, nil
}

// adopt reports an agent that was already running.
//
// Nothing is launched, so nothing is bound - unless a previous call in this
// process started it, in which case the binding still stands and is returned.
func (s *Service) adopt(projectID, taskID string, status session.AgentStatus) (Result, error) {
	run, bound := s.Run(projectID)

	if taskID != "" {
		switch {
		case !bound:
			return Result{Agent: status, Adopted: true}, newError(CodeAgentRunning,
				"an agent is already running in project %s and AgentMux did not start it, "+
					"so there is no session to record; stop the agent and start it again "+
					"to have the attempt recorded", projectID).
				withDetail("field", "taskId")
		case run.AgentSessionID == "":
			return Result{Agent: status, Adopted: true, Run: &run}, newError(CodeAgentRunning,
				"the agent running in project %s was started without a task and cannot be "+
					"re-pointed at one while it runs; stop the agent and start it again", projectID).
				withDetail("field", "taskId")
		}
	}

	res := Result{Agent: status, Adopted: true}
	if bound {
		res.Run = &run
	}
	return res, nil
}

// Stop ends a project's agent and closes its attempt as cancelled.
//
// It is the `agent/stop` path: the agent is interrupted and the runtime is left
// alone, which is what Ctrl-C at the terminal does. Ending the runtime as well
// is the runtime's own stop, and removing the terminal is DELETE on the runtime.
func (s *Service) Stop(ctx context.Context, in StopInput) (Result, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return Result{}, newError(CodeInvalidInput, "an agent must be stopped in a named project").
			withDetail("field", "projectId")
	}

	lock := s.lockFor(projectID)
	lock.Lock()
	defer lock.Unlock()

	status, err := s.runtimes.StopAgent(ctx, projectID)
	if err != nil {
		return Result{}, err
	}
	// The interrupt can be declined: a program that handles Ctrl-C itself, or a
	// process that ignores it, is still running when StopAgent returns. Closing
	// the attempt then would record that a session ended while it is still
	// going, and detaching would stop observing it - so nothing is closed and
	// the runtime's own explanation is passed back instead.
	if status.Running {
		run, bound := s.Run(projectID)
		res := Result{Agent: status}
		if bound {
			res.Run = &run
		}
		return res, nil
	}
	run, attempt := s.end(ctx, projectID, OutcomeCancelled)
	return Result{Agent: status, Run: run, Session: attempt}, nil
}

// Release ends observation of a project's agent without touching the agent.
//
// It is what the runtime's own stop and destroy handlers call. A runtime that
// is being torn down takes the agent with it, and an adapter left listening on a
// port for a runtime that no longer exists is a resource held for the life of
// the process; a subscriber left following it is a goroutine that will never
// see anything again.
func (s *Service) Release(ctx context.Context, projectID string, outcome Outcome) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return newError(CodeInvalidInput, "a runtime must be named to release its agent")
	}

	lock := s.lockFor(projectID)
	lock.Lock()
	defer lock.Unlock()

	s.end(ctx, projectID, outcome)
	return nil
}

// end closes out whatever is bound for a project, and reports what it closed.
//
// It is safe to call when nothing is bound, which is the common case: a runtime
// stopped with no agent in it, or stopped twice.
func (s *Service) end(ctx context.Context, projectID string, outcome Outcome) (*Run, *task.AgentSession) {
	run, bound := s.forget(projectID)

	// The cleanup runs on a context that outlives the request, for the reason
	// cleanupTimeout gives. Everything below is bounded and none of it can fail
	// the caller: the agent is already gone by the time this runs, and a
	// listener that would not shut down does not make it still be there.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	runtimeID := project.SessionNameFor(projectID)
	if err := s.adapters.Detach(cleanupCtx, runtimeID); err != nil {
		s.log.Warn("could not detach the claude adapter",
			"projectId", projectID, "runtimeId", runtimeID, "error", err)
	}
	if err := s.settings.Remove(runtimeID); err != nil {
		s.log.Warn("could not remove the claude settings document",
			"projectId", projectID, "runtimeId", runtimeID, "error", err)
	}

	if !bound || run.AgentSessionID == "" {
		return nil, nil
	}
	attempt, err := s.sessions.UpdateSessionStatus(ctx, run.AgentSessionID, taskStatusFor(outcome))
	if err != nil {
		// The agent is already gone; what is lost is the record of how it ended.
		// The attempt stays RUNNING, which is wrong and is reported rather than
		// hidden.
		s.log.Warn("could not close the agent session",
			"agentSessionId", run.AgentSessionID, "outcome", outcome, "error", err)
		return &run, nil
	}
	s.log.Info("claude agent released",
		"projectId", projectID, "agentSessionId", run.AgentSessionID, "outcome", outcome)
	return &run, attempt
}

// taskBelongsTo refuses a task that is not this project's.
//
// # Why this is here and not in the task service
//
// A task belongs to exactly one project for its whole life, and the task
// service enforces that when a task is created - the project is resolved before
// anything is written. What it does not enforce is that a *later* request
// mentioning a task also mentions the same project, because until this phase no
// request named both.
//
// This one does, and the failure it would otherwise produce is not a refusal
// but a wrong record. `POST /api/projects/{A}/runtime/agent/start` with a task
// belonging to B would bind A's runtime to B's attempt, and the two events that
// result are written under different projects - `session.created` under the
// task's project, `session.status_changed` under the task's project with A's
// runtime id - into an append-only log that cannot be corrected. A runtime id
// from one project under another project's history is a row nobody can ever
// interpret, so the check happens before anything is created.
//
// It is a check and not a translation: a caller that named two projects has
// made a mistake, and guessing which one it meant would be inventing an intent.
func (s *Service) taskBelongsTo(ctx context.Context, taskID, projectID string) error {
	t, err := s.sessions.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.ProjectID != projectID {
		return newError(CodeTaskMismatch,
			"task %s belongs to project %s, not to %s; an attempt is recorded in the "+
				"project its task belongs to", taskID, t.ProjectID, projectID).
			withDetail("field", "taskId").
			withDetail("taskProjectId", t.ProjectID)
	}
	return nil
}

// taskStatusFor translates an outcome into the task model's vocabulary.
//
// The translation happens once, here, so that this package never restates the
// task statuses and the task model never learns what an outcome is.
func taskStatusFor(outcome Outcome) string {
	switch outcome {
	case OutcomeCancelled:
		return task.StatusSessionCancelled
	case OutcomeFailed:
		return task.StatusSessionFailed
	default:
		return task.StatusSessionCompleted
	}
}

// watch follows an attached adapter and closes the attempt when the session
// ends by itself.
//
// `SessionEnd` is the one signal in this build that says an attempt ran to its
// own end: Claude Code fires no hook meaning "the work is done", and the
// `result` envelope that would say so is on the stream the runtime owns.
// docs/AGENT_RUNTIME_BINDING.md §5.
//
// The subscription is opened inside the goroutine rather than before it, and the
// wait group is incremented before the goroutine exists. Both are about Close:
// it detaches every adapter and then waits, and a subscription opened outside
// would be a `Wait` that could return before the `Add` that follows it.
func (s *Service) watch(runtimeID string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch, err := s.adapters.Subscribe(ctx, runtimeID)
		if err != nil {
			// The adapter went away between attaching and following it, which
			// is a shutdown or an immediate stop. There is nothing to follow.
			s.log.Debug("could not follow the claude adapter for a session end",
				"runtimeId", runtimeID, "error", err)
			return
		}
		for ev := range ch {
			if ev.Kind != claude.KindSessionEnd {
				continue
			}
			// The session ended on its own. Whether an attempt was recorded or
			// not, the adapter is holding a port for a process that has gone.
			s.end(context.Background(), ev.ProjectID, OutcomeCompleted)
			return
		}
	}()
}

// Run reports the binding for a project.
func (s *Service) Run(projectID string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[projectID]
	if !ok {
		return Run{}, false
	}
	return *run, true
}

// Runs reports every binding, ordered by project id.
//
// The order is by identifier rather than by time so that two calls over the
// same set return the same sequence, for the same reason the adapter's own
// listings do.
func (s *Service) Runs() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0, len(s.runs))
	for _, run := range s.runs {
		out = append(out, *run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out
}

// Close detaches every adapter and stops following anything.
//
// It deliberately does not change any attempt's status. The server stopping is
// not the work stopping: Claude is a child of the shell inside tmux and outlives
// this process, so an attempt that was running is still running, and marking it
// failed because the server was restarted would be recording an event that did
// not happen.
//
// What it does mean is that the attempts it leaves running are no longer
// observed, and a later phase has to reconcile that. docs/AGENT_RUNTIME_BINDING.md
// §6 records it with the rest of the binding's lifetime.
func (s *Service) Close(ctx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	// The adapters are detached before the watchers are waited for, and the
	// order is not a preference. A watcher is ranging over the channel its
	// adapter publishes to, and that channel is closed by the adapter stopping -
	// so waiting first would be waiting for a goroutine that is waiting for the
	// thing being waited on. Detaching is what ends them.
	var err error
	if closer, ok := s.adapters.(interface{ Close(context.Context) error }); ok {
		err = closer.Close(cleanupCtx)
	}
	s.wg.Wait()
	return err
}

// remember records a binding.
func (s *Service) remember(run *Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[run.ProjectID] = run
}

// forget removes and returns a binding.
func (s *Service) forget(projectID string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[projectID]
	if !ok {
		return Run{}, false
	}
	delete(s.runs, projectID)
	return *run, true
}

// undo takes back what a failed start did, newest first.
//
// It exists as a type rather than a closure so that the list of things to take
// back is visible in one place. A chain that cleans up as it goes has its
// cleanup spread across eight error branches, and the branch somebody adds later
// is the one that forgets.
type undo struct {
	service   *Service
	projectID string
	runtimeID string

	startedRuntime bool
	launched       bool
	attached       bool
	wroteSettings  bool
	attemptID      string
}

// run takes back every step that was completed, in reverse order.
//
// Nothing here fails the caller. The caller is already being told why the start
// failed; a second error about the cleanup would replace the useful one. Each
// failure is logged with what it was, and the chain carries on, because a
// settings file that could not be removed must not stop the runtime from being
// stopped.
func (u *undo) run(ctx context.Context) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	if u.launched {
		if _, err := u.service.runtimes.StopAgent(cleanupCtx, u.projectID); err != nil {
			u.service.log.Warn("could not stop an agent whose start failed",
				"projectId", u.projectID, "error", err)
		}
	}
	if u.attached {
		if err := u.service.adapters.Detach(cleanupCtx, u.runtimeID); err != nil {
			u.service.log.Warn("could not detach an adapter whose start failed",
				"runtimeId", u.runtimeID, "error", err)
		}
	}
	if u.wroteSettings {
		if err := u.service.settings.Remove(u.runtimeID); err != nil {
			u.service.log.Warn("could not remove a settings document for a failed start",
				"runtimeId", u.runtimeID, "error", err)
		}
	}
	if u.attemptID != "" {
		// The attempt was created and the launch did not happen. It is failed
		// rather than deleted, because there is no delete: an attempt that was
		// made and did not work is a fact worth keeping, and the reason it
		// failed is in the error the caller was given.
		if _, err := u.service.sessions.UpdateSessionStatus(
			cleanupCtx, u.attemptID, task.StatusSessionFailed); err != nil {
			u.service.log.Warn("could not fail an agent session whose start failed",
				"agentSessionId", u.attemptID, "error", err)
		}
	}
	if u.startedRuntime {
		// Only a runtime this call started is stopped. One that was already
		// running belongs to whatever started it, and ending a terminal somebody
		// was using because an agent failed to launch in it would be a much
		// larger failure than the one being reported.
		if _, err := u.service.runtimes.Stop(cleanupCtx, u.projectID); err != nil {
			u.service.log.Warn("could not stop a runtime started for a failed agent launch",
				"projectId", u.projectID, "error", err)
		}
	}
}
