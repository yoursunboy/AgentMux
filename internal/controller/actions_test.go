package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/controller"
	"github.com/kutonlagos/agentmux/internal/project"
)

// The queue read is tested against fakes of the two things it reads, for the
// reason the card tests are: it decides nothing about either. What the fakes buy
// is the ability to hand it an order and a count that disagree, which is the one
// thing a real projection will not do on demand.

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeActions struct {
	list     []attention.Action
	byID     map[string]attention.Action
	byType   map[attention.ActionType]int
	listErr  error
	getErr   error
	countErr error

	listCalls  int
	countCalls int
}

func (f *fakeActions) ListActions(_ context.Context, limit int) ([]attention.Action, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if limit > 0 && limit < len(f.list) {
		return f.list[:limit], nil
	}
	return f.list, nil
}

func (f *fakeActions) Action(_ context.Context, id string) (attention.Action, error) {
	if f.getErr != nil {
		return attention.Action{}, f.getErr
	}
	a, ok := f.byID[id]
	if !ok {
		return attention.Action{}, &attention.Error{
			Code:    attention.CodeNotFound,
			Message: "no event has raised the action " + id,
		}
	}
	return a, nil
}

func (f *fakeActions) PendingCountsByType(context.Context) (map[attention.ActionType]int, error) {
	f.countCalls++
	if f.countErr != nil {
		return nil, f.countErr
	}
	return f.byType, nil
}

// queueHarness builds the aggregation over a project list and a queue, so a
// test can take either away and ask what the queue does without it.
type queueHarness struct {
	t        *testing.T
	service  *controller.Service
	projects *fakeProjects
	actions  *fakeActions
}

func newQueueHarness(t *testing.T, projects []*project.Project, actions *fakeActions) *queueHarness {
	t.Helper()
	h := &queueHarness{
		t:        t,
		projects: &fakeProjects{list: projects},
		actions:  actions,
	}
	h.build()
	return h
}

func (h *queueHarness) build() {
	h.t.Helper()
	var reader controller.ActionReader
	if h.actions != nil {
		reader = h.actions
	}
	service, err := controller.NewService(controller.Options{
		Projects: h.projects,
		Actions:  reader,
		Logger:   discardLogger(),
	})
	if err != nil {
		h.t.Fatalf("controller.NewService returned an error: %v", err)
	}
	h.service = service
}

// anAction builds a pending action with the fields the queue reads.
func anAction(id, projectID string, actionType attention.ActionType, created time.Time) attention.Action {
	return attention.Action{
		ID:             id,
		AgentSessionID: "sess_" + id,
		ProjectID:      projectID,
		Type:           actionType,
		Status:         attention.ActionPending,
		Reason:         "permission requested",
		CreatedAt:      created,
	}
}

// ---------------------------------------------------------------------------
// The queue
// ---------------------------------------------------------------------------

// TestTheQueueNamesEachActionsProject is the whole reason this read exists.
//
// An action carries a project id because that is what the projection stores, and
// "which agent needs me" is a question about a project a person can recognise.
// The name is resolved in one listing rather than one read per action.
func TestTheQueueNamesEachActionsProject(t *testing.T) {
	actions := &fakeActions{
		list: []attention.Action{
			anAction("act_a", "p_a", attention.ActionViewFailure, at(10)),
			anAction("act_b", "p_b", attention.ActionPermissionRequest, at(11)),
			anAction("act_c", "p_c", attention.ActionViewCompletion, at(12)),
		},
		byType: map[attention.ActionType]int{
			attention.ActionPermissionRequest: 1,
			attention.ActionViewFailure:       1,
			attention.ActionViewCompletion:    1,
		},
	}
	h := newQueueHarness(t, []*project.Project{
		projectRow("p_a", "AgentMux", at(1)),
		projectRow("p_b", "PTE Trainer", at(1)),
		projectRow("p_c", "Resume Parser", at(1)),
	}, actions)

	view, err := h.service.Actions(context.Background(), attention.DefaultListLimit)
	if err != nil {
		t.Fatalf("Actions returned an error: %v", err)
	}

	want := []string{"AgentMux", "PTE Trainer", "Resume Parser"}
	got := make([]string, 0, len(view.Actions))
	for _, a := range view.Actions {
		got = append(got, a.ProjectName)
	}
	if !sameOrder(got, want) {
		t.Errorf("the queue names the projects %v; want %v", got, want)
	}

	if view.Count != 3 {
		t.Errorf("count = %d; want 3", view.Count)
	}
	// The order the queue layer returned is the order the view keeps. Nothing
	// here re-sorts: the ordering is a property of the queue, and a second one
	// here would be a second answer to which action is at the top.
	if view.Actions[0].ID != "act_a" || view.Actions[2].ID != "act_c" {
		t.Errorf("the view reordered the queue: %s first, %s last",
			view.Actions[0].ID, view.Actions[2].ID)
	}
}

// TestTheQueueSplitsWhatStopsWorkFromWhatCanBeRead is the two counts.
//
// It is the product decision the Action Center is shaped by: a permission
// request is the only thing nothing progresses without, and everything else is
// worth reading later. The level is read from the type, so this is also the test
// that the fold and the projection agree.
func TestTheQueueSplitsWhatStopsWorkFromWhatCanBeRead(t *testing.T) {
	actions := &fakeActions{
		list: []attention.Action{
			anAction("act_a", "p_a", attention.ActionPermissionRequest, at(10)),
			anAction("act_b", "p_a", attention.ActionViewFailure, at(11)),
			anAction("act_c", "p_b", attention.ActionViewFailure, at(12)),
			anAction("act_d", "p_c", attention.ActionViewCompletion, at(13)),
		},
		byType: map[attention.ActionType]int{
			attention.ActionPermissionRequest: 1,
			attention.ActionViewFailure:       2,
			attention.ActionViewCompletion:    1,
		},
	}
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, actions)

	view, err := h.service.Actions(context.Background(), attention.DefaultListLimit)
	if err != nil {
		t.Fatalf("Actions returned an error: %v", err)
	}

	if view.NeedsYou != 1 {
		t.Errorf("needsYou = %d; want 1 - one permission request is waiting", view.NeedsYou)
	}
	if view.Notices != 3 {
		t.Errorf("notices = %d; want 3 - two failures and a completion are worth reading", view.Notices)
	}
	// The counts are pending-only, so a settled action is in neither. That is
	// the whole difference between a headline and a history.
	if view.NeedsYou+view.Notices != 4 {
		t.Errorf("the two counts total %d; want 4", view.NeedsYou+view.Notices)
	}
}

// TestASettledActionIsCountedInNeitherList is the "needs you" count that can go
// down.
//
// Nothing resolves a VIEW_FAILURE, so the queue is a backlog; the headline must
// still be a count of what is waiting rather than of everything the project has
// ever done, or it would only ever grow.
func TestASettledActionIsCountedInNeitherList(t *testing.T) {
	resolvedAt := at(30)
	settled := anAction("act_b", "p_a", attention.ActionPermissionRequest, at(11))
	settled.Status = attention.ActionResolved
	settled.ResolvedAt = &resolvedAt

	actions := &fakeActions{
		list: []attention.Action{
			anAction("act_a", "p_a", attention.ActionPermissionRequest, at(10)),
			settled,
		},
		// The projection's own count reports one pending permission request; the
		// settled one is not in this map at all, because the grouped query
		// filters on status.
		byType: map[attention.ActionType]int{attention.ActionPermissionRequest: 1},
	}
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, actions)

	view, err := h.service.Actions(context.Background(), attention.DefaultListLimit)
	if err != nil {
		t.Fatalf("Actions returned an error: %v", err)
	}

	// Both rows are listed - the queue is a record as well as a to-do list - and
	// only one is counted.
	if view.Count != 2 {
		t.Errorf("count = %d; want 2 - the settled action is still listed", view.Count)
	}
	if view.NeedsYou != 1 || view.Notices != 0 {
		t.Errorf("needsYou/notices = %d/%d; want 1/0 - a dealt-with action needs nobody",
			view.NeedsYou, view.Notices)
	}
}

// TestATypeThisBuildDoesNotKnowCountsAsANotice is the else branch of the fold.
//
// A row from a newer build is something waiting to be read. It is certainly not
// a claim that work has stopped, so it must not land in the one list that means
// "nothing progresses without you". The projection cannot produce such a row, so
// the count map is the only place this is reachable.
func TestATypeThisBuildDoesNotKnowCountsAsANotice(t *testing.T) {
	actions := &fakeActions{
		list: nil,
		byType: map[attention.ActionType]int{
			attention.ActionPermissionRequest:          1,
			attention.ActionType("VIEW_SOMETHING_NEW"): 2,
		},
	}
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, actions)

	view, err := h.service.Actions(context.Background(), attention.DefaultListLimit)
	if err != nil {
		t.Fatalf("Actions returned an error: %v", err)
	}

	if view.NeedsYou != 1 {
		t.Errorf("needsYou = %d; want 1 - only the type this build knows is a demand", view.NeedsYou)
	}
	if view.Notices != 2 {
		t.Errorf("notices = %d; want 2 - an unknown type is a notice", view.Notices)
	}
}

// TestTheQueueCountsWhatIsWaitingRatherThanWhatItListed is the headline's one
// guarantee.
//
// The counts come from the projection and describe every pending action; the
// list is a page. A bar that showed the page length would change when somebody
// asked for fewer rows, which is the one thing a count on a console bar must not
// do.
func TestTheQueueCountsWhatIsWaitingRatherThanWhatItListed(t *testing.T) {
	actions := &fakeActions{
		list: []attention.Action{
			anAction("act_a", "p_a", attention.ActionPermissionRequest, at(10)),
			anAction("act_b", "p_a", attention.ActionPermissionRequest, at(11)),
			anAction("act_c", "p_a", attention.ActionPermissionRequest, at(12)),
		},
		byType: map[attention.ActionType]int{attention.ActionPermissionRequest: 9},
	}
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, actions)

	view, err := h.service.Actions(context.Background(), 2)
	if err != nil {
		t.Fatalf("Actions returned an error: %v", err)
	}

	if view.Count != 2 {
		t.Errorf("count = %d; want 2 - the page that was asked for", view.Count)
	}
	if view.NeedsYou != 9 {
		t.Errorf("needsYou = %d; want 9 - the headline is what is waiting, not the page length", view.NeedsYou)
	}
}

// ---------------------------------------------------------------------------
// One action
// ---------------------------------------------------------------------------

// TestReadingOneActionNamesItsProject is the detail page's read.
func TestReadingOneActionNamesItsProject(t *testing.T) {
	actions := &fakeActions{
		byID: map[string]attention.Action{
			"act_a": anAction("act_a", "p_a", attention.ActionPermissionRequest, at(10)),
		},
	}
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, actions)

	got, err := h.service.Action(context.Background(), "act_a")
	if err != nil {
		t.Fatalf("Action returned an error: %v", err)
	}
	if got.ProjectName != "AgentMux" {
		t.Errorf("projectName = %q; want %q", got.ProjectName, "AgentMux")
	}
	if got.Level != string(attention.LevelActionRequired) {
		t.Errorf("level = %q; want %q", got.Level, attention.LevelActionRequired)
	}
	if got.Status != string(attention.ActionPending) {
		t.Errorf("status = %q; want %q", got.Status, attention.ActionPending)
	}
	if got.ResolvedAt != nil {
		t.Errorf("resolvedAt = %v; want nil for a pending action", got.ResolvedAt)
	}
}

// TestAMissingActionKeepsTheQueuesOwnError is the 404's shape.
//
// The error is passed through unwrapped, so the API answers with the code every
// other not-found in this build uses. Wrapping it in a controller code would
// give one absence two spellings, and a client would have to know which endpoint
// it called to know which to expect.
func TestAMissingActionKeepsTheQueuesOwnError(t *testing.T) {
	h := newQueueHarness(t, nil, &fakeActions{byID: map[string]attention.Action{}})

	_, err := h.service.Action(context.Background(), "act_missing")
	if !attention.IsCode(err, attention.CodeNotFound) {
		t.Fatalf("Action returned %v; want an error carrying %s", err, attention.CodeNotFound)
	}
	if controller.CodeOf(err) != "" {
		t.Errorf("Action wrapped the queue's error in the controller code %q; want it passed through",
			controller.CodeOf(err))
	}
}

// ---------------------------------------------------------------------------
// What degrades, and what does not
// ---------------------------------------------------------------------------

// TestTheQueueStillListsWithoutProjectNames is the degraded shape.
//
// A project list that could not be read costs the names and nothing else. A
// queue that showed what is waiting without saying which project it belongs to
// is worth more than an error page, and the client falls back to the id.
func TestTheQueueStillListsWithoutProjectNames(t *testing.T) {
	actions := &fakeActions{
		list:   []attention.Action{anAction("act_a", "p_a", attention.ActionViewFailure, at(10))},
		byType: map[attention.ActionType]int{attention.ActionViewFailure: 1},
	}
	h := newQueueHarness(t, nil, actions)
	h.projects.err = errors.New("the database is gone")

	view, err := h.service.Actions(context.Background(), attention.DefaultListLimit)
	if err != nil {
		t.Fatalf("Actions returned an error: %v; want the queue without names", err)
	}
	if len(view.Actions) != 1 {
		t.Fatalf("the queue has %d rows; want 1", len(view.Actions))
	}
	if view.Actions[0].ProjectName != "" {
		t.Errorf("projectName = %q; want empty - the list could not be read",
			view.Actions[0].ProjectName)
	}
	if view.Actions[0].ProjectID != "p_a" {
		t.Errorf("projectId = %q; want the id the client falls back to", view.Actions[0].ProjectID)
	}
	// The counts are a separate read and are unaffected by the names.
	if view.NeedsYou != 0 || view.Notices != 1 {
		t.Errorf("needsYou/notices = %d/%d; want 0/1", view.NeedsYou, view.Notices)
	}
}

// TestTheQueueStillListsWhenTheCountsCannotBeRead is the other degraded shape.
func TestTheQueueStillListsWhenTheCountsCannotBeRead(t *testing.T) {
	actions := &fakeActions{
		list:     []attention.Action{anAction("act_a", "p_a", attention.ActionViewFailure, at(10))},
		countErr: errors.New("the database is gone"),
	}
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, actions)

	view, err := h.service.Actions(context.Background(), attention.DefaultListLimit)
	if err != nil {
		t.Fatalf("Actions returned an error: %v; want the queue without counts", err)
	}
	if len(view.Actions) != 1 || view.Actions[0].ProjectName != "AgentMux" {
		t.Fatalf("the queue lost more than the counts: %+v", view.Actions)
	}
	if view.NeedsYou != 0 || view.Notices != 0 {
		t.Errorf("needsYou/notices = %d/%d; want 0/0 - the count could not be read",
			view.NeedsYou, view.Notices)
	}
}

// TestAQueueThatCannotBeListedIsTheOneThingThatFails is the request itself.
//
// The listing is what was asked for. Everything else on the page is a section
// that can say it is missing; the list is not.
func TestAQueueThatCannotBeListedIsTheOneThingThatFails(t *testing.T) {
	actions := &fakeActions{listErr: errors.New("the database is gone")}
	h := newQueueHarness(t, nil, actions)

	_, err := h.service.Actions(context.Background(), attention.DefaultListLimit)
	if err == nil {
		t.Fatal("Actions returned no error; want the failure the listing had")
	}
	if controller.CodeOf(err) != controller.CodeUnavailable {
		t.Errorf("Actions returned %v; want the code %s", err, controller.CodeUnavailable)
	}
}

// TestTheQueueIsUnavailableWithoutAQueue is a server built without the
// projection.
//
// The reader is optional in Options the same way the others are, and a server
// without it explains itself rather than answering with an empty queue - which a
// client could not tell from a project where nothing is waiting.
func TestTheQueueIsUnavailableWithoutAQueue(t *testing.T) {
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, nil)

	if _, err := h.service.Actions(context.Background(), attention.DefaultListLimit); err == nil {
		t.Error("Actions returned no error on a server with no action queue")
	} else if controller.CodeOf(err) != controller.CodeUnavailable {
		t.Errorf("Actions returned %v; want the code %s", err, controller.CodeUnavailable)
	}

	if _, err := h.service.Action(context.Background(), "act_a"); err == nil {
		t.Error("Action returned no error on a server with no action queue")
	}

	// And the dashboard still draws, with an empty summary rather than a
	// failure: a header that could not count what is waiting should say nothing,
	// not take the page down.
	queue := h.service.Queue(context.Background())
	if queue.NeedsYou != 0 || queue.Notices != 0 {
		t.Errorf("Queue = %+v; want zeroes without a queue", queue)
	}
}

// TestTheDashboardCarriesTheQueueCounts is the bar's read.
//
// It rides on the dashboard response rather than behind its own request, so the
// console's header is drawn from one response that can fail in one way.
func TestTheDashboardCarriesTheQueueCounts(t *testing.T) {
	actions := &fakeActions{
		byType: map[attention.ActionType]int{
			attention.ActionPermissionRequest: 2,
			attention.ActionViewFailure:       3,
		},
	}
	h := newQueueHarness(t, []*project.Project{projectRow("p_a", "AgentMux", at(1))}, actions)

	dashboard, err := h.service.Dashboard(context.Background(), controller.ServerSummary{Status: serverStatus})
	if err != nil {
		t.Fatalf("Dashboard returned an error: %v", err)
	}
	if dashboard.Queue.NeedsYou != 2 {
		t.Errorf("queue.needsYou = %d; want 2", dashboard.Queue.NeedsYou)
	}
	if dashboard.Queue.Notices != 3 {
		t.Errorf("queue.notices = %d; want 3", dashboard.Queue.Notices)
	}
}
