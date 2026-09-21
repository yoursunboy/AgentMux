package agentstate_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/agentstate"
	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/storage"
	"github.com/kutonlagos/agentmux/internal/task"
)

// The projection is tested against a real SQLite database.
//
// It has to be: the whole of what makes it correct under replay, under
// concurrency and across a restart lives in one conditional SQL statement, and
// a fake repository would be a fake of the thing under test. What is faked is
// only what a projection does not decide - the event log it reads during a
// rebuild, and the project list it walks.
//
// The tests are in an external package because internal/storage implements the
// repository this service is handed, and a test in the same package could not
// import it without a cycle.

// testClock is the fixed time every test's rows are stamped with.
var testClock = time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testClock }

// at returns a time offset from the test clock, for building an ordered log.
func at(seconds int) time.Time { return testClock.Add(time.Duration(seconds) * time.Second) }

const (
	testProject = "p_00000000000000000001"
	otherProj   = "p_00000000000000000002"
	testRuntime = "amx-" + testProject
	otherRun    = "amx-" + otherProj
	testAttempt = "sess_00000000000000000001"
	otherTry    = "sess_00000000000000000002"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

type harness struct {
	t       *testing.T
	store   *storage.Store
	service *agentstate.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store, err := storage.Open(context.Background(), storage.OpenOptions{
		Path: filepath.Join(t.TempDir(), "agentmux.db"),
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

	service, err := agentstate.NewService(agentstate.Options{
		Repository: store.AgentStates(),
		Logger:     discardLogger(),
		Now:        fixedNow,
	})
	if err != nil {
		t.Fatalf("agentstate.NewService returned an error: %v", err)
	}
	return &harness{t: t, store: store, service: service}
}

// project folds one event.
func (h *harness) project(ev *event.AgentEvent) {
	h.t.Helper()
	if err := h.service.Project(context.Background(), ev); err != nil {
		h.t.Fatalf("Project(%s) returned an error: %v", ev.Type, err)
	}
}

// state reads one attempt's state.
func (h *harness) state(agentSessionID string) agentstate.AgentState {
	h.t.Helper()
	state, err := h.service.State(context.Background(), agentSessionID)
	if err != nil {
		h.t.Fatalf("State(%s) returned an error: %v", agentSessionID, err)
	}
	return state
}

// eventFor builds an event of a type, with a payload rendered from a map.
func eventFor(id, eventType, projectID, runtimeID string, payload map[string]any, when time.Time) *event.AgentEvent {
	var encoded json.RawMessage
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		encoded = body
	}
	source := event.SourceAgent
	if len(eventType) >= 8 && eventType[:8] == "session." {
		source = event.SourceUser
	}
	return &event.AgentEvent{
		ID:        id,
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Type:      eventType,
		Source:    source,
		Payload:   encoded,
		CreatedAt: when,
	}
}

// bound brings an attempt into existence and binds it to a runtime, the way the
// event log does: a session.created, then a session.status_changed carrying
// both the attempt and the runtime.
func (h *harness) bound(projectID, runtimeID, attemptID string, base int) {
	h.t.Helper()
	h.project(eventFor("evt_created_"+attemptID, task.TypeSessionCreated, projectID, "",
		map[string]any{"taskId": "task_x", "agentSession": attemptID}, at(base)))
	h.project(eventFor("evt_running_"+attemptID, task.TypeSessionStatusChanged, projectID, runtimeID,
		map[string]any{
			"taskId": "task_x", "agentSession": attemptID,
			"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
		}, at(base+1)))
}

// ---------------------------------------------------------------------------
// 1. The mapping
// ---------------------------------------------------------------------------

// TestAnAgentEventProjectsIntoTheStatusItAsserts is §六's table, one row per
// case.
func TestAnAgentEventProjectsIntoTheStatusItAsserts(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		want      agentstate.Status
		lastEvent string
	}{
		{"a session began", claude.TypeAgentStarted, agentstate.StatusRunning, claude.TypeAgentStarted},
		{"a prompt was submitted", claude.TypeAgentPromptSubmitted, agentstate.StatusRunning, claude.TypeAgentPromptSubmitted},
		{"a permission was asked for", claude.TypeAgentPermissionRequested, agentstate.StatusWaitingPermission, claude.TypeAgentPermissionRequested},
		{"a turn succeeded", claude.TypeAgentCompleted, agentstate.StatusCompleted, claude.TypeAgentCompleted},
		{"a turn failed", claude.TypeAgentFailed, agentstate.StatusFailed, claude.TypeAgentFailed},
		{"a session ended", claude.TypeAgentSessionEnded, agentstate.StatusStopped, claude.TypeAgentSessionEnded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.bound(testProject, testRuntime, testAttempt, 0)
			h.project(eventFor("evt_x", tc.eventType, testProject, testRuntime, nil, at(10)))

			state := h.state(testAttempt)
			if state.Status != tc.want {
				t.Errorf("%s projected to %s; want %s", tc.eventType, state.Status, tc.want)
			}
			if state.LastEvent != tc.lastEvent {
				t.Errorf("lastEvent = %q; want %q", state.LastEvent, tc.lastEvent)
			}
			if !state.LastEventAt.Equal(at(10)) {
				t.Errorf("lastEventAt = %s; want %s", state.LastEventAt, at(10))
			}
			if state.RuntimeID != testRuntime {
				t.Errorf("runtimeId = %q; want %q", state.RuntimeID, testRuntime)
			}
			if state.ProjectID != testProject {
				t.Errorf("projectId = %q; want %q", state.ProjectID, testProject)
			}
		})
	}
}

// TestACompletionCandidateRecordsItselfWithoutMovingTheState is the rule §六
// draws: the model stopping is not the work succeeding.
func TestACompletionCandidateRecordsItselfWithoutMovingTheState(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)
	h.project(eventFor("evt_1", claude.TypeAgentStarted, testProject, testRuntime, nil, at(10)))
	h.project(eventFor("evt_2", claude.TypeAgentCompletedCandidate, testProject, testRuntime, nil, at(11)))

	state := h.state(testAttempt)
	if state.Status != agentstate.StatusRunning {
		t.Errorf("status = %s after a completion candidate; want RUNNING: "+
			"the model stopping is not the turn succeeding", state.Status)
	}
	if state.LastEvent != claude.TypeAgentCompletedCandidate {
		t.Errorf("lastEvent = %q; want the candidate: the row must still say what was last seen",
			state.LastEvent)
	}
}

// TestTheSessionEventsCreateAndBindTheAttempt is where the attempt comes from
// at all: an agent event names a runtime and no attempt.
func TestTheSessionEventsCreateAndBindTheAttempt(t *testing.T) {
	h := newHarness(t)

	h.project(eventFor("evt_1", task.TypeSessionCreated, testProject, "",
		map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)))

	created := h.state(testAttempt)
	if created.Status != agentstate.StatusCreated {
		t.Errorf("status = %s after session.created; want CREATED", created.Status)
	}
	if created.RuntimeID != "" {
		t.Errorf("runtimeId = %q; an attempt is created before its runtime exists", created.RuntimeID)
	}

	h.project(eventFor("evt_2", task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
		}, at(1)))

	bound := h.state(testAttempt)
	if bound.RuntimeID != testRuntime {
		t.Errorf("runtimeId = %q after the status change; want %q", bound.RuntimeID, testRuntime)
	}
	if bound.Status != agentstate.StatusRunning {
		t.Errorf("status = %s; want RUNNING", bound.Status)
	}
}

// TestABindingIsNeverCleared is the case a plain assignment would get wrong: an
// event that carries no runtime must not erase one a previous event
// established.
func TestABindingIsNeverCleared(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)

	// A later session event with no runtime - a status change before a runtime
	// is attached, or one whose payload was assembled without it.
	h.project(eventFor("evt_later", task.TypeSessionStatusChanged, testProject, "",
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionRunning, "to": task.StatusSessionCompleted,
		}, at(5)))

	state := h.state(testAttempt)
	if state.RuntimeID != testRuntime {
		t.Errorf("runtimeId = %q; want %q: a binding is set once and never cleared",
			state.RuntimeID, testRuntime)
	}
	if state.Status != agentstate.StatusCompleted {
		t.Errorf("status = %s; want COMPLETED", state.Status)
	}
}

// ---------------------------------------------------------------------------
// 2. Idempotency and ordering
// ---------------------------------------------------------------------------

// TestProjectingTheSameEventTwiceChangesNothing is §十二.
func TestProjectingTheSameEventTwiceChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)

	ev := eventFor("evt_1", claude.TypeAgentPermissionRequested, testProject, testRuntime, nil, at(10))
	h.project(ev)
	first := h.state(testAttempt)

	h.project(ev)
	h.project(ev)
	again := h.state(testAttempt)

	if again != first {
		t.Errorf("the state changed on replay:\n first = %+v\n again = %+v", first, again)
	}
}

// TestAnOlderEventDoesNotOverwriteANewerOne is what makes the projection
// order-independent: replay is not required to arrive in order.
func TestAnOlderEventDoesNotOverwriteANewerOne(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)

	// The later event is projected first.
	h.project(eventFor("evt_late", claude.TypeAgentCompleted, testProject, testRuntime, nil, at(20)))
	// Then the earlier one arrives, as a rebuild of an out-of-order page would.
	h.project(eventFor("evt_early", claude.TypeAgentStarted, testProject, testRuntime, nil, at(10)))

	state := h.state(testAttempt)
	if state.Status != agentstate.StatusCompleted {
		t.Errorf("status = %s; want COMPLETED: an event older than the row must not move it back",
			state.Status)
	}
	if state.LastEvent != claude.TypeAgentCompleted {
		t.Errorf("lastEvent = %q; want the later event", state.LastEvent)
	}
}

// TestEventsProjectedConcurrentlyAgree is §十三 without a lock in the service:
// the ordering is settled by the events' own times, so whichever order the
// writes land in the answer is the same.
func TestEventsProjectedConcurrentlyAgree(t *testing.T) {
	const rounds = 20

	for round := 0; round < rounds; round++ {
		h := newHarness(t)
		h.bound(testProject, testRuntime, testAttempt, 0)

		earlier := eventFor("evt_a", claude.TypeAgentStarted, testProject, testRuntime, nil, at(10))
		later := eventFor("evt_b", claude.TypeAgentFailed, testProject, testRuntime, nil, at(11))

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = h.service.Project(context.Background(), later) }()
		go func() { defer wg.Done(); _ = h.service.Project(context.Background(), earlier) }()
		wg.Wait()

		if got := h.state(testAttempt); got.Status != agentstate.StatusFailed {
			t.Fatalf("round %d: status = %s; want FAILED regardless of which write landed first",
				round, got.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Scope
// ---------------------------------------------------------------------------

// TestEventsDoNotCrossProjects is the isolation rule: a runtime belongs to one
// project, and a state is found by the runtime an event names.
func TestEventsDoNotCrossProjects(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)
	h.bound(otherProj, otherRun, otherTry, 0)

	h.project(eventFor("evt_1", claude.TypeAgentPermissionRequested, testProject, testRuntime, nil, at(10)))
	h.project(eventFor("evt_2", claude.TypeAgentFailed, otherProj, otherRun, nil, at(10)))

	mine := h.state(testAttempt)
	if mine.Status != agentstate.StatusWaitingPermission {
		t.Errorf("the first project's state = %s; want WAITING_PERMISSION", mine.Status)
	}
	if mine.ProjectID != testProject {
		t.Errorf("projectId = %q; want %q", mine.ProjectID, testProject)
	}

	theirs := h.state(otherTry)
	if theirs.Status != agentstate.StatusFailed {
		t.Errorf("the second project's state = %s; want FAILED", theirs.Status)
	}
	if theirs.ProjectID != otherProj {
		t.Errorf("projectId = %q; want %q", theirs.ProjectID, otherProj)
	}

	list, err := h.service.ListByProject(context.Background(), testProject, 0)
	if err != nil {
		t.Fatalf("ListByProject returned an error: %v", err)
	}
	if len(list) != 1 || list[0].AgentSessionID != testAttempt {
		t.Errorf("listing project %s returned %d state(s); want only its own", testProject, len(list))
	}
}

// TestAnAgentEventWithNoBoundAttemptIsNotProjected is the limitation stated
// rather than hidden: an agent started without a task has no attempt for its
// events to be the state of.
func TestAnAgentEventWithNoBoundAttemptIsNotProjected(t *testing.T) {
	h := newHarness(t)

	// Nothing was ever bound to this runtime.
	h.project(eventFor("evt_1", claude.TypeAgentStarted, testProject, testRuntime, nil, at(10)))

	if _, err := h.service.State(context.Background(), testAttempt); !agentstate.IsCode(err, agentstate.CodeNotFound) {
		t.Errorf("State() = %v; want %q - no attempt is bound, so nothing should have been written",
			err, agentstate.CodeNotFound)
	}
	states, err := h.service.ListByProject(context.Background(), testProject, 0)
	if err != nil {
		t.Fatalf("ListByProject returned an error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("%d state(s) were written for an unbound runtime; want none", len(states))
	}
}

// TestTheNewestAttemptOnARuntimeOwnsItsEvents is the second-attempt case: a
// runtime hosts one agent at a time, and the binding moves on.
func TestTheNewestAttemptOnARuntimeOwnsItsEvents(t *testing.T) {
	h := newHarness(t)

	first := "sess_00000000000000000001"
	second := "sess_00000000000000000002"

	h.bound(testProject, testRuntime, first, 0)
	h.project(eventFor("evt_1", task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": first,
			"from": task.StatusSessionRunning, "to": task.StatusSessionCancelled,
		}, at(5)))

	h.bound(testProject, testRuntime, second, 10)
	h.project(eventFor("evt_2", claude.TypeAgentStarted, testProject, testRuntime, nil, at(20)))

	if got := h.state(second); got.Status != agentstate.StatusRunning {
		t.Errorf("the second attempt = %s; want RUNNING", got.Status)
	}
	if got := h.state(first); got.Status != agentstate.StatusStopped {
		t.Errorf("the first attempt = %s; want STOPPED - it was cancelled and must not move again",
			got.Status)
	}
}

// ---------------------------------------------------------------------------
// 4. Restart
// ---------------------------------------------------------------------------

// TestStateSurvivesARestart is §十九's restart case: a state is stored, not
// computed on demand, so a new process reads what the old one wrote.
func TestStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)
	h.project(eventFor("evt_1", claude.TypeAgentPermissionRequested, testProject, testRuntime, nil, at(10)))
	before := h.state(testAttempt)

	// A second service over the same database, which is what a restarted server
	// builds.
	restarted, err := agentstate.NewService(agentstate.Options{
		Repository: h.store.AgentStates(),
		Logger:     discardLogger(),
		Now:        func() time.Time { return at(999) },
	})
	if err != nil {
		t.Fatalf("building a second service failed: %v", err)
	}

	after, err := restarted.State(context.Background(), testAttempt)
	if err != nil {
		t.Fatalf("the restarted service could not read the state: %v", err)
	}
	if after != before {
		t.Errorf("the state changed across a restart:\n before = %+v\n after  = %+v", before, after)
	}
}

// ---------------------------------------------------------------------------
// 5. Rebuild
// ---------------------------------------------------------------------------

// fakeEvents is an EventReader over a slice.
type fakeEvents struct {
	byProject map[string][]*event.AgentEvent
	err       error
}

func (f *fakeEvents) ListProject(_ context.Context, projectID string, opts event.ListOptions) (event.Page, error) {
	if f.err != nil {
		return event.Page{}, f.err
	}
	all := f.byProject[projectID]
	start := 0
	if opts.Before != "" {
		for i, ev := range all {
			if ev.ID == opts.Before {
				start = i + 1
				break
			}
		}
	}
	limit := opts.Limit
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	page := event.Page{Events: all[start:end]}
	if end < len(all) && len(page.Events) > 0 {
		page.NextBefore = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

// fakeProjects is a ProjectLister over a slice.
type fakeProjects struct {
	ids []string
	err error
}

func (f *fakeProjects) List(context.Context, project.ListFilter) ([]*project.Project, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*project.Project, 0, len(f.ids))
	for _, id := range f.ids {
		out = append(out, &project.Project{ID: id})
	}
	return out, nil
}

// withRebuild gives a harness an event log and a project list to rebuild from.
func (h *harness) withRebuild(events *fakeEvents, projects *fakeProjects) *agentstate.Service {
	h.t.Helper()
	service, err := agentstate.NewService(agentstate.Options{
		Repository: h.store.AgentStates(),
		Events:     events,
		Projects:   projects,
		Logger:     discardLogger(),
		Now:        fixedNow,
	})
	if err != nil {
		h.t.Fatalf("agentstate.NewService returned an error: %v", err)
	}
	return service
}

// logOf builds an event log in newest-first order, which is how the store
// returns one.
func logOf(events ...*event.AgentEvent) []*event.AgentEvent {
	out := make([]*event.AgentEvent, len(events))
	for i, ev := range events {
		out[len(events)-1-i] = ev
	}
	return out
}

// TestRebuildReproducesTheLiveProjection is the property the whole design rests
// on: the states are derived from the log, so recomputing them from the log
// gives what the live projection gave.
func TestRebuildReproducesTheLiveProjection(t *testing.T) {
	h := newHarness(t)

	created := eventFor("evt_1", task.TypeSessionCreated, testProject, "",
		map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0))
	running := eventFor("evt_2", task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
		}, at(1))
	started := eventFor("evt_3", claude.TypeAgentStarted, testProject, testRuntime, nil, at(2))
	asked := eventFor("evt_4", claude.TypeAgentPermissionRequested, testProject, testRuntime, nil, at(3))
	stopped := eventFor("evt_5", claude.TypeAgentSessionEnded, testProject, testRuntime, nil, at(4))

	// Live: the events are projected as they arrive.
	for _, ev := range []*event.AgentEvent{created, running, started, asked, stopped} {
		h.project(ev)
	}
	live := h.state(testAttempt)

	// Then the table is thrown away and rebuilt from the log alone.
	service := h.withRebuild(
		&fakeEvents{byProject: map[string][]*event.AgentEvent{
			testProject: logOf(created, running, started, asked, stopped),
		}},
		&fakeProjects{ids: []string{testProject}},
	)
	report, err := service.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("Rebuild returned an error: %v", err)
	}
	if report.Events != 5 {
		t.Errorf("the rebuild read %d event(s); want 5", report.Events)
	}
	if report.States != 1 {
		t.Errorf("the rebuild produced %d state(s); want 1", report.States)
	}

	rebuilt, err := service.State(context.Background(), testAttempt)
	if err != nil {
		t.Fatalf("reading the rebuilt state failed: %v", err)
	}
	if rebuilt != live {
		t.Errorf("the rebuild disagrees with the live projection:\n live    = %+v\n rebuilt = %+v",
			live, rebuilt)
	}
	if rebuilt.Status != agentstate.StatusStopped {
		t.Errorf("rebuilt status = %s; want STOPPED", rebuilt.Status)
	}
}

// TestRebuildIsIdempotent runs it twice.
func TestRebuildIsIdempotent(t *testing.T) {
	h := newHarness(t)

	created := eventFor("evt_1", task.TypeSessionCreated, testProject, "",
		map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0))
	running := eventFor("evt_2", task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
		}, at(1))
	started := eventFor("evt_3", claude.TypeAgentStarted, testProject, testRuntime, nil, at(2))

	service := h.withRebuild(
		&fakeEvents{byProject: map[string][]*event.AgentEvent{
			testProject: logOf(created, running, started),
		}},
		&fakeProjects{ids: []string{testProject}},
	)

	ctx := context.Background()
	if _, err := service.Rebuild(ctx); err != nil {
		t.Fatalf("the first Rebuild returned an error: %v", err)
	}
	first, err := service.State(ctx, testAttempt)
	if err != nil {
		t.Fatalf("reading the first rebuild failed: %v", err)
	}

	if _, err := service.Rebuild(ctx); err != nil {
		t.Fatalf("the second Rebuild returned an error: %v", err)
	}
	second, err := service.State(ctx, testAttempt)
	if err != nil {
		t.Fatalf("reading the second rebuild failed: %v", err)
	}

	if first != second {
		t.Errorf("two rebuilds disagree:\n first  = %+v\n second = %+v", first, second)
	}
}

// TestRebuildIfNeededOnlyRunsWhenTheProjectionIsEmpty is the start-up rule.
func TestRebuildIfNeededOnlyRunsWhenTheProjectionIsEmpty(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	events := &fakeEvents{byProject: map[string][]*event.AgentEvent{
		testProject: logOf(
			eventFor("evt_1", task.TypeSessionCreated, testProject, "",
				map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)),
		),
	}}
	service := h.withRebuild(events, &fakeProjects{ids: []string{testProject}})

	report, ran, err := service.RebuildIfNeeded(ctx)
	if err != nil {
		t.Fatalf("RebuildIfNeeded returned an error: %v", err)
	}
	if !ran {
		t.Fatal("RebuildIfNeeded did not run on an empty projection")
	}
	if report.States != 1 {
		t.Errorf("the rebuild produced %d state(s); want 1", report.States)
	}

	// A second call finds a populated table and leaves it alone.
	_, ran, err = service.RebuildIfNeeded(ctx)
	if err != nil {
		t.Fatalf("the second RebuildIfNeeded returned an error: %v", err)
	}
	if ran {
		t.Error("RebuildIfNeeded ran again over a projection that already held a state")
	}
}

// TestARebuildThatCannotReadTheLogLeavesTheProjectionAlone checks that a
// failure does not half-empty the table.
func TestARebuildThatCannotReadTheLogLeavesTheProjectionAlone(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)
	before := h.state(testAttempt)

	service := h.withRebuild(
		&fakeEvents{err: errors.New("the log went away")},
		&fakeProjects{ids: []string{testProject}},
	)
	if _, err := service.Rebuild(context.Background()); err == nil {
		t.Fatal("Rebuild succeeded over a log it could not read")
	}

	after, err := service.State(context.Background(), testAttempt)
	if err != nil {
		t.Fatalf("the state was lost by a failed rebuild: %v", err)
	}
	if after != before {
		t.Errorf("a failed rebuild changed the state:\n before = %+v\n after  = %+v", before, after)
	}
}

// ---------------------------------------------------------------------------
// 6. The event service is the only way in
// ---------------------------------------------------------------------------

// TestTheEventServiceIsTheOnlyPathIntoState is §九 and §十 end to end: a real
// event service with the projection installed writes a state as a side effect
// of storing an event, and nothing else does.
func TestTheEventServiceIsTheOnlyPathIntoState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventLog, err := event.NewService(event.Options{
		Repository: h.store.Events(),
		Logger:     discardLogger(),
		Now:        fixedNow,
	})
	if err != nil {
		t.Fatalf("event.NewService returned an error: %v", err)
	}
	eventLog.SetProjector(h.service)

	// A project row, because an event names a project and the store checks it.
	if err := h.store.Projects().Create(ctx, &project.Project{
		ID: testProject, Name: "checkout", HostPath: "/tmp/checkout",
		RuntimePath: "/tmp/checkout", Status: project.StatusStopped,
		CreatedAt: testClock, UpdatedAt: testClock,
	}); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}

	emit := func(eventType, runtimeID string, payload map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encoding the payload failed: %v", err)
		}
		if _, err := eventLog.CreateEvent(ctx, testProject, runtimeID, eventType, event.SourceAgent, body); err != nil {
			t.Fatalf("CreateEvent(%s) returned an error: %v", eventType, err)
		}
	}

	emit(task.TypeSessionCreated, "", map[string]any{"taskId": "task_x", "agentSession": testAttempt})
	emit(task.TypeSessionStatusChanged, testRuntime, map[string]any{
		"taskId": "task_x", "agentSession": testAttempt,
		"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
	})
	emit(claude.TypeAgentPermissionRequested, testRuntime, map[string]any{"event": "PermissionRequest", "tool": "Bash"})

	state := h.state(testAttempt)
	if state.Status != agentstate.StatusWaitingPermission {
		t.Errorf("status = %s after a permission request through the event service; want WAITING_PERMISSION",
			state.Status)
	}
	if state.RuntimeID != testRuntime {
		t.Errorf("runtimeId = %q; want %q", state.RuntimeID, testRuntime)
	}
	if state.LastEvent != claude.TypeAgentPermissionRequested {
		t.Errorf("lastEvent = %q; want the permission request", state.LastEvent)
	}
}

// TestAnEventTheLogRefusesWritesNoState is the same path's other half: nothing
// is derived from an event that was never stored.
func TestAnEventTheLogRefusesWritesNoState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventLog, err := event.NewService(event.Options{
		Repository: h.store.Events(),
		Logger:     discardLogger(),
		Now:        fixedNow,
	})
	if err != nil {
		t.Fatalf("event.NewService returned an error: %v", err)
	}
	eventLog.SetProjector(h.service)

	if err := h.store.Projects().Create(ctx, &project.Project{
		ID: testProject, Name: "checkout", HostPath: "/tmp/checkout",
		RuntimePath: "/tmp/checkout", Status: project.StatusStopped,
		CreatedAt: testClock, UpdatedAt: testClock,
	}); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}

	// A payload the event service refuses: `sessionId` names a session
	// credential as far as CheckPayload is concerned.
	body := json.RawMessage(`{"taskId":"task_x","sessionId":"sess_x"}`)
	if _, err := eventLog.CreateEvent(ctx, testProject, "", task.TypeSessionCreated, event.SourceUser, body); err == nil {
		t.Fatal("CreateEvent accepted a payload naming a session credential")
	}

	states, err := h.service.ListByProject(ctx, testProject, 0)
	if err != nil {
		t.Fatalf("ListByProject returned an error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("%d state(s) were derived from an event the log refused; want none", len(states))
	}
}

// ---------------------------------------------------------------------------
// 7. The model
// ---------------------------------------------------------------------------

func TestAnUnobservedAttemptHasNoState(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.State(context.Background(), testAttempt); !agentstate.IsCode(err, agentstate.CodeNotFound) {
		t.Errorf("State() = %v; want %q", err, agentstate.CodeNotFound)
	}
	if _, err := h.service.State(context.Background(), ""); !agentstate.IsCode(err, agentstate.CodeInvalidState) {
		t.Errorf("State(\"\") = %v; want %q", err, agentstate.CodeInvalidState)
	}
}

func TestTheStatusVocabularyIsClosed(t *testing.T) {
	want := []agentstate.Status{
		agentstate.StatusCreated, agentstate.StatusRunning, agentstate.StatusWaitingInput,
		agentstate.StatusWaitingPermission, agentstate.StatusCompleted,
		agentstate.StatusFailed, agentstate.StatusStopped,
	}
	got := agentstate.Statuses()
	if len(got) != len(want) {
		t.Fatalf("Statuses() returned %d statuses; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Statuses()[%d] = %q; want %q", i, got[i], want[i])
		}
		if !agentstate.ValidStatus(string(want[i])) {
			t.Errorf("ValidStatus(%q) = false", want[i])
		}
	}
	if agentstate.ValidStatus("BANANA") {
		t.Error("ValidStatus accepted a status that does not exist")
	}

	// Terminal is a description, not a gate: it is what the API reports and is
	// never consulted to refuse an event.
	for _, s := range []agentstate.Status{agentstate.StatusCompleted, agentstate.StatusFailed, agentstate.StatusStopped} {
		if !s.Terminal() {
			t.Errorf("%s.Terminal() = false; want true", s)
		}
	}
	for _, s := range []agentstate.Status{agentstate.StatusCreated, agentstate.StatusRunning, agentstate.StatusWaitingPermission} {
		if s.Terminal() {
			t.Errorf("%s.Terminal() = true; want false", s)
		}
	}
}

func TestAStateMustValidateBeforeItIsStored(t *testing.T) {
	cases := map[string]agentstate.AgentState{
		"no attempt": {ProjectID: testProject, Status: agentstate.StatusRunning,
			LastEvent: claude.TypeAgentStarted, LastEventAt: testClock},
		"no project": {AgentSessionID: testAttempt, Status: agentstate.StatusRunning,
			LastEvent: claude.TypeAgentStarted, LastEventAt: testClock},
		"unknown status": {AgentSessionID: testAttempt, ProjectID: testProject, Status: "BANANA",
			LastEvent: claude.TypeAgentStarted, LastEventAt: testClock},
		"no last event": {AgentSessionID: testAttempt, ProjectID: testProject,
			Status: agentstate.StatusRunning, LastEventAt: testClock},
		"no last event time": {AgentSessionID: testAttempt, ProjectID: testProject,
			Status: agentstate.StatusRunning, LastEvent: claude.TypeAgentStarted},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			if err := state.Validate(); err == nil {
				t.Errorf("Validate accepted a state with %s", name)
			}
		})
	}

	good := agentstate.AgentState{
		AgentSessionID: testAttempt, ProjectID: testProject, RuntimeID: testRuntime,
		Status: agentstate.StatusRunning, LastEvent: claude.TypeAgentStarted,
		LastEventAt: testClock, UpdatedAt: testClock,
	}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate rejected a valid state: %v", err)
	}
}

func TestNewServiceRefusesAnIncompleteConfiguration(t *testing.T) {
	if _, err := agentstate.NewService(agentstate.Options{}); err == nil {
		t.Error("NewService succeeded with no repository")
	}
}

// TestProjectionSurvivesACancelledRequest is the case where the event was
// stored and the call that produced it is already over.
//
// Every event is written on the context of whatever produced it - an HTTP
// request, a hook delivery - and those end. A projection that honoured that
// cancellation would silently skip exactly the last event of an attempt, which
// is the one that decides what its state is.
func TestProjectionSurvivesACancelledRequest(t *testing.T) {
	h := newHarness(t)
	h.bound(testProject, testRuntime, testAttempt, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already gone

	ev := eventFor("evt_last", claude.TypeAgentFailed, testProject, testRuntime, nil, at(10))
	if err := h.service.Project(ctx, ev); err != nil {
		t.Fatalf("Project on a cancelled context returned an error: %v", err)
	}

	state := h.state(testAttempt)
	if state.Status != agentstate.StatusFailed {
		t.Errorf("status = %s; want FAILED - a stored event is projected whatever "+
			"became of the request that produced it", state.Status)
	}
}

// TestAnUnknownSessionStatusStillBindsTheRuntime is the defensive path.
//
// The task service validates a transition before it emits one, so a status this
// build does not know cannot reach here today. It is handled anyway because the
// obvious handling - skip the event - would drop the binding with the status,
// and a runtime that stopped finding its attempt would stop projecting every
// agent event that followed.
func TestAnUnknownSessionStatusStillBindsTheRuntime(t *testing.T) {
	h := newHarness(t)

	// The attempt exists, from its creation event.
	h.project(eventFor("evt_1", task.TypeSessionCreated, testProject, "",
		map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)))

	// A status change carrying a status this build has never heard of.
	h.project(eventFor("evt_2", task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionCreated, "to": "SOME_LATER_STATUS",
		}, at(1)))

	state := h.state(testAttempt)
	if state.RuntimeID != testRuntime {
		t.Errorf("runtimeId = %q; want %q - the binding must survive a status the "+
			"projection cannot read", state.RuntimeID, testRuntime)
	}
	if state.Status != agentstate.StatusCreated {
		t.Errorf("status = %s; want CREATED - an unreadable status moves nothing", state.Status)
	}

	// And the binding works: an agent event now finds the attempt.
	h.project(eventFor("evt_3", claude.TypeAgentStarted, testProject, testRuntime, nil, at(2)))
	if got := h.state(testAttempt); got.Status != agentstate.StatusRunning {
		t.Errorf("status = %s after an agent event; want RUNNING", got.Status)
	}
}
