package attention_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/agentstate"
	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/storage"
	"github.com/kutonlagos/agentmux/internal/task"
)

// The projection is tested against a real SQLite database and the real agent
// state projection, because it reads one and writes through the other.
//
// What is faked is only what it does not decide: the event log it reads during
// a rebuild, and the project list it walks. Those are the same two fakes the
// state projection's suite uses, and for the same reason.
//
// The tests are in an external package because internal/storage implements the
// repository this service is handed.

var testClock = time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)

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
	t          *testing.T
	store      *storage.Store
	states     *agentstate.Service
	attention  *attention.Service
	events     *fakeEvents
	projects   *fakeProjects
	configured bool
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

	states, err := agentstate.NewService(agentstate.Options{
		Repository: store.AgentStates(),
		Logger:     discardLogger(),
		Now:        func() time.Time { return testClock },
	})
	if err != nil {
		t.Fatalf("agentstate.NewService returned an error: %v", err)
	}

	h := &harness{t: t, store: store, states: states}
	h.events = &fakeEvents{}
	h.projects = &fakeProjects{}
	h.build()
	return h
}

// build constructs the attention service over the current fakes.
func (h *harness) build() {
	h.t.Helper()
	service, err := attention.NewService(attention.Options{
		Repository: h.store.Attention(),
		States:     h.states,
		Events:     h.events,
		Projects:   h.projects,
		Logger:     discardLogger(),
	})
	if err != nil {
		h.t.Fatalf("attention.NewService returned an error: %v", err)
	}
	h.attention = service
}

// project folds one event through both projections, in the order the server
// installs them: the state first, because attention reads it.
func (h *harness) project(ev *event.AgentEvent) {
	h.t.Helper()
	if err := h.states.Project(context.Background(), ev); err != nil {
		h.t.Fatalf("the state projection of %s returned an error: %v", ev.Type, err)
	}
	if err := h.attention.Project(context.Background(), ev); err != nil {
		h.t.Fatalf("the attention projection of %s returned an error: %v", ev.Type, err)
	}
}

// bind brings an attempt into existence and binds it to a runtime, the way the
// log does: a session.created, then a session.status_changed carrying both.
func (h *harness) bind(projectID, runtimeID, attemptID string, base int) {
	h.t.Helper()
	h.project(eventFor(idFor("created-"+attemptID), task.TypeSessionCreated, projectID, "",
		map[string]any{"taskId": "task_x", "agentSession": attemptID}, at(base)))
	h.project(eventFor(idFor("running-"+attemptID), task.TypeSessionStatusChanged, projectID, runtimeID,
		map[string]any{
			"taskId": "task_x", "agentSession": attemptID,
			"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
		}, at(base+1)))
}

// level reads one attempt's attention level.
func (h *harness) level(agentSessionID string) attention.Attention {
	h.t.Helper()
	a, err := h.attention.Attention(context.Background(), agentSessionID)
	if err != nil {
		h.t.Fatalf("Attention(%s) returned an error: %v", agentSessionID, err)
	}
	return a
}

// actions reads one attempt's actions.
func (h *harness) actions(agentSessionID string) []attention.Action {
	h.t.Helper()
	list, err := h.attention.Actions(context.Background(), agentSessionID)
	if err != nil {
		h.t.Fatalf("Actions(%s) returned an error: %v", agentSessionID, err)
	}
	return list
}

// state reads one attempt's agent state, which the permission case asserts
// alongside the attention.
func (h *harness) state(agentSessionID string) agentstate.AgentState {
	h.t.Helper()
	state, err := h.states.State(context.Background(), agentSessionID)
	if err != nil {
		h.t.Fatalf("State(%s) returned an error: %v", agentSessionID, err)
	}
	return state
}

// eventFor builds an event of a type with a rendered payload.
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

// agentEvent builds an `agent.*` event whose identifier has the shape the event
// service mints, so that the action identity derived from it is valid. A short
// id would make every case here fail for a reason that has nothing to do with
// what it is testing.
func agentEvent(seed, eventType, projectID, runtimeID string, when time.Time) *event.AgentEvent {
	return eventFor(idFor(seed), eventType, projectID, runtimeID, nil, when)
}

// idFor turns any seed into an event id of the shape the event service mints.
//
// It hashes rather than pads, because a seed like "created_sess_0000…" contains
// characters an event id cannot: the projection derives an action's identity
// from the event's, and an id that is not an event id makes it refuse - which
// would fail a test for a reason that has nothing to do with what it asserts.
func idFor(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "evt_" + hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------------------
// 1. Permission — state, attention and action together
// ---------------------------------------------------------------------------

// TestAPermissionRequestNeedsSomebody is §十九's first case, and it asserts all
// three layers at once because that is the point of the phase: one event, a
// state, a level and a queued action.
func TestAPermissionRequestNeedsSomebody(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)
	h.project(agentEvent("aa", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))

	// The state.
	if got := h.state(testAttempt); got.Status != agentstate.StatusWaitingPermission {
		t.Errorf("state = %s; want WAITING_PERMISSION", got.Status)
	}

	// The attention.
	a := h.level(testAttempt)
	if a.Level != attention.LevelActionRequired {
		t.Errorf("level = %s; want ACTION_REQUIRED", a.Level)
	}
	if a.Reason != attention.ReasonPermissionRequested {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonPermissionRequested)
	}
	if a.ProjectID != testProject {
		t.Errorf("projectId = %q; want %q", a.ProjectID, testProject)
	}

	// The action.
	list := h.actions(testAttempt)
	if len(list) != 1 {
		t.Fatalf("the attempt has %d action(s); want 1", len(list))
	}
	action := list[0]
	if action.Type != attention.ActionPermissionRequest {
		t.Errorf("action type = %s; want PERMISSION_REQUEST", action.Type)
	}
	if action.Status != attention.ActionPending {
		t.Errorf("action status = %s; want PENDING", action.Status)
	}
	if action.ResolvedAt != nil {
		t.Errorf("a pending action has a resolution time: %v", action.ResolvedAt)
	}
	if action.ID != attention.ActionIDForEvent(idFor("aa")) {
		t.Errorf("action id = %q; want the one derived from its event", action.ID)
	}
}

// TestAPermissionActionCarriesNoDecision is §十二's boundary, asserted on the
// row: the action records that a question was asked and says nothing about what
// the answer might be.
func TestAPermissionActionCarriesNoDecision(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)
	h.project(agentEvent("ab", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))

	action := h.actions(testAttempt)[0]
	encoded, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("could not encode the action: %v", err)
	}

	// Every field of an action, checked against the words a decision would need.
	for _, forbidden := range []string{"allow", "deny", "ALLOW", "DENY", "decision", "approve", "reject"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("an action carries %q, which is a decision this phase does not make:\n%s",
				forbidden, encoded)
		}
	}
	// And the vocabulary itself has no such value.
	for _, status := range attention.ActionStatuses() {
		if status != attention.ActionPending &&
			status != attention.ActionResolved &&
			status != attention.ActionExpired {
			t.Errorf("the action status vocabulary has grown a %q", status)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Failure and completion
// ---------------------------------------------------------------------------

func TestAFailureIsAWarning(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)
	h.project(agentEvent("ac", claude.TypeAgentFailed, testProject, testRuntime, at(10)))

	a := h.level(testAttempt)
	if a.Level != attention.LevelWarning {
		t.Errorf("level = %s; want WARNING", a.Level)
	}
	if a.Reason != attention.ReasonAgentFailed {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonAgentFailed)
	}
	list := h.actions(testAttempt)
	if len(list) != 1 || list[0].Type != attention.ActionViewFailure {
		t.Fatalf("actions = %+v; want one VIEW_FAILURE", list)
	}
}

func TestACompletionIsInformation(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)
	h.project(agentEvent("ad", claude.TypeAgentCompleted, testProject, testRuntime, at(10)))

	a := h.level(testAttempt)
	if a.Level != attention.LevelInfo {
		t.Errorf("level = %s; want INFO", a.Level)
	}
	list := h.actions(testAttempt)
	if len(list) != 1 || list[0].Type != attention.ActionViewCompletion {
		t.Fatalf("actions = %+v; want one VIEW_COMPLETION", list)
	}
}

// TestTheQuietEventsLeaveNothingWaiting is the other half of §七's table: a
// started session and a submitted prompt produce a level and no action.
func TestTheQuietEventsLeaveNothingWaiting(t *testing.T) {
	cases := map[string]string{
		"a session began":        claude.TypeAgentStarted,
		"a prompt was submitted": claude.TypeAgentPromptSubmitted,
		"the model stopped":      claude.TypeAgentCompletedCandidate,
	}
	for name, eventType := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.bind(testProject, testRuntime, testAttempt, 0)
			h.project(agentEvent("ae", eventType, testProject, testRuntime, at(10)))

			if got := h.level(testAttempt).Level; got != attention.LevelNone {
				t.Errorf("level = %s; want NONE", got)
			}
			if list := h.actions(testAttempt); len(list) != 0 {
				t.Errorf("actions = %+v; want none - nothing is waiting on anybody", list)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Duplicates and replay
// ---------------------------------------------------------------------------

// TestTheSameEventRaisesOneAction is §十九's no-duplicate rule, and it is what
// deriving the action identity from the event buys.
func TestTheSameEventRaisesOneAction(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)

	ev := agentEvent("af", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10))
	h.project(ev)
	h.project(ev)
	h.project(ev)

	if got := len(h.actions(testAttempt)); got != 1 {
		t.Errorf("the attempt has %d action(s); want 1: the same event is the same action", got)
	}
}

// TestProjectingTheSameEventTwiceLeavesTheLevelAlone is the attention half.
func TestProjectingTheSameEventTwiceLeavesTheLevelAlone(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)

	ev := agentEvent("b0", claude.TypeAgentFailed, testProject, testRuntime, at(10))
	h.project(ev)
	first := h.level(testAttempt)
	h.project(ev)

	if again := h.level(testAttempt); again != first {
		t.Errorf("the level changed on replay:\n first = %+v\n again = %+v", first, again)
	}
}

// TestAnOlderEventDoesNotOverwriteANewerOne is the guard the attention table
// carries, the same one the state table does.
func TestAnOlderEventDoesNotOverwriteANewerOne(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)

	h.project(agentEvent("b1", claude.TypeAgentFailed, testProject, testRuntime, at(20)))
	h.project(agentEvent("b2", claude.TypeAgentStarted, testProject, testRuntime, at(10)))

	a := h.level(testAttempt)
	if a.Level != attention.LevelWarning {
		t.Errorf("level = %s; want WARNING: an event older than the row must not move it",
			a.Level)
	}
}

// ---------------------------------------------------------------------------
// 4. Resolution
// ---------------------------------------------------------------------------

// TestALaterEventResolvesAPendingPermission is the one thing that takes an
// action off the queue.
//
// Nothing says "the permission was answered" - there is no such event - so the
// next thing that happens to the attempt is the evidence. The alternative was a
// queue that only ever grows.
func TestALaterEventResolvesAPendingPermission(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)

	h.project(agentEvent("b3", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))
	if got := h.actions(testAttempt)[0].Status; got != attention.ActionPending {
		t.Fatalf("the permission action is %s; want PENDING", got)
	}

	// The agent moves on.
	h.project(agentEvent("b4", claude.TypeAgentPromptSubmitted, testProject, testRuntime, at(20)))

	list := h.actions(testAttempt)
	if len(list) != 1 {
		t.Fatalf("the attempt has %d action(s); want 1", len(list))
	}
	if list[0].Status != attention.ActionResolved {
		t.Errorf("the permission action is %s; want RESOLVED", list[0].Status)
	}
	if list[0].ResolvedAt == nil {
		t.Fatal("a resolved action has no resolution time")
	}
	if !list[0].ResolvedAt.Equal(at(20)) {
		t.Errorf("resolvedAt = %s; want the time of the event that resolved it, %s",
			list[0].ResolvedAt, at(20))
	}
	// And the level moved with it.
	if got := h.level(testAttempt).Level; got != attention.LevelNone {
		t.Errorf("level = %s; want NONE - the agent moved on", got)
	}
}

// TestACompletionCandidateDoesNotResolveAPermission is the case that would be
// got wrong by treating every event as evidence.
//
// The model stopping is not the question being answered: a turn can end while a
// tool call is still refused and unanswered.
func TestACompletionCandidateDoesNotResolveAPermission(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)

	h.project(agentEvent("b5", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))
	h.project(agentEvent("b6", claude.TypeAgentCompletedCandidate, testProject, testRuntime, at(20)))

	if got := h.actions(testAttempt)[0].Status; got != attention.ActionPending {
		t.Errorf("the permission action is %s; want PENDING: the model stopping settles nothing",
			got)
	}
}

// TestAFailureResolvesAPendingPermission is the other direction: the agent
// broke, so the question it was waiting on is moot.
func TestAFailureResolvesAPendingPermission(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)

	h.project(agentEvent("b7", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))
	h.project(agentEvent("b8", claude.TypeAgentFailed, testProject, testRuntime, at(20)))

	list := h.actions(testAttempt)
	if len(list) != 2 {
		t.Fatalf("the attempt has %d action(s); want the permission and the failure", len(list))
	}
	for _, action := range list {
		switch action.Type {
		case attention.ActionPermissionRequest:
			if action.Status != attention.ActionResolved {
				t.Errorf("the permission action is %s; want RESOLVED", action.Status)
			}
		case attention.ActionViewFailure:
			if action.Status != attention.ActionPending {
				t.Errorf("the failure action is %s; want PENDING - it was just raised",
					action.Status)
			}
		}
	}
}

// TestAnEventThisPackageDoesNotReadResolvesNothing keeps the rule tight.
func TestAnEventThisPackageDoesNotReadResolvesNothing(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)
	h.project(agentEvent("b9", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))

	// A runtime event, which is about the terminal rather than the agent.
	h.project(eventFor("evt_runtime_event", "runtime.stopped", testProject, testRuntime, nil, at(20)))

	if got := h.actions(testAttempt)[0].Status; got != attention.ActionPending {
		t.Errorf("the permission action is %s; want PENDING - a runtime event settles nothing", got)
	}
}

// ---------------------------------------------------------------------------
// 5. Scope
// ---------------------------------------------------------------------------

func TestAttentionDoesNotCrossProjects(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)
	h.bind(otherProj, otherRun, otherTry, 0)

	h.project(agentEvent("c0", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))
	h.project(agentEvent("c1", claude.TypeAgentFailed, otherProj, otherRun, at(10)))

	if got := h.level(testAttempt).Level; got != attention.LevelActionRequired {
		t.Errorf("the first project's level = %s; want ACTION_REQUIRED", got)
	}
	if got := h.level(otherTry).Level; got != attention.LevelWarning {
		t.Errorf("the second project's level = %s; want WARNING", got)
	}

	list, err := h.attention.ListAttentionByProject(context.Background(), testProject, 0)
	if err != nil {
		t.Fatalf("ListAttentionByProject returned an error: %v", err)
	}
	if len(list) != 1 || list[0].AgentSessionID != testAttempt {
		t.Errorf("the listing returned %d row(s); want only this project's", len(list))
	}

	actions, err := h.attention.ListActionsByProject(context.Background(), testProject, 0)
	if err != nil {
		t.Fatalf("ListActionsByProject returned an error: %v", err)
	}
	if len(actions) != 1 || actions[0].AgentSessionID != testAttempt {
		t.Errorf("the action listing returned %d row(s); want only this project's", len(actions))
	}
}

// TestAnAgentEventWithNoBoundAttemptIsNotProjected is the limitation stated
// rather than hidden: an agent started without a task has no attempt for the
// event to be about.
func TestAnAgentEventWithNoBoundAttemptIsNotProjected(t *testing.T) {
	h := newHarness(t)

	h.project(agentEvent("c2", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(10)))

	if _, err := h.attention.Attention(context.Background(), testAttempt); !attention.IsCode(err, attention.CodeNotFound) {
		t.Errorf("Attention() = %v; want %q", err, attention.CodeNotFound)
	}
	list, err := h.attention.ListAttentionByProject(context.Background(), testProject, 0)
	if err != nil {
		t.Fatalf("ListAttentionByProject returned an error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("%d row(s) were written for an unbound runtime; want none", len(list))
	}
}

// TestAnAttemptExistsWithNoAttention is the row that makes every attempt
// answerable: an attempt that has not started is at NONE rather than absent, so
// a client renders one shape.
func TestAnAttemptIsCreatedAtNoAttention(t *testing.T) {
	h := newHarness(t)

	// Only the creation event, so what is asserted is the row the creation
	// makes rather than the one the bind makes.
	h.project(eventFor(idFor("created-"+testAttempt), task.TypeSessionCreated, testProject, "",
		map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)))

	a := h.level(testAttempt)
	if a.Level != attention.LevelNone {
		t.Errorf("level = %s right after an attempt is created; want NONE", a.Level)
	}
	if a.Reason != attention.ReasonAttemptCreated {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonAttemptCreated)
	}

	// And the bind leaves it quiet, with the reason saying where the attempt is.
	h.project(eventFor(idFor("running-"+testAttempt), task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
		}, at(1)))

	a = h.level(testAttempt)
	if a.Level != attention.LevelNone {
		t.Errorf("level = %s after an attempt is bound; want NONE", a.Level)
	}
	if a.Reason != attention.ReasonAttemptRunning {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonAttemptRunning)
	}
}

// TestAnAttemptThatFailedToLaunchStillNeedsSomebody is the case the phase
// brief's example table does not cover and the projection has to.
//
// An agent that never launched produces no `agent.*` event at all - the
// coordinator marks the attempt FAILED and the only record is a
// `session.status_changed`. Without a mapping for it, a home screen built on
// this projection would say "nothing needs you" about a start that failed.
func TestAnAttemptThatFailedToLaunchStillNeedsSomebody(t *testing.T) {
	h := newHarness(t)

	h.project(eventFor(idFor("created-"+testAttempt), task.TypeSessionCreated, testProject, "",
		map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)))
	h.project(eventFor(idFor("failed-"+testAttempt), task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionCreated, "to": task.StatusSessionFailed,
		}, at(1)))

	a := h.level(testAttempt)
	if a.Level != attention.LevelWarning {
		t.Errorf("level = %s for an attempt that failed to launch; want WARNING", a.Level)
	}
	if a.Reason != attention.ReasonAttemptFailed {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonAttemptFailed)
	}
	list := h.actions(testAttempt)
	if len(list) != 1 || list[0].Type != attention.ActionViewFailure {
		t.Errorf("actions = %+v; want one VIEW_FAILURE", list)
	}
}

// TestACancelledAttemptIsQuiet is the other side of that: somebody stopping an
// attempt is not something to look at.
func TestACancelledAttemptIsQuiet(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)

	h.project(eventFor(idFor("cancelled-"+testAttempt), task.TypeSessionStatusChanged, testProject, testRuntime,
		map[string]any{
			"taskId": "task_x", "agentSession": testAttempt,
			"from": task.StatusSessionRunning, "to": task.StatusSessionCancelled,
		}, at(10)))

	a := h.level(testAttempt)
	if a.Level != attention.LevelNone {
		t.Errorf("level = %s for a cancelled attempt; want NONE", a.Level)
	}
	if a.Reason != attention.ReasonAttemptCancelled {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonAttemptCancelled)
	}
	if list := h.actions(testAttempt); len(list) != 0 {
		t.Errorf("actions = %+v; want none - a cancel leaves nothing waiting", list)
	}
}

// ---------------------------------------------------------------------------
// 6. Rebuild
// ---------------------------------------------------------------------------

// fakeEvents is an EventReader over a map of project timelines.
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

// fakeProjects is a ProjectLister over a list of ids.
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

// logOf reverses a timeline into the order the store returns it.
func logOf(events ...*event.AgentEvent) []*event.AgentEvent {
	out := make([]*event.AgentEvent, len(events))
	for i, ev := range events {
		out[len(events)-1-i] = ev
	}
	return out
}

// TestRebuildReproducesTheLiveProjection is §十七's property: both tables are
// folded from the log, so recomputing them from the log gives what the live
// projection gave - including when an action was resolved.
func TestRebuildReproducesTheLiveProjection(t *testing.T) {
	h := newHarness(t)

	// A log with everything in it: a permission that is later settled, a
	// failure, and a completion.
	events := []*event.AgentEvent{
		eventFor(idFor("created-x"), task.TypeSessionCreated, testProject, "",
			map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)),
		eventFor(idFor("running-x"), task.TypeSessionStatusChanged, testProject, testRuntime,
			map[string]any{
				"taskId": "task_x", "agentSession": testAttempt,
				"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
			}, at(1)),
		agentEvent("d0", claude.TypeAgentStarted, testProject, testRuntime, at(2)),
		agentEvent("d1", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(3)),
		agentEvent("d2", claude.TypeAgentPromptSubmitted, testProject, testRuntime, at(4)),
		agentEvent("d3", claude.TypeAgentFailed, testProject, testRuntime, at(5)),
	}

	for _, ev := range events {
		h.project(ev)
	}
	liveAttention := h.level(testAttempt)
	liveActions := h.actions(testAttempt)

	// Now the tables are thrown away and rebuilt from the log alone. The state
	// projection is left as it is, because a rebuild reads it.
	h.events.byProject = map[string][]*event.AgentEvent{testProject: logOf(events...)}
	h.projects.ids = []string{testProject}
	h.build()

	report, err := h.attention.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("Rebuild returned an error: %v", err)
	}
	if report.Events != len(events) {
		t.Errorf("the rebuild read %d event(s); want %d", report.Events, len(events))
	}
	if report.Attention != 1 {
		t.Errorf("the rebuild produced %d attention row(s); want 1", report.Attention)
	}
	if report.Actions != 2 {
		t.Errorf("the rebuild produced %d action(s); want 2", report.Actions)
	}

	if got := h.level(testAttempt); got != liveAttention {
		t.Errorf("the rebuilt attention disagrees:\n live    = %+v\n rebuilt = %+v", liveAttention, got)
	}

	rebuilt := h.actions(testAttempt)
	if len(rebuilt) != len(liveActions) {
		t.Fatalf("the rebuild produced %d action(s); the live projection had %d",
			len(rebuilt), len(liveActions))
	}
	byID := make(map[string]attention.Action, len(rebuilt))
	for _, a := range rebuilt {
		byID[a.ID] = a
	}
	for _, want := range liveActions {
		got, ok := byID[want.ID]
		if !ok {
			t.Errorf("the rebuild lost action %s", want.ID)
			continue
		}
		if !sameAction(got, want) {
			t.Errorf("action %s differs:\n live    = %s\n rebuilt = %s",
				want.ID, describe(want), describe(got))
		}
	}
}

// TestRebuildIsIdempotent runs it twice.
func TestRebuildIsIdempotent(t *testing.T) {
	h := newHarness(t)

	events := []*event.AgentEvent{
		eventFor(idFor("created-y"), task.TypeSessionCreated, testProject, "",
			map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)),
		eventFor(idFor("running-y"), task.TypeSessionStatusChanged, testProject, testRuntime,
			map[string]any{
				"taskId": "task_x", "agentSession": testAttempt,
				"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
			}, at(1)),
		agentEvent("d4", claude.TypeAgentPermissionRequested, testProject, testRuntime, at(2)),
	}
	for _, ev := range events {
		h.project(ev)
	}

	h.events.byProject = map[string][]*event.AgentEvent{testProject: logOf(events...)}
	h.projects.ids = []string{testProject}
	h.build()

	ctx := context.Background()
	if _, err := h.attention.Rebuild(ctx); err != nil {
		t.Fatalf("the first Rebuild returned an error: %v", err)
	}
	firstAttention := h.level(testAttempt)
	firstActions := h.actions(testAttempt)

	if _, err := h.attention.Rebuild(ctx); err != nil {
		t.Fatalf("the second Rebuild returned an error: %v", err)
	}

	if got := h.level(testAttempt); got != firstAttention {
		t.Errorf("two rebuilds disagree on attention:\n first  = %+v\n second = %+v", firstAttention, got)
	}
	second := h.actions(testAttempt)
	if len(second) != len(firstActions) {
		t.Fatalf("two rebuilds produced %d and %d actions", len(firstActions), len(second))
	}
	for i := range firstActions {
		if second[i] != firstActions[i] {
			t.Errorf("two rebuilds disagree on action %d:\n first  = %+v\n second = %+v",
				i, firstActions[i], second[i])
		}
	}
}

// TestRebuildIfNeededOnlyRunsWhenEmpty is the start-up rule.
func TestRebuildIfNeededOnlyRunsWhenEmpty(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	events := []*event.AgentEvent{
		eventFor(idFor("created-z"), task.TypeSessionCreated, testProject, "",
			map[string]any{"taskId": "task_x", "agentSession": testAttempt}, at(0)),
		eventFor(idFor("running-z"), task.TypeSessionStatusChanged, testProject, testRuntime,
			map[string]any{
				"taskId": "task_x", "agentSession": testAttempt,
				"from": task.StatusSessionCreated, "to": task.StatusSessionRunning,
			}, at(1)),
	}
	for _, ev := range events {
		h.project(ev)
	}

	h.events.byProject = map[string][]*event.AgentEvent{testProject: logOf(events...)}
	h.projects.ids = []string{testProject}
	h.build()

	report, ran, err := h.attention.RebuildIfNeeded(ctx)
	if err != nil {
		t.Fatalf("RebuildIfNeeded returned an error: %v", err)
	}
	if !ran {
		t.Fatal("RebuildIfNeeded did not run over an empty projection")
	}
	if report.Attention != 1 {
		t.Errorf("the rebuild produced %d attention row(s); want 1", report.Attention)
	}

	// A second call finds attention populated; the action table is empty
	// because nothing in this log raises one, so it runs again - and that is
	// the documented behaviour rather than a bug. What matters is that it does
	// not lose anything.
	if _, _, err := h.attention.RebuildIfNeeded(ctx); err != nil {
		t.Fatalf("the second RebuildIfNeeded returned an error: %v", err)
	}
	if got := h.level(testAttempt); got.AgentSessionID != testAttempt {
		t.Errorf("the row was lost by a second rebuild: %+v", got)
	}
	if got := len(h.actions(testAttempt)); got != 0 {
		t.Errorf("%d action(s) after a rebuild of a log that raises none", got)
	}
}

// TestARebuildThatCannotReadTheLogLeavesTheProjectionAlone checks that a
// failure does not half-empty the tables.
func TestARebuildThatCannotReadTheLogLeavesTheProjectionAlone(t *testing.T) {
	h := newHarness(t)
	h.bind(testProject, testRuntime, testAttempt, 0)
	h.project(agentEvent("d5", claude.TypeAgentFailed, testProject, testRuntime, at(10)))
	beforeAttention := h.level(testAttempt)
	beforeActions := h.actions(testAttempt)

	h.events.err = errors.New("the log went away")
	h.projects.ids = []string{testProject}
	h.build()

	if _, err := h.attention.Rebuild(context.Background()); err == nil {
		t.Fatal("Rebuild succeeded over a log it could not read")
	}

	if got := h.level(testAttempt); got != beforeAttention {
		t.Errorf("a failed rebuild changed the attention:\n before = %+v\n after  = %+v", beforeAttention, got)
	}
	after := h.actions(testAttempt)
	if len(after) != len(beforeActions) {
		t.Errorf("a failed rebuild lost actions: had %d, now %d", len(beforeActions), len(after))
	}
}

// sameAction compares two actions by value.
//
// It cannot be ==: resolved_at is a pointer, and two rows holding the same
// instant are not the same pointer. The comparison is the fields, with the
// times compared as instants.
func sameAction(a, b attention.Action) bool {
	if a.ID != b.ID || a.AgentSessionID != b.AgentSessionID || a.ProjectID != b.ProjectID ||
		a.Type != b.Type || a.Status != b.Status || a.Reason != b.Reason {
		return false
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	switch {
	case a.ResolvedAt == nil && b.ResolvedAt == nil:
		return true
	case a.ResolvedAt == nil || b.ResolvedAt == nil:
		return false
	default:
		return a.ResolvedAt.Equal(*b.ResolvedAt)
	}
}

// describe renders an action with its times, for a failure message.
func describe(a attention.Action) string {
	resolved := "never"
	if a.ResolvedAt != nil {
		resolved = a.ResolvedAt.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s %s type=%s reason=%q created=%s resolved=%s",
		a.ID, a.Status, a.Type, a.Reason, a.CreatedAt.Format(time.RFC3339Nano), resolved)
}

// ---------------------------------------------------------------------------
// 7. The model
// ---------------------------------------------------------------------------

func TestTheLevelVocabularyIsClosed(t *testing.T) {
	want := []attention.Level{
		attention.LevelNone, attention.LevelActionRequired,
		attention.LevelWarning, attention.LevelInfo,
	}
	got := attention.Levels()
	if len(got) != len(want) {
		t.Fatalf("Levels() returned %d levels; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Levels()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
	if attention.ValidLevel("URGENT") {
		t.Error("ValidLevel accepted a level that does not exist")
	}
}

func TestTheActionVocabularyIsClosed(t *testing.T) {
	if got := len(attention.ActionTypes()); got != 3 {
		t.Errorf("ActionTypes() returned %d types; want 3", got)
	}
	for _, banned := range []string{"ALLOW", "DENY", "EXECUTE", "ANSWER"} {
		if attention.ValidActionType(banned) {
			t.Errorf("ValidActionType(%q) = true; this phase defines no such action", banned)
		}
	}
	if attention.ValidActionStatus("DONE") {
		t.Error("ValidActionStatus accepted a status that does not exist")
	}
}

func TestAnActionIdIsDerivedFromItsEvent(t *testing.T) {
	eventID := idFor("1234abcd")
	actionID := attention.ActionIDForEvent(eventID)
	if !attention.ValidActionID(actionID) {
		t.Errorf("ActionIDForEvent(%q) = %q, which is not an action id", eventID, actionID)
	}
	if actionID == attention.ActionIDForEvent(idFor("1234abce")) {
		t.Error("two events produced the same action id")
	}
	if got := attention.ActionIDForEvent("not-an-event-id"); got != "" {
		t.Errorf("ActionIDForEvent on a non-event id = %q; want empty", got)
	}
	// Deriving it twice is the same answer, which is what makes the queue
	// idempotent.
	if again := attention.ActionIDForEvent(eventID); again != actionID {
		t.Errorf("ActionIDForEvent is not a function: %q then %q", actionID, again)
	}
}

func TestRowsMustValidateBeforeTheyAreStored(t *testing.T) {
	badAttention := map[string]attention.Attention{
		"no attempt": {ProjectID: testProject, Level: attention.LevelNone,
			Reason: attention.ReasonAgentStarted, UpdatedAt: testClock},
		"no project": {AgentSessionID: testAttempt, Level: attention.LevelNone,
			Reason: attention.ReasonAgentStarted, UpdatedAt: testClock},
		"unknown level": {AgentSessionID: testAttempt, ProjectID: testProject, Level: "URGENT",
			Reason: attention.ReasonAgentStarted, UpdatedAt: testClock},
		"no reason": {AgentSessionID: testAttempt, ProjectID: testProject, Level: attention.LevelNone,
			UpdatedAt: testClock},
		"no time": {AgentSessionID: testAttempt, ProjectID: testProject, Level: attention.LevelNone,
			Reason: attention.ReasonAgentStarted},
	}
	for name, a := range badAttention {
		t.Run("attention/"+name, func(t *testing.T) {
			if err := a.Validate(); err == nil {
				t.Errorf("Validate accepted attention with %s", name)
			}
		})
	}

	badAction := map[string]attention.Action{
		"no id": {AgentSessionID: testAttempt, ProjectID: testProject, Type: attention.ActionViewFailure,
			Status: attention.ActionPending, Reason: attention.ReasonAgentFailed, CreatedAt: testClock},
		"unknown type": {ID: attention.ActionIDForEvent(idFor("aaaa")), AgentSessionID: testAttempt,
			ProjectID: testProject, Type: "ALLOW", Status: attention.ActionPending,
			Reason: attention.ReasonAgentFailed, CreatedAt: testClock},
		"resolved with no time": {ID: attention.ActionIDForEvent(idFor("aaaa")), AgentSessionID: testAttempt,
			ProjectID: testProject, Type: attention.ActionViewFailure, Status: attention.ActionResolved,
			Reason: attention.ReasonAgentFailed, CreatedAt: testClock},
	}
	for name, a := range badAction {
		t.Run("action/"+name, func(t *testing.T) {
			if err := a.Validate(); err == nil {
				t.Errorf("Validate accepted an action with %s", name)
			}
		})
	}
}

func TestAskWithoutNamingAnythingIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.attention.Attention(ctx, "  "); !attention.IsCode(err, attention.CodeInvalidAttention) {
		t.Errorf("Attention(\"\") = %v; want %q", err, attention.CodeInvalidAttention)
	}
	if _, err := h.attention.Actions(ctx, ""); !attention.IsCode(err, attention.CodeInvalidAction) {
		t.Errorf("Actions(\"\") = %v; want %q", err, attention.CodeInvalidAction)
	}
	if _, err := h.attention.ListAttentionByProject(ctx, "", 0); !attention.IsCode(err, attention.CodeInvalidAttention) {
		t.Errorf("ListAttentionByProject(\"\") = %v; want %q", err, attention.CodeInvalidAttention)
	}
	if _, err := h.attention.ListActionsByProject(ctx, "", 0); !attention.IsCode(err, attention.CodeInvalidAction) {
		t.Errorf("ListActionsByProject(\"\") = %v; want %q", err, attention.CodeInvalidAction)
	}
}

func TestNewServiceRefusesAnIncompleteConfiguration(t *testing.T) {
	if _, err := attention.NewService(attention.Options{}); err == nil {
		t.Error("NewService succeeded with no repository")
	}
}
