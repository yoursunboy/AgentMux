package controller_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/agentstate"
	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/controller"
	"github.com/kutonlagos/agentmux/internal/project"
)

// The aggregation is tested against fakes of the three things it reads.
//
// It decides nothing about any of them - it asks, arranges and sorts - so the
// fakes are not hiding anything the way a faked repository would. What they buy
// is control: a test can put two projects in an order and assert the console
// puts them in another, which a real database would make tedious and a real
// projection would make impossible.

var testClock = time.Date(2026, time.September, 21, 15, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return testClock.Add(time.Duration(seconds) * time.Second) }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeProjects struct {
	list []*project.Project
	err  error
}

func (f *fakeProjects) List(context.Context, project.ListFilter) ([]*project.Project, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.list, nil
}

type fakeStates struct {
	byProject map[string]agentstate.AgentState
	err       error

	calls int
}

func (f *fakeStates) StatesForProjects(_ context.Context, ids []string) (map[string]agentstate.AgentState, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]agentstate.AgentState, len(ids))
	for _, id := range ids {
		if state, ok := f.byProject[id]; ok {
			out[id] = state
		}
	}
	return out, nil
}

type fakeAttention struct {
	levels     map[string]attention.Attention
	pending    map[string]int
	err        error
	pendingErr error

	attentionCalls int
	pendingCalls   int
}

func (f *fakeAttention) AttentionForProjects(_ context.Context, ids []string) (map[string]attention.Attention, error) {
	f.attentionCalls++
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]attention.Attention, len(ids))
	for _, id := range ids {
		if a, ok := f.levels[id]; ok {
			out[id] = a
		}
	}
	return out, nil
}

func (f *fakeAttention) PendingCountsForProjects(_ context.Context, ids []string) (map[string]int, error) {
	f.pendingCalls++
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		if n, ok := f.pending[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// projectRow builds a project with the two fields the aggregation reads.
func projectRow(id, name string, updated time.Time) *project.Project {
	return &project.Project{
		ID: id, Name: name, HostPath: "/tmp/" + name, RuntimePath: "/tmp/" + name,
		Status: project.StatusStopped, CreatedAt: testClock, UpdatedAt: updated,
	}
}

type harness struct {
	t         *testing.T
	service   *controller.Service
	projects  *fakeProjects
	states    *fakeStates
	attention *fakeAttention
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		projects:  &fakeProjects{},
		states:    &fakeStates{byProject: map[string]agentstate.AgentState{}},
		attention: &fakeAttention{levels: map[string]attention.Attention{}, pending: map[string]int{}},
	}
	h.build()
	return h
}

// build constructs the service over the current fakes, so a test can replace
// one of them with nil and ask what the aggregation does without it.
func (h *harness) build() {
	h.t.Helper()
	var states controller.StateReader = h.states
	var attentionReader controller.AttentionReader = h.attention
	if h.states == nil {
		states = nil
	}
	if h.attention == nil {
		attentionReader = nil
	}
	service, err := controller.NewService(controller.Options{
		Projects:  h.projects,
		Agents:    states,
		Attention: attentionReader,
		Logger:    discardLogger(),
	})
	if err != nil {
		h.t.Fatalf("controller.NewService returned an error: %v", err)
	}
	h.service = service
}

func (h *harness) cards() []controller.ProjectCard {
	h.t.Helper()
	cards, err := h.service.Projects(context.Background())
	if err != nil {
		h.t.Fatalf("Projects returned an error: %v", err)
	}
	return cards
}

// names is the card order, as names, for an ordering assertion.
func names(cards []controller.ProjectCard) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Name)
	}
	return out
}

func sameOrder(got, want []string) bool {
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

const serverStatus = "online"

// ---------------------------------------------------------------------------
// 1. Empty
// ---------------------------------------------------------------------------

// TestNoProjectsIsAnEmptyConsole is §十六's first case. It is the ordinary
// answer for a fresh installation, and it is a list rather than null so that a
// client renders one shape.
func TestNoProjectsIsAnEmptyConsole(t *testing.T) {
	h := newHarness(t)
	h.projects.list = nil

	dashboard, err := h.service.Dashboard(context.Background(), controller.ServerSummary{
		Status: serverStatus, RuntimeAvailable: true, Version: "0.6.5",
	})
	if err != nil {
		t.Fatalf("Dashboard returned an error: %v", err)
	}
	if dashboard.Count != 0 {
		t.Errorf("count = %d; want 0", dashboard.Count)
	}
	if dashboard.Projects == nil {
		t.Error("projects = null; want an empty list")
	}
	if dashboard.Server.Status != serverStatus {
		t.Errorf("server status = %q; want %q", dashboard.Server.Status, serverStatus)
	}
}

// ---------------------------------------------------------------------------
// 2. One project, four sections
// ---------------------------------------------------------------------------

// TestAProjectCombinesItsFourSections is §十六's second case: everything the
// console shows about a project, in one card.
func TestAProjectCombinesItsFourSections(t *testing.T) {
	h := newHarness(t)
	h.projects.list = []*project.Project{
		{
			ID: "p_a", Name: "checkout", HostPath: "/tmp/a", RuntimePath: "/tmp/a",
			Status: project.StatusRunning, CreatedAt: testClock, UpdatedAt: at(1),
		},
	}
	h.states.byProject["p_a"] = agentstate.AgentState{
		AgentSessionID: "sess_a", ProjectID: "p_a", RuntimeID: "amx-p_a",
		Status: agentstate.StatusWaitingPermission, LastEvent: "agent.permission_requested",
		LastEventAt: at(10), UpdatedAt: at(10),
	}
	h.attention.levels["p_a"] = attention.Attention{
		AgentSessionID: "sess_a", ProjectID: "p_a",
		Level: attention.LevelActionRequired, Reason: attention.ReasonPermissionRequested,
		UpdatedAt: at(10),
	}
	h.attention.pending["p_a"] = 1

	cards := h.cards()
	if len(cards) != 1 {
		t.Fatalf("%d card(s); want 1", len(cards))
	}
	card := cards[0]

	if card.ID != "p_a" || card.Name != "checkout" {
		t.Errorf("the card identifies %q/%q; want p_a/checkout", card.ID, card.Name)
	}
	if card.Runtime.Status != project.StatusRunning {
		t.Errorf("runtime = %q; want %q - the project model's own vocabulary",
			card.Runtime.Status, project.StatusRunning)
	}
	if card.Agent == nil || !card.Agent.Available {
		t.Fatalf("agent = %+v; want an available summary", card.Agent)
	}
	if card.Agent.SessionID != "sess_a" || card.Agent.Status != string(agentstate.StatusWaitingPermission) {
		t.Errorf("agent = %+v; want the attempt and its status", card.Agent)
	}
	if card.Agent.LastEvent != "agent.permission_requested" {
		t.Errorf("lastEvent = %q; want the event type", card.Agent.LastEvent)
	}
	if card.Attention == nil || card.Attention.Level != string(attention.LevelActionRequired) {
		t.Fatalf("attention = %+v; want ACTION_REQUIRED", card.Attention)
	}
	if card.Attention.Reason != attention.ReasonPermissionRequested {
		t.Errorf("reason = %q; want %q", card.Attention.Reason, attention.ReasonPermissionRequested)
	}
	if card.Actions.Pending != 1 || !card.Actions.Available {
		t.Errorf("actions = %+v; want one pending and available", card.Actions)
	}
	// The card's time is the most recent of everything on it, so a client can
	// sort or badge on one field.
	if !card.UpdatedAt.Equal(at(10)) {
		t.Errorf("updatedAt = %s; want the newest of the project's, the state's and the attention's", card.UpdatedAt)
	}
}

// TestAProjectWithNoAgentReportsNull is §八's rule: an agent that has never run
// is absent rather than invented.
func TestAProjectWithNoAgentReportsNull(t *testing.T) {
	h := newHarness(t)
	h.projects.list = []*project.Project{
		{ID: "p_a", Name: "quiet", HostPath: "/tmp/a", RuntimePath: "/tmp/a",
			Status: project.StatusStopped, CreatedAt: testClock, UpdatedAt: at(1)},
	}

	card := h.cards()[0]
	if card.Agent != nil {
		t.Errorf("agent = %+v; want null - nothing has run in this project", card.Agent)
	}
	if card.Attention != nil {
		t.Errorf("attention = %+v; want null", card.Attention)
	}
	if card.Actions.Pending != 0 {
		t.Errorf("pending = %d; want 0", card.Actions.Pending)
	}
	if !card.Actions.Available {
		t.Error("actions reports unavailable, but the projection answered")
	}
}

// ---------------------------------------------------------------------------
// 3. Sorting
// ---------------------------------------------------------------------------

// TestTheConsoleLeadsWithWhatNeedsSomebody is §六's order, and §十六 asks for it
// by name: ACTION_REQUIRED first.
func TestTheConsoleLeadsWithWhatNeedsSomebody(t *testing.T) {
	h := newHarness(t)

	// Deliberately listed in the reverse of the order they should come back in,
	// so a test that passed by accident would have to pass by sorting.
	h.projects.list = []*project.Project{
		projectRow("p_idle", "idle", at(1)),
		projectRow("p_done", "done", at(2)),
		projectRow("p_work", "working", at(3)),
		projectRow("p_warn", "warning", at(4)),
		projectRow("p_act", "blocked", at(5)),
	}
	h.states.byProject["p_work"] = agentstate.AgentState{
		AgentSessionID: "s1", ProjectID: "p_work", Status: agentstate.StatusRunning,
		LastEvent: "agent.started", LastEventAt: at(10), UpdatedAt: at(10),
	}
	h.states.byProject["p_done"] = agentstate.AgentState{
		AgentSessionID: "s2", ProjectID: "p_done", Status: agentstate.StatusCompleted,
		LastEvent: "agent.completed", LastEventAt: at(10), UpdatedAt: at(10),
	}
	h.attention.levels["p_warn"] = attention.Attention{
		AgentSessionID: "s3", ProjectID: "p_warn",
		Level: attention.LevelWarning, Reason: attention.ReasonAgentFailed, UpdatedAt: at(10),
	}
	h.attention.levels["p_act"] = attention.Attention{
		AgentSessionID: "s4", ProjectID: "p_act",
		Level: attention.LevelActionRequired, Reason: attention.ReasonPermissionRequested,
		UpdatedAt: at(10),
	}

	got := names(h.cards())
	want := []string{"blocked", "warning", "working", "done", "idle"}
	if !sameOrder(got, want) {
		t.Errorf("the console order is %v; want %v", got, want)
	}
}

// TestTheSortIsTotalAndStable is what makes a console that redraws every few
// seconds stop shuffling: two cards that are otherwise equal still come back in
// the same order twice.
func TestTheSortIsTotalAndStable(t *testing.T) {
	h := newHarness(t)
	h.projects.list = []*project.Project{
		projectRow("p_b", "same", at(5)),
		projectRow("p_a", "same", at(5)),
		projectRow("p_c", "same", at(5)),
	}

	first := names(h.cards())
	second := names(h.cards())
	if !sameOrder(first, second) {
		t.Errorf("two reads disagree: %v then %v", first, second)
	}
	// Same rank, same time, same name - so the id breaks the tie, and it is the
	// only thing that can.
	if !sameOrder(first, []string{"same", "same", "same"}) {
		t.Errorf("names = %v", first)
	}
	cards := h.cards()
	if cards[0].ID != "p_a" || cards[1].ID != "p_b" || cards[2].ID != "p_c" {
		t.Errorf("ties broke as %s, %s, %s; want the id order",
			cards[0].ID, cards[1].ID, cards[2].ID)
	}
}

// TestAProjectWithNothingToSaySortsLast keeps the ranking honest in both
// directions: a card with no section at all is at the bottom rather than
// silently ranked as though it were working.
func TestAProjectWithNothingToSaySortsLast(t *testing.T) {
	h := newHarness(t)
	h.projects.list = []*project.Project{
		projectRow("p_none", "silent", at(9)),
		projectRow("p_info", "informative", at(1)),
	}
	h.attention.levels["p_info"] = attention.Attention{
		AgentSessionID: "s", ProjectID: "p_info",
		Level: attention.LevelInfo, Reason: attention.ReasonAgentCompleted, UpdatedAt: at(10),
	}

	if got := names(h.cards()); !sameOrder(got, []string{"informative", "silent"}) {
		t.Errorf("order = %v; want the project with something to say first", got)
	}
}

// ---------------------------------------------------------------------------
// 4. Missing services
// ---------------------------------------------------------------------------

// TestAMissingProjectionDegradesRatherThanFailing is §十二: a dashboard shows
// what it can.
func TestAMissingProjectionDegradesRatherThanFailing(t *testing.T) {
	h := newHarness(t)
	h.projects.list = []*project.Project{projectRow("p_a", "checkout", at(1))}

	// Neither projection is wired.
	h.states = nil
	h.attention = nil
	h.build()

	cards := h.cards()
	if len(cards) != 1 {
		t.Fatalf("%d card(s); want 1 - a missing projection is not a missing project", len(cards))
	}
	card := cards[0]

	if card.Agent == nil || card.Agent.Available {
		t.Errorf("agent = %+v; want a section that says it is unavailable", card.Agent)
	}
	if card.Attention == nil || card.Attention.Available {
		t.Errorf("attention = %+v; want a section that says it is unavailable", card.Attention)
	}
	if card.Actions.Available {
		t.Errorf("actions = %+v; want a section that says it is unavailable", card.Actions)
	}
	// What the server does know is still there.
	if card.Name != "checkout" || card.Runtime.Status != project.StatusStopped {
		t.Errorf("the card lost what it knew: %+v", card)
	}
}

// TestAProjectionThatFailsDegradesTheSameWay is the other half: a read that
// could not complete is a section that says so, not a failed request.
func TestAProjectionThatFailsDegradesTheSameWay(t *testing.T) {
	h := newHarness(t)
	h.projects.list = []*project.Project{projectRow("p_a", "checkout", at(1))}
	h.states.err = errors.New("the state table went away")
	h.attention.err = errors.New("the attention table went away")
	h.attention.pendingErr = errors.New("the action table went away")

	cards := h.cards()
	if len(cards) != 1 {
		t.Fatalf("%d card(s); want 1", len(cards))
	}
	card := cards[0]
	if card.Agent == nil || card.Agent.Available {
		t.Errorf("agent = %+v; want unavailable", card.Agent)
	}
	if card.Attention == nil || card.Attention.Available {
		t.Errorf("attention = %+v; want unavailable", card.Attention)
	}
	if card.Actions.Available {
		t.Errorf("actions = %+v; want unavailable", card.Actions)
	}
}

// TestAProjectListingThatFailsIsTheOneThingThatFails is the boundary of the
// degradation rule: without projects there is no console to degrade.
func TestAProjectListingThatFailsIsTheOneThingThatFails(t *testing.T) {
	h := newHarness(t)
	h.projects.err = errors.New("the project table went away")

	if _, err := h.service.Projects(context.Background()); err == nil {
		t.Fatal("Projects succeeded with no project list")
	} else if !controller.IsCode(err, controller.CodeUnavailable) {
		t.Errorf("error = %v; want %q", err, controller.CodeUnavailable)
	}
}

// ---------------------------------------------------------------------------
// 5. Multi project
// ---------------------------------------------------------------------------

// TestProjectsDoNotBleedIntoEachOther is §十六's isolation case.
func TestProjectsDoNotBleedIntoEachOther(t *testing.T) {
	h := newHarness(t)
	h.projects.list = []*project.Project{
		projectRow("p_a", "alpha", at(1)),
		projectRow("p_b", "bravo", at(1)),
	}
	h.states.byProject["p_a"] = agentstate.AgentState{
		AgentSessionID: "sess_a", ProjectID: "p_a", Status: agentstate.StatusRunning,
		LastEvent: "agent.started", LastEventAt: at(10), UpdatedAt: at(10),
	}
	h.attention.levels["p_b"] = attention.Attention{
		AgentSessionID: "sess_b", ProjectID: "p_b",
		Level: attention.LevelWarning, Reason: attention.ReasonAgentFailed, UpdatedAt: at(10),
	}
	h.attention.pending["p_b"] = 3

	byID := map[string]controller.ProjectCard{}
	for _, c := range h.cards() {
		byID[c.ID] = c
	}

	alpha := byID["p_a"]
	if alpha.Agent == nil || alpha.Agent.SessionID != "sess_a" {
		t.Errorf("alpha's agent = %+v; want its own attempt", alpha.Agent)
	}
	if alpha.Attention != nil {
		t.Errorf("alpha has attention %+v; it was never given any", alpha.Attention)
	}
	if alpha.Actions.Pending != 0 {
		t.Errorf("alpha has %d pending; want 0", alpha.Actions.Pending)
	}

	bravo := byID["p_b"]
	if bravo.Agent != nil {
		t.Errorf("bravo has an agent %+v; it was never given one", bravo.Agent)
	}
	if bravo.Attention == nil || bravo.Attention.Level != string(attention.LevelWarning) {
		t.Errorf("bravo's attention = %+v; want WARNING", bravo.Attention)
	}
	if bravo.Actions.Pending != 3 {
		t.Errorf("bravo has %d pending; want 3", bravo.Actions.Pending)
	}
}

// ---------------------------------------------------------------------------
// 6. Batch reads
// ---------------------------------------------------------------------------

// TestTheAggregationReadsOncePerSection is §十三: the cost grows with the size
// of the answer and not with the number of projects.
//
// It is asserted by counting calls rather than by timing, because a timing
// assertion would pass on a fast machine with a slow implementation.
func TestTheAggregationReadsOncePerSection(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 50; i++ {
		h.projects.list = append(h.projects.list, projectRow(
			"p_"+string(rune('a'+i%26))+string(rune('a'+i/26)), "project", at(i)))
	}

	if got := len(h.cards()); got != 50 {
		t.Fatalf("%d card(s); want 50", got)
	}

	if h.states.calls != 1 {
		t.Errorf("the state projection was read %d time(s) for 50 projects; want 1", h.states.calls)
	}
	if h.attention.attentionCalls != 1 {
		t.Errorf("attention was read %d time(s) for 50 projects; want 1", h.attention.attentionCalls)
	}
	if h.attention.pendingCalls != 1 {
		t.Errorf("pending counts were read %d time(s) for 50 projects; want 1", h.attention.pendingCalls)
	}
}

// TestAnEmptyProjectListReadsNothing checks the batch calls are skipped rather
// than made with an empty list, which would be a query that cannot match
// anything.
func TestAnEmptyProjectListReadsNothing(t *testing.T) {
	h := newHarness(t)
	h.projects.list = nil

	if got := len(h.cards()); got != 0 {
		t.Fatalf("%d card(s); want 0", got)
	}
	if h.states.calls != 0 || h.attention.attentionCalls != 0 || h.attention.pendingCalls != 0 {
		t.Errorf("an empty console read the projections anyway: %d, %d, %d",
			h.states.calls, h.attention.attentionCalls, h.attention.pendingCalls)
	}
}

// ---------------------------------------------------------------------------
// 7. Construction
// ---------------------------------------------------------------------------

func TestNewServiceRefusesNoProjectList(t *testing.T) {
	if _, err := controller.NewService(controller.Options{}); err == nil {
		t.Error("NewService succeeded with no project list")
	}
	// The two projections are optional, and a service without them is a
	// legitimate configuration rather than an error.
	if _, err := controller.NewService(controller.Options{Projects: &fakeProjects{}}); err != nil {
		t.Errorf("NewService refused a service with no projections: %v", err)
	}
}
