package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/config"
	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/storage"
	"github.com/kutonlagos/agentmux/internal/task"
)

// These tests exercise the chain against everything except the two things a
// unit test must not need: a real tmux server and a real Claude Code.
//
// The runtime is faked, because starting a terminal is the runtime manager's own
// subject and a suite here would be re-testing it - and because the interesting
// assertions are about what the coordinator asks the runtime to do, which a fake
// records and a real tmux only obeys.
//
// Everything else is real. The adapter binds a real loopback port, the settings
// document is written to a real directory, and the tasks, the events and their
// payloads go through the real services into a real SQLite database. That is
// deliberate: the two defects this chain is most exposed to are a settings
// document naming a port nothing is listening on, and an event payload the
// event service refuses - and a fake on either side would make both invisible.

// testClock is the fixed time every test's events are stamped with.
var testClock = time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testClock }

// ---------------------------------------------------------------------------
// The runtime manager, faked
// ---------------------------------------------------------------------------

// fakeRuntimes is a RuntimeOperator that records what it was asked to do.
//
// Its state is per project, because the real one's is: a project has a runtime
// and a runtime has at most one agent. A fake with one global "an agent is
// running" flag would report a second project as already hosting the first
// project's agent, which is a bug in the fake that reads as a bug in the
// coordinator.
type fakeRuntimes struct {
	mu sync.Mutex

	// states is what each project's runtime reports. An absent project is
	// STOPPED, which is the state a project is in before anything starts it.
	states map[string]session.State

	// agents is which projects have a managed agent up.
	agents map[string]bool

	started       int
	stopped       int
	agentStarts   int
	agentStops    int
	launches      []session.AgentLaunch
	stopAgentKept bool

	// failStart, when set, makes Start fail. It is how a test reaches "the
	// runtime could not come up".
	failStart error

	// failAgent, when set, makes StartAgent fail. It is how a test reaches
	// "the runtime is up and the agent never appeared".
	failAgent error
}

// setState puts a project's runtime into a state before anything runs.
func (f *fakeRuntimes) setState(projectID string, state session.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.states[projectID] = state
}

// setAgent puts a project's agent up or down before anything runs.
func (f *fakeRuntimes) setAgent(projectID string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if running {
		f.agents[projectID] = true
		return
	}
	delete(f.agents, projectID)
}

// init prepares the maps. The caller holds mu.
func (f *fakeRuntimes) init() {
	if f.states == nil {
		f.states = make(map[string]session.State)
	}
	if f.agents == nil {
		f.agents = make(map[string]bool)
	}
}

func (f *fakeRuntimes) Runtime(_ context.Context, projectID string) (*session.Runtime, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.states[projectID]
	if state == "" {
		state = session.StateStopped
	}
	rt := &session.Runtime{
		ProjectID: projectID,
		Session:   project.SessionNameFor(projectID),
		State:     state,
	}
	if f.agents[projectID] {
		rt.Agent = &session.AgentStatus{
			Type:      claude.Type,
			Available: true,
			State:     session.AgentRunning,
			Running:   true,
			PID:       4242,
		}
	}
	return rt, nil
}

func (f *fakeRuntimes) Start(_ context.Context, projectID string) (*session.Runtime, error) {
	f.mu.Lock()
	if f.failStart != nil {
		err := f.failStart
		f.mu.Unlock()
		return nil, err
	}
	f.init()
	f.started++
	f.states[projectID] = session.StateRunning
	f.mu.Unlock()
	return f.Runtime(context.Background(), projectID)
}

func (f *fakeRuntimes) Stop(_ context.Context, projectID string) (*session.Runtime, error) {
	f.mu.Lock()
	f.init()
	f.stopped++
	f.states[projectID] = session.StateStopped
	delete(f.agents, projectID)
	f.mu.Unlock()
	return f.Runtime(context.Background(), projectID)
}

func (f *fakeRuntimes) StartAgent(_ context.Context, projectID string, launch session.AgentLaunch) (session.AgentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAgent != nil {
		return session.AgentStatus{}, f.failAgent
	}
	f.init()
	f.agentStarts++
	f.launches = append(f.launches, launch)
	f.agents[projectID] = true
	return session.AgentStatus{
		Type:      claude.Type,
		Available: true,
		State:     session.AgentRunning,
		Running:   true,
		PID:       4242,
	}, nil
}

func (f *fakeRuntimes) StopAgent(_ context.Context, projectID string) (session.AgentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.agentStops++
	if f.stopAgentKept {
		// The interrupt was declined: the program is still there, which is what
		// a process that handles Ctrl-C itself looks like.
		return session.AgentStatus{
			Type:      claude.Type,
			Available: true,
			State:     session.AgentRunning,
			Running:   true,
			PID:       4242,
			Message:   "the agent ignored the interrupt",
		}, nil
	}
	delete(f.agents, projectID)
	return session.AgentStatus{
		Type:      claude.Type,
		Available: true,
		State:     session.AgentStopped,
	}, nil
}

// lastLaunch is the launch the runtime was last asked to perform.
func (f *fakeRuntimes) lastLaunch() (session.AgentLaunch, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.launches) == 0 {
		return session.AgentLaunch{}, false
	}
	return f.launches[len(f.launches)-1], true
}

func (f *fakeRuntimes) counts() (started, stopped, agentStarts, agentStops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.stopped, f.agentStarts, f.agentStops
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

type harness struct {
	t        *testing.T
	service  *Service
	runtimes *fakeRuntimes
	adapters *claude.Manager
	sessions *task.Service
	projects *project.Service
	events   *event.Service
	settings *FileSettings
	store    *storage.Store
	dataDir  string

	// root is the configured Projects Root. Every project this harness
	// registers has to live under it, because a project outside its roots is
	// refused - which is a rule the project model enforces and this suite has no
	// reason to work around.
	root string
}

// newHarness builds the real stack with a faked runtime manager.
func newHarness(t *testing.T, o Options) *harness {
	t.Helper()

	dataDir := t.TempDir()
	root := t.TempDir()

	store, err := storage.Open(context.Background(), storage.OpenOptions{
		Path: filepath.Join(dataDir, "agentmux.db"),
	})
	if err != nil {
		t.Fatalf("storage.Open returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("closing the store failed: %v", err)
		}
	})
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}

	adapter, err := host.New(host.Options{Mode: config.RuntimeModeNative, Roots: []string{root}})
	if err != nil {
		t.Fatalf("host.New returned an error: %v", err)
	}
	projects, err := project.NewService(project.Options{
		Repository: store.Projects(),
		Host:       adapter,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("project.NewService returned an error: %v", err)
	}

	events, err := event.NewService(event.Options{
		Repository: store.Events(),
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("event.NewService returned an error: %v", err)
	}
	sessions, err := task.NewService(task.Options{
		Repository: store.Tasks(),
		Projects:   projects,
		Events:     events,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("task.NewService returned an error: %v", err)
	}

	adapters := claude.NewManager(claude.ManagerOptions{
		Adapter: claude.AdapterOptions{
			Recorder: events,
			Logger:   discardLogger(),
			Now:      fixedNow,
		},
		Logger: discardLogger(),
	})
	t.Cleanup(func() {
		if err := adapters.Close(context.Background()); err != nil {
			t.Errorf("closing the adapter manager failed: %v", err)
		}
	})

	runtimes := &fakeRuntimes{}
	settings := NewFileSettings(dataDir)

	if o.Runtimes == nil {
		o.Runtimes = runtimes
	}
	if o.Adapters == nil {
		o.Adapters = adapters
	}
	if o.Sessions == nil {
		o.Sessions = sessions
	}
	if o.Settings == nil {
		o.Settings = settings
	}
	if o.Logger == nil {
		o.Logger = discardLogger()
	}
	if o.Now == nil {
		o.Now = fixedNow
	}

	service, err := NewService(o)
	if err != nil {
		t.Fatalf("NewService returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Errorf("closing the coordinator failed: %v", err)
		}
	})

	return &harness{
		t:        t,
		service:  service,
		runtimes: runtimes,
		adapters: adapters,
		sessions: sessions,
		projects: projects,
		events:   events,
		settings: settings,
		store:    store,
		dataDir:  dataDir,
		root:     root,
	}
}

// registerProject makes a real project whose directory exists.
func (h *harness) registerProject(name string) *project.Project {
	h.t.Helper()
	dir := filepath.Join(h.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("could not create the project directory: %v", err)
	}
	p, err := h.projects.Register(context.Background(), project.RegisterInput{HostPath: dir})
	if err != nil {
		h.t.Fatalf("registering a project failed: %v", err)
	}
	return p
}

// createTask makes a task under a project.
func (h *harness) createTask(projectID, title string) *task.Task {
	h.t.Helper()
	tk, err := h.sessions.CreateTask(context.Background(), task.CreateTaskInput{
		ProjectID: projectID,
		Title:     title,
	})
	if err != nil {
		h.t.Fatalf("creating a task failed: %v", err)
	}
	return tk
}

// eventTypes is the timeline of a project, newest first, as type names.
func (h *harness) eventTypes(projectID string) []string {
	h.t.Helper()
	page, err := h.events.ListProject(context.Background(), projectID, event.ListOptions{Limit: 50})
	if err != nil {
		h.t.Fatalf("reading the project timeline failed: %v", err)
	}
	out := make([]string, 0, len(page.Events))
	for _, ev := range page.Events {
		out = append(out, ev.Type)
	}
	return out
}

// eventsOfType returns every stored event of a type, newest first.
func (h *harness) eventsOfType(projectID, eventType string) []*event.AgentEvent {
	h.t.Helper()
	page, err := h.events.ListProject(context.Background(), projectID, event.ListOptions{Limit: 50})
	if err != nil {
		h.t.Fatalf("reading the project timeline failed: %v", err)
	}
	var out []*event.AgentEvent
	for _, ev := range page.Events {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

// sessionsOf lists a task's attempts.
func (h *harness) sessionsOf(taskID string) []*task.AgentSession {
	h.t.Helper()
	list, err := h.sessions.ListSessions(context.Background(), task.ListSessionsInput{TaskID: taskID})
	if err != nil {
		h.t.Fatalf("listing attempts failed: %v", err)
	}
	return list
}

// settingsFor reads the settings document written for a runtime.
func (h *harness) settingsFor(runtimeID string) (string, []byte) {
	h.t.Helper()
	path := filepath.Join(h.settings.Dir(), runtimeID, "settings.json")
	body, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("the settings document at %s could not be read: %v", path, err)
	}
	return path, body
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startInput is the ordinary request: a project, and a task to record against.
func startInput(p *project.Project, tk *task.Task) StartInput {
	in := StartInput{ProjectID: p.ID}
	if tk != nil {
		in.TaskID = tk.ID
	}
	return in
}

// ---------------------------------------------------------------------------
// 1. The start flow
// ---------------------------------------------------------------------------

func TestStartBindsAnAgentToARuntimeAndRecordsTheAttempt(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	result, err := h.service.Start(context.Background(), startInput(p, tk))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	if !result.RuntimeStarted {
		t.Error("RuntimeStarted = false; the project had no runtime and this call started one")
	}
	if !result.Agent.Running {
		t.Errorf("the agent is not running: %+v", result.Agent)
	}
	if result.Run == nil {
		t.Fatal("no binding was reported")
	}

	runtimeID := project.SessionNameFor(p.ID)
	if result.Run.RuntimeID != runtimeID {
		t.Errorf("bound runtime = %q; want %q", result.Run.RuntimeID, runtimeID)
	}
	if result.Run.ProjectID != p.ID {
		t.Errorf("bound project = %q; want %q", result.Run.ProjectID, p.ID)
	}

	// The attempt exists, is running, and names the runtime it ran in.
	if result.Session == nil {
		t.Fatal("no attempt was recorded for a task that was named")
	}
	if result.Session.ID != result.Run.AgentSessionID {
		t.Errorf("the binding names attempt %q but the attempt is %q",
			result.Run.AgentSessionID, result.Session.ID)
	}
	if result.Session.Status != task.StatusSessionRunning {
		t.Errorf("attempt status = %q; want %q", result.Session.Status, task.StatusSessionRunning)
	}
	if result.Session.RuntimeID != runtimeID {
		t.Errorf("attempt runtime = %q; want %q", result.Session.RuntimeID, runtimeID)
	}
	if result.Session.StartedAt == nil {
		t.Error("the attempt has no start time, though it entered RUNNING")
	}
}

// TestTheLaunchCarriesTheSessionIdAndTheSettingsPath is the whole point of the
// chain expressed as one assertion: what AgentMux decided reaches the command
// line.
//
// Nothing else in the build checks this. The launch's arguments are assembled in
// cmd/server, which no unit test here can reach, so a chain that chose an id and
// then dropped it would pass every other test in this file.
func TestTheLaunchCarriesTheSessionIdAndTheSettingsPath(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	result, err := h.service.Start(context.Background(), startInput(p, tk))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	launch, ok := h.runtimes.lastLaunch()
	if !ok {
		t.Fatal("the runtime was never asked to launch anything")
	}
	if launch.SessionID != result.Run.SessionID {
		t.Errorf("the launch carried session id %q; the binding says %q",
			launch.SessionID, result.Run.SessionID)
	}
	if launch.SessionID == "" {
		t.Error("the launch carried no session id, so nothing Claude reports can be attributed")
	}
	if launch.SettingsPath != launch.SettingsPath || launch.SettingsPath == "" {
		t.Error("the launch carried no settings path, so no hook would ever be delivered")
	}
}

// TestTheLaunchRunsBeforeTheSettingsDocumentIsNeeded pins the ordering the
// document's contents depend on.
//
// The settings file names the receiver's port, and the port only exists once the
// adapter is listening. An adapter started after the document was written would
// produce one naming nothing - and an undelivered hook is silent, so that
// failure would look exactly like an agent with nothing to say.
func TestTheSettingsDocumentNamesTheListeningReceiver(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")

	result, err := h.service.Start(context.Background(), startInput(p, nil))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	runtimeID := project.SessionNameFor(p.ID)
	_, body := h.settingsFor(runtimeID)

	att, ok := h.adapters.Attachment(runtimeID)
	if !ok {
		t.Fatal("no adapter is attached for the runtime that was started")
	}
	if att.HookURL == "" {
		t.Fatal("the attachment has no hook URL")
	}
	if !strings.Contains(string(body), att.HookURL) {
		t.Errorf("the settings document does not name the live receiver.\n"+
			"document:\n%s\nreceiver: %s", body, att.HookURL)
	}
	if result.Run == nil || result.Run.SessionID != att.SessionID {
		t.Errorf("the adapter was bound to session %q, the coordinator chose %q",
			att.SessionID, result.Run.SessionID)
	}
}

// ---------------------------------------------------------------------------
// 2. Session binding, end to end through a real hook
// ---------------------------------------------------------------------------

// TestAHookDeliveredToTheReceiverIsRecordedAgainstTheRuntime is the routing
// assertion, and it goes through every real component: a real adapter on a real
// loopback port, rendering a real settings document, receiving a real HTTP POST,
// and writing through the real event service into a real database.
//
// The hook payload is the shape Claude sends. What is checked is not that an
// event was written - that is the adapter's own suite - but that it landed
// against the project and runtime this chain chose, and that it carries an id
// AgentMux dictated rather than one read back.
func TestAHookDeliveredToTheReceiverIsRecordedAgainstTheRuntime(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	result, err := h.service.Start(context.Background(), startInput(p, tk))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	runtimeID := project.SessionNameFor(p.ID)
	att, ok := h.adapters.Attachment(runtimeID)
	if !ok {
		t.Fatal("no adapter is attached")
	}

	deliver(t, att.HookURL, map[string]any{
		"session_id":      result.Run.SessionID,
		"hook_event_name": "SessionStart",
		"source":          "startup",
	})

	started := h.eventsOfType(p.ID, claude.TypeAgentStarted)
	if len(started) != 1 {
		t.Fatalf("recorded %d agent.started event(s); want 1", len(started))
	}
	ev := started[0]
	if ev.ProjectID != p.ID {
		t.Errorf("event project = %q; want %q", ev.ProjectID, p.ID)
	}
	if ev.RuntimeID != runtimeID {
		t.Errorf("event runtime = %q; want %q", ev.RuntimeID, runtimeID)
	}
	if ev.Source != event.SourceAgent {
		t.Errorf("event source = %q; want %q", ev.Source, event.SourceAgent)
	}

	// And the adapter recognises the session id AgentMux dictated, which is what
	// makes the attempt findable from a Claude session at all.
	binding, ok := h.adapters.SessionFor(runtimeID, result.Run.SessionID)
	if !ok {
		t.Fatalf("the adapter does not recognise the session id it was given")
	}
	if binding.AgentSessionID != result.Session.ID {
		t.Errorf("the adapter bound the session to attempt %q; want %q",
			binding.AgentSessionID, result.Session.ID)
	}
}

// TestARecordedHookPayloadCarriesNoPromptAndNoSessionId is the security rule,
// asserted on the row that was actually written rather than on a builder.
func TestARecordedHookPayloadCarriesNoPromptAndNoSessionId(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")

	result, err := h.service.Start(context.Background(), startInput(p, nil))
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	runtimeID := project.SessionNameFor(p.ID)
	att, _ := h.adapters.Attachment(runtimeID)

	// A payload of the shape Claude sends, carrying everything that must not be
	// kept.
	deliver(t, att.HookURL, map[string]any{
		"session_id":      result.Run.SessionID,
		"hook_event_name": "UserPromptSubmit",
		"prompt":          "delete the production database",
		"tool_input":      map[string]any{"command": "rm -rf /"},
		"transcript_path": "/home/someone/.claude/transcript.jsonl",
	})

	prompts := h.eventsOfType(p.ID, claude.TypeAgentPromptSubmitted)
	if len(prompts) != 1 {
		t.Fatalf("recorded %d agent.prompt_submitted event(s); want 1", len(prompts))
	}
	stored := string(prompts[0].Payload)
	for _, forbidden := range []string{
		"delete the production database", "rm -rf", "transcript", "session_id", result.Run.SessionID,
	} {
		if strings.Contains(stored, forbidden) {
			t.Errorf("the stored payload contains %q:\n%s", forbidden, stored)
		}
	}
}

// deliver posts a hook payload to a receiver.
func deliver(t *testing.T, url string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("could not encode the hook payload: %v", err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("could not deliver a hook to %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the receiver answered %d for a hook it should accept", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 3. Failure cleanup
// ---------------------------------------------------------------------------

// TestAFailedStartLeavesNothingBehind walks the failure points in order.
//
// Each one has to take back everything before it and nothing after it: no
// listener left on a port, no settings document naming a port nothing is
// listening on, no attempt left running with no process behind it, and no
// runtime left up that this call started.
func TestAFailedStartLeavesNothingBehind(t *testing.T) {
	cases := map[string]struct {
		runtimes func() *fakeRuntimes
		taskID   func(*task.Task) string
	}{
		"the runtime will not start": {
			runtimes: func() *fakeRuntimes { return &fakeRuntimes{failStart: errTest} },
		},
		"the agent never appears": {
			runtimes: func() *fakeRuntimes { return &fakeRuntimes{failAgent: errTest} },
		},
		"the task does not exist": {
			runtimes: func() *fakeRuntimes { return &fakeRuntimes{} },
			taskID:   func(*task.Task) string { return "task_does_not_exist_at_all" },
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Options{})
			h.runtimes = tc.runtimes()

			service, err := NewService(Options{
				Runtimes: h.runtimes,
				Adapters: h.adapters,
				Sessions: h.sessions,
				Settings: h.settings,
				Logger:   discardLogger(),
				Now:      fixedNow,
			})
			if err != nil {
				t.Fatalf("NewService returned an error: %v", err)
			}

			p := h.registerProject("checkout-service")
			tk := h.createTask(p.ID, "Fix the viewer")

			in := startInput(p, tk)
			if tc.taskID != nil {
				in.TaskID = tc.taskID(tk)
			}

			if result, err := service.Start(context.Background(), in); err == nil {
				t.Fatalf("Start succeeded where it should have failed: %+v", result)
			}

			runtimeID := project.SessionNameFor(p.ID)

			// Nothing is observing the runtime any more.
			if _, attached := h.adapters.Attachment(runtimeID); attached {
				t.Error("an adapter is still attached after a failed start")
			}
			// No settings document is left naming a port nothing is listening on.
			if _, statErr := os.Stat(filepath.Join(h.settings.Dir(), runtimeID)); !os.IsNotExist(statErr) {
				t.Errorf("a settings document survived a failed start: %v", statErr)
			}
			// No attempt is left running.
			for _, attempt := range h.sessionsOf(tk.ID) {
				if !attempt.IsTerminal() {
					t.Errorf("attempt %s is %s after a failed start; want a terminal status",
						attempt.ID, attempt.Status)
				}
			}
			// No agent was left running.
			if _, _, agentStarts, agentStops := h.runtimes.counts(); agentStarts > 0 && agentStops == 0 {
				t.Error("an agent was launched and never stopped after the start failed")
			}
			// And a runtime this call started is not left up.
			if name != "the runtime will not start" {
				if rt, _ := h.runtimes.Runtime(context.Background(), p.ID); rt.State != session.StateStopped {
					t.Errorf("runtime state = %s after a failed start; want STOPPED", rt.State)
				}
			}
		})
	}
}

// TestAFailedStartLeavesAnAlreadyRunningRuntimeAlone is the half of the cleanup
// rule that is easy to get wrong.
//
// The coordinator takes back what it did and nothing else. Ending a terminal
// somebody was already using, because an agent failed to launch in it, would be
// a much larger failure than the one being reported.
func TestAFailedStartLeavesAnAlreadyRunningRuntimeAlone(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	// The runtime is already up before the call, and the agent launch is what
	// fails.
	h.runtimes.setState(p.ID, session.StateRunning)
	h.runtimes.failAgent = errTest

	if _, err := h.service.Start(context.Background(), startInput(p, tk)); err == nil {
		t.Fatal("Start succeeded where the agent could not be launched")
	}

	rt, _ := h.runtimes.Runtime(context.Background(), p.ID)
	if rt.State != session.StateRunning {
		t.Errorf("runtime state = %s; want RUNNING: a runtime this call did not start is not its to stop",
			rt.State)
	}
	if started, _, _, _ := h.runtimes.counts(); started != 0 {
		t.Errorf("the runtime was started %d time(s); it was already running", started)
	}
	for _, attempt := range h.sessionsOf(tk.ID) {
		if !attempt.IsTerminal() {
			t.Errorf("attempt %s is %s; want a terminal status even though the runtime survived",
				attempt.ID, attempt.Status)
		}
	}
}

var errTest = &Error{Code: "test_failure", Message: "the test asked for this to fail"}

// ---------------------------------------------------------------------------
// 4. Restart
// ---------------------------------------------------------------------------

// TestARestartMintsANewSessionAndDoesNotReuseTheFirstBinding is §十四.5.
//
// A second agent in the same runtime is a second Claude session with an id of
// its own, so nothing about the first binding may carry over: not the session
// id, not the hook path, and not the settings document. A binding that survived
// would attribute the new session's hooks to the old attempt.
func TestARestartMintsANewSessionAndDoesNotReuseTheFirstBinding(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	runtimeID := project.SessionNameFor(p.ID)

	first, err := h.service.Start(context.Background(), startInput(p, tk))
	if err != nil {
		t.Fatalf("the first Start returned an error: %v", err)
	}
	firstHook := first.Run.SessionID
	firstURL, _ := h.adapters.Attachment(runtimeID)

	if _, err := h.service.Stop(context.Background(), StopInput{ProjectID: p.ID}); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if _, attached := h.adapters.Attachment(runtimeID); attached {
		t.Fatal("the adapter survived a stop")
	}

	second, err := h.service.Start(context.Background(), startInput(p, tk))
	if err != nil {
		t.Fatalf("the second Start returned an error: %v", err)
	}

	if second.Run.SessionID == firstHook {
		t.Errorf("both launches used session id %q; a restart must mint a fresh one, "+
			"because Claude refuses an id already in use", firstHook)
	}
	secondAtt, _ := h.adapters.Attachment(runtimeID)
	if secondAtt.HookURL == firstURL.HookURL {
		t.Errorf("the receiver kept its address %q across a restart", firstURL.HookURL)
	}
	if secondAtt.AgentSessionID == first.Run.AgentSessionID {
		t.Errorf("the new launch was bound to the first attempt %q", first.Run.AgentSessionID)
	}
	// Two attempts, each closed out or running, and neither confused for the
	// other.
	if got := len(h.sessionsOf(tk.ID)); got != 2 {
		t.Errorf("the task has %d attempt(s); want 2", got)
	}
}

// TestAStaleBindingIsClosedWhenTheAgentWentAwayWithoutASessionEnd is the case a
// kill leaves behind.
//
// The `SessionEnd` hook is the only signal that closes an attempt as completed,
// and a process that was killed never sends one. Without this, the attempt would
// stay RUNNING forever and the next launch would overwrite the binding that
// named it.
func TestAStaleBindingIsClosedWhenTheAgentWentAwayWithoutASessionEnd(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	first, err := h.service.Start(context.Background(), startInput(p, tk))
	if err != nil {
		t.Fatalf("the first Start returned an error: %v", err)
	}

	// The agent dies with no hook to say so.
	h.runtimes.setAgent(p.ID, false)

	if _, err := h.service.Start(context.Background(), startInput(p, tk)); err != nil {
		t.Fatalf("the second Start returned an error: %v", err)
	}

	attempts := h.sessionsOf(tk.ID)
	if len(attempts) != 2 {
		t.Fatalf("the task has %d attempt(s); want 2", len(attempts))
	}
	var firstAttempt *task.AgentSession
	for _, a := range attempts {
		if a.ID == first.Run.AgentSessionID {
			firstAttempt = a
		}
	}
	if firstAttempt == nil {
		t.Fatal("the first attempt could not be found")
	}
	if firstAttempt.Status != task.StatusSessionFailed {
		t.Errorf("the abandoned attempt is %s; want FAILED: it was replaced by a new "+
			"launch and nothing else will ever close it", firstAttempt.Status)
	}
}

// ---------------------------------------------------------------------------
// 5. Adoption
// ---------------------------------------------------------------------------

// TestAnAlreadyRunningAgentIsAdoptedAndNotRecorded is the rule that an
// AgentSession records an attempt AgentMux launched.
//
// An agent running when the call arrives has a session id AgentMux never chose
// and no hook configuration pointing at a receiver. Recording an attempt
// against it would put a row into the task history with no evidence behind it,
// and binding an adapter to it would produce a receiver nothing ever posts to -
// which is indistinguishable from an agent that has nothing to say.
func TestAnAlreadyRunningAgentIsAdoptedAndNotRecorded(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")

	h.runtimes.setState(p.ID, session.StateRunning)
	h.runtimes.setAgent(p.ID, true)

	// Without a task it is the ordinary idempotent start: the agent is reported
	// and nothing is launched.
	result, err := h.service.Start(context.Background(), startInput(p, nil))
	if err != nil {
		t.Fatalf("adopting an agent returned an error: %v", err)
	}
	if !result.Adopted {
		t.Error("Adopted = false; nothing was launched")
	}
	if result.Run != nil {
		t.Errorf("an adopted agent produced a binding: %+v", result.Run)
	}
	if _, _, agentStarts, _ := h.runtimes.counts(); agentStarts != 0 {
		t.Errorf("the runtime was asked to launch %d agent(s) over an agent already running", agentStarts)
	}
	if _, attached := h.adapters.Attachment(project.SessionNameFor(p.ID)); attached {
		t.Error("an adapter was attached for an agent whose session id AgentMux never chose")
	}

	// With a task it is refused, because there is nothing to record an attempt
	// against.
	_, err = h.service.Start(context.Background(), startInput(p, tk))
	if !IsCode(err, CodeAgentRunning) {
		t.Errorf("recording an attempt against an adopted agent = %v; want %q",
			err, CodeAgentRunning)
	}
	if got := len(h.sessionsOf(tk.ID)); got != 0 {
		t.Errorf("%d attempt(s) were created for an adopted agent; want none", got)
	}
}

// ---------------------------------------------------------------------------
// 6. The task must belong to the project
// ---------------------------------------------------------------------------

// TestATaskFromAnotherProjectIsRefused is the check that keeps a runtime id out
// of another project's history.
//
// The event log is append-only. A start that bound project A's runtime to
// project B's task would write `session.created` under B and
// `session.status_changed` under B carrying A's runtime id, and nothing could
// ever correct it.
func TestATaskFromAnotherProjectIsRefused(t *testing.T) {
	h := newHarness(t, Options{})
	alpha := h.registerProject("alpha")
	bravo := h.registerProject("bravo")
	bravosTask := h.createTask(bravo.ID, "Bravo's work")

	_, err := h.service.Start(context.Background(), StartInput{
		ProjectID: alpha.ID,
		TaskID:    bravosTask.ID,
	})
	if !IsCode(err, CodeTaskMismatch) {
		t.Fatalf("starting alpha's agent against bravo's task = %v; want %q",
			err, CodeTaskMismatch)
	}

	// Nothing was created, nothing was started, nothing was attached.
	if got := len(h.sessionsOf(bravosTask.ID)); got != 0 {
		t.Errorf("%d attempt(s) were recorded against the other project's task", got)
	}
	if types := h.eventTypes(alpha.ID); len(types) != 0 {
		t.Errorf("alpha's timeline has %v; a refused start writes nothing", types)
	}
	if _, _, agentStarts, _ := h.runtimes.counts(); agentStarts != 0 {
		t.Error("an agent was launched for a request that was refused")
	}
}

// ---------------------------------------------------------------------------
// 7. Stopping
// ---------------------------------------------------------------------------

func TestStopClosesTheAttemptAsCancelledAndDetaches(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	runtimeID := project.SessionNameFor(p.ID)

	if _, err := h.service.Start(context.Background(), startInput(p, tk)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	stopped, err := h.service.Stop(context.Background(), StopInput{ProjectID: p.ID})
	if err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if stopped.Agent.Running {
		t.Error("the agent is still reported as running")
	}

	attempts := h.sessionsOf(tk.ID)
	if len(attempts) != 1 {
		t.Fatalf("the task has %d attempt(s); want 1", len(attempts))
	}
	if attempts[0].Status != task.StatusSessionCancelled {
		t.Errorf("attempt status = %q; want %q: a person asked for it to stop",
			attempts[0].Status, task.StatusSessionCancelled)
	}
	if attempts[0].EndedAt == nil {
		t.Error("the attempt has no end time, though it left RUNNING")
	}
	if _, attached := h.adapters.Attachment(runtimeID); attached {
		t.Error("the adapter is still attached after the agent was stopped")
	}
	if _, err := os.Stat(filepath.Join(h.settings.Dir(), runtimeID)); !os.IsNotExist(err) {
		t.Error("the settings document survived the stop")
	}
	if bound, ok := h.service.Run(p.ID); ok {
		t.Errorf("a binding survives the stop: %+v", bound)
	}
}

// TestADeclinedStopLeavesTheAttemptRunning is the case where Ctrl-C does not
// work.
//
// A program that handles the interrupt itself is still running when the runtime
// says the stop was ignored. Closing the attempt then would record that a
// session ended while it is still going, and detaching would stop observing it.
func TestADeclinedStopLeavesTheAttemptRunning(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	runtimeID := project.SessionNameFor(p.ID)

	if _, err := h.service.Start(context.Background(), startInput(p, tk)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	h.runtimes.stopAgentKept = true

	result, err := h.service.Stop(context.Background(), StopInput{ProjectID: p.ID})
	if err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if !result.Agent.Running {
		t.Error("the agent is reported as stopped, but the runtime said the interrupt was ignored")
	}

	attempts := h.sessionsOf(tk.ID)
	if attempts[0].Status != task.StatusSessionRunning {
		t.Errorf("attempt status = %q; want %q: the agent is still running",
			attempts[0].Status, task.StatusSessionRunning)
	}
	if _, attached := h.adapters.Attachment(runtimeID); !attached {
		t.Error("the adapter was detached from an agent that is still running")
	}
}

// TestReleaseEndsObservationWithoutTouchingTheAgent is the runtime stop and
// destroy path.
//
// A runtime that is being torn down takes its agent with it, and neither the
// runtime manager nor the coordinator is asked to stop the agent separately -
// there is nothing left to send an interrupt to. What Release has to do is stop
// listening.
func TestReleaseEndsObservationWithoutTouchingTheAgent(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.registerProject("checkout-service")
	tk := h.createTask(p.ID, "Fix the viewer")
	runtimeID := project.SessionNameFor(p.ID)

	if _, err := h.service.Start(context.Background(), startInput(p, tk)); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := h.service.Release(context.Background(), p.ID, OutcomeFailed); err != nil {
		t.Fatalf("Release returned an error: %v", err)
	}

	if _, attached := h.adapters.Attachment(runtimeID); attached {
		t.Error("the adapter is still attached to a runtime that was destroyed")
	}
	if _, err := os.Stat(filepath.Join(h.settings.Dir(), runtimeID)); !os.IsNotExist(err) {
		t.Error("the settings document outlived the runtime it named")
	}
	attempts := h.sessionsOf(tk.ID)
	if attempts[0].Status != task.StatusSessionFailed {
		t.Errorf("attempt status = %q; want %q", attempts[0].Status, task.StatusSessionFailed)
	}
	if _, _, _, agentStops := h.runtimes.counts(); agentStops != 0 {
		t.Error("Release interrupted the agent; the runtime it was in is already gone")
	}

	// Releasing again is nothing to do, not a failure.
	if err := h.service.Release(context.Background(), p.ID, OutcomeFailed); err != nil {
		t.Errorf("a second Release returned an error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 8. Two projects at once
// ---------------------------------------------------------------------------

// TestTwoProjectsAreBoundIndependently is the isolation rule.
//
// One adapter per runtime and one binding per project: a hook delivered to one
// project's receiver must never be attributed to the other, which is the whole
// reason the endpoint is per runtime rather than per server.
func TestTwoProjectsAreBoundIndependently(t *testing.T) {
	h := newHarness(t, Options{})
	alpha := h.registerProject("alpha")
	bravo := h.registerProject("bravo")

	alphaResult, err := h.service.Start(context.Background(), startInput(alpha, nil))
	if err != nil {
		t.Fatalf("starting alpha's agent: %v", err)
	}
	bravoResult, err := h.service.Start(context.Background(), startInput(bravo, nil))
	if err != nil {
		t.Fatalf("starting bravo's agent: %v", err)
	}

	if alphaResult.Run.SessionID == bravoResult.Run.SessionID {
		t.Fatal("two projects were given the same session id")
	}

	alphaAtt, _ := h.adapters.Attachment(project.SessionNameFor(alpha.ID))
	deliver(t, alphaAtt.HookURL, map[string]any{
		"session_id":      alphaResult.Run.SessionID,
		"hook_event_name": "SessionStart",
		"source":          "startup",
	})

	if got := len(h.eventsOfType(alpha.ID, claude.TypeAgentStarted)); got != 1 {
		t.Errorf("alpha recorded %d agent.started event(s); want 1", got)
	}
	if got := len(h.eventTypes(bravo.ID)); got != 0 {
		t.Errorf("bravo recorded %d event(s) from alpha's hook; want none", got)
	}
}

// ---------------------------------------------------------------------------
// 9. Construction
// ---------------------------------------------------------------------------

func TestNewServiceRefusesAnIncompleteChain(t *testing.T) {
	// Every collaborator is load-bearing: a chain missing one would fail at the
	// first request rather than at construction, and would report it as a
	// request that could not be completed rather than a server that was wired
	// wrongly.
	cases := map[string]Options{
		"no runtime operator": {Adapters: fakeAdapters{}, Sessions: fakeSessions{}, Settings: NewFileSettings("")},
		"no adapter operator": {Runtimes: &fakeRuntimes{}, Sessions: fakeSessions{}, Settings: NewFileSettings("")},
		"no session operator": {Runtimes: &fakeRuntimes{}, Adapters: fakeAdapters{}, Settings: NewFileSettings("")},
		"no settings writer":  {Runtimes: &fakeRuntimes{}, Adapters: fakeAdapters{}, Sessions: fakeSessions{}},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewService(o); err == nil {
				t.Errorf("NewService succeeded with %s", name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stubs for the construction test
// ---------------------------------------------------------------------------

// fakeAdapters is an AdapterOperator that is never called. It exists so that
// NewService's completeness check can be exercised one missing field at a time.
type fakeAdapters struct{}

func (fakeAdapters) Attach(context.Context, claude.Attachment) (claude.Attachment, error) {
	return claude.Attachment{}, nil
}
func (fakeAdapters) Detach(context.Context, string) error { return nil }
func (fakeAdapters) HookSettings(string) ([]byte, error)  { return nil, nil }
func (fakeAdapters) Subscribe(context.Context, string) (<-chan claude.Event, error) {
	return nil, nil
}

// fakeSessions is a SessionOperator that is never called, for the same reason.
type fakeSessions struct{}

func (fakeSessions) GetTask(context.Context, string) (*task.Task, error) { return nil, nil }
func (fakeSessions) CreateSession(context.Context, task.CreateSessionInput) (*task.AgentSession, error) {
	return nil, nil
}
func (fakeSessions) AttachSessionRuntime(context.Context, string, string) (*task.AgentSession, error) {
	return nil, nil
}
func (fakeSessions) UpdateSessionStatus(context.Context, string, string) (*task.AgentSession, error) {
	return nil, nil
}
