package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/controller"
)

// These tests are about the two dashboard routes and about what they must never
// say.
//
// The cards are built from real state: the chain that starts an agent, fails to
// launch it, and leaves an attempt, a warning and a queued action behind is
// driven through the API, and the dashboard is read back. A test that built a
// card by hand would be testing the encoding rather than the join.

// ---------------------------------------------------------------------------
// GET /api/controller
// ---------------------------------------------------------------------------

func TestTheDashboardOfAnEmptyServer(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodGet, "/api/controller", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	dashboard := decode[controller.Dashboard](t, recorder)

	if dashboard.Server.Status != "online" {
		t.Errorf("server status = %q; want online - the server answering is the whole of the evidence",
			dashboard.Server.Status)
	}
	if dashboard.Server.Version == "" {
		t.Error("the dashboard does not say which build is running")
	}
	if dashboard.Count != 0 {
		t.Errorf("count = %d; want 0", dashboard.Count)
	}
	if dashboard.Projects == nil {
		t.Error("projects = null; want an empty list")
	}
}

// TestTheDashboardCombinesEveryService is the whole point of the phase: one
// request instead of seven.
func TestTheDashboardCombinesEveryService(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	recorder := h.call(http.MethodGet, "/api/controller", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	dashboard := decode[controller.Dashboard](t, recorder)

	if dashboard.Count != 1 {
		t.Fatalf("count = %d; want 1", dashboard.Count)
	}
	card := dashboard.Projects[0]

	if card.ID != projectID {
		t.Errorf("card id = %q; want %q", card.ID, projectID)
	}
	if card.Name != "checkout-service" {
		t.Errorf("card name = %q; want the project's own name", card.Name)
	}
	// The runtime status is the project model's, passed through rather than
	// translated. It is compared against what the project endpoint says rather
	// than against a value chosen here: a failed launch stops the runtime the
	// coordinator started, so the value is "stopped" and the point of the
	// assertion is that the dashboard and the project list agree - and that the
	// spelling is the one the project model uses rather than a second one this
	// layer invented.
	fresh := decode[projectResponse](t, h.call(http.MethodGet, "/api/projects/"+projectID, "")).Project
	if card.Runtime.Status != fresh.Status {
		t.Errorf("the dashboard says the runtime is %q and the project endpoint says %q",
			card.Runtime.Status, fresh.Status)
	}

	// The agent, from the state projection.
	if card.Agent == nil || !card.Agent.Available {
		t.Fatalf("agent = %+v; want an available summary", card.Agent)
	}
	if card.Agent.SessionID != attempt.ID {
		t.Errorf("agent sessionId = %q; want %q", card.Agent.SessionID, attempt.ID)
	}
	if card.Agent.Status == "" {
		t.Error("the agent section does not say what the agent is doing")
	}

	// The attention, from the attention projection.
	if card.Attention == nil || !card.Attention.Available {
		t.Fatalf("attention = %+v; want an available summary", card.Attention)
	}
	if card.Attention.Level == "" {
		t.Error("the attention section does not say how much it needs anybody")
	}

	// The queue, counted rather than listed.
	if !card.Actions.Available {
		t.Fatalf("actions = %+v; want an available summary", card.Actions)
	}
	if card.Actions.Pending != 1 {
		t.Errorf("pending = %d; want the one action the failed launch raised", card.Actions.Pending)
	}
}

// TestTheDashboardLeadsWithWhatNeedsSomebody is §十六's sorting case, over the
// real API rather than over the aggregation.
func TestTheDashboardLeadsWithWhatNeedsSomebody(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})

	// One project that needs somebody, and one that is quiet.
	blockedID, _ := h.registerProject(t, "blocked-service")
	h.startAnAttempt(t, blockedID)
	quietID, _ := h.registerProject(t, "quiet-service")

	dashboard := decode[controller.Dashboard](t, h.call(http.MethodGet, "/api/controller", ""))
	if dashboard.Count != 2 {
		t.Fatalf("count = %d; want 2", dashboard.Count)
	}
	if dashboard.Projects[0].ID != blockedID {
		t.Errorf("the dashboard leads with %q; want the project that needs somebody (%q)",
			dashboard.Projects[0].ID, blockedID)
	}
	if dashboard.Projects[1].ID != quietID {
		t.Errorf("the second card is %q; want %q", dashboard.Projects[1].ID, quietID)
	}
}

// ---------------------------------------------------------------------------
// GET /api/controller/projects
// ---------------------------------------------------------------------------

func TestTheDashboardProjectsRoute(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startAnAttempt(t, projectID)
	h.registerProject(t, "other-service")

	recorder := h.call(http.MethodGet, "/api/controller/projects", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	list := decode[controllerProjectsResponse](t, recorder)

	if list.Count != 2 || len(list.Projects) != 2 {
		t.Fatalf("count = %d, projects = %d; want 2 and 2", list.Count, len(list.Projects))
	}
	// It is the dashboard's list, without the server block.
	dashboard := decode[controller.Dashboard](t, h.call(http.MethodGet, "/api/controller", ""))
	if len(dashboard.Projects) != len(list.Projects) {
		t.Fatalf("the two routes disagree: %d and %d", len(dashboard.Projects), len(list.Projects))
	}
	for i := range list.Projects {
		if list.Projects[i].ID != dashboard.Projects[i].ID {
			t.Errorf("card %d is %q on one route and %q on the other",
				i, list.Projects[i].ID, dashboard.Projects[i].ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Degradation and refusal
// ---------------------------------------------------------------------------

// TestTheDashboardDegradesWhenAProjectionIsMissing is §十二 over the API: a
// server built without one still answers, and says which part it cannot.
func TestTheDashboardDegradesWhenAProjectionIsMissing(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{spec: testAgentSpec},
		runtimeAvailable: true,
		withoutAttention: true,
	})
	projectID, _ := h.registerProject(t, "checkout-service")
	_ = projectID

	recorder := h.call(http.MethodGet, "/api/controller", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; a missing projection must not fail the request: %s",
			recorder.Code, recorder.Body)
	}
	card := decode[controller.Dashboard](t, recorder).Projects[0]

	if card.Attention == nil || card.Attention.Available {
		t.Errorf("attention = %+v; want a section that says it is unavailable", card.Attention)
	}
	if card.Actions.Available {
		t.Errorf("actions = %+v; want a section that says it is unavailable", card.Actions)
	}
	// The state projection is wired, so that section still answers.
	if card.Agent != nil && !card.Agent.Available {
		t.Errorf("agent = %+v; the state projection is wired and should have answered", card.Agent)
	}
}

func TestTheDashboardIsRefusedOnAServerWithoutTheAggregation(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{withoutController: true})

	for _, path := range []string{"/api/controller", "/api/controller/projects"} {
		h.wantError(t, h.call(http.MethodGet, path, ""), http.StatusServiceUnavailable, CodeInternal)
	}
}

// TestTheDashboardCannotBeChanged keeps §一's rule at the route level: this is
// a read model, and there is nothing to write.
func TestTheDashboardCannotBeChanged(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/api/controller", "/api/controller/projects"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			recorder := h.call(method, path, `{"anything":"at all"}`)
			if recorder.Code < 400 {
				t.Errorf("%s %s answered %d; want an error - every controller route is a read",
					method, path, recorder.Code)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Security
// ---------------------------------------------------------------------------

// TestTheDashboardCarriesNothingItShouldNot is §十六's scan, run over the bytes
// a client actually receives rather than over the DTO.
//
// It is the one test here that would notice a field added to a summary by a
// later phase: the response is searched for the words that would appear if
// anything but a status, an identifier or a count had got in.
func TestTheDashboardCarriesNothingItShouldNot(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startAnAttempt(t, projectID)

	recorder := h.call(http.MethodGet, "/api/controller", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	body := strings.ToLower(recorder.Body.String())

	for _, forbidden := range []string{
		"password", "passwd", "token", "secret", "api_key", "apikey",
		"credential", "private_key", "authorization", "bearer",
		"prompt", "tool_input", "toolinput", "transcript",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the dashboard response contains %q:\n%s", forbidden, recorder.Body)
		}
	}

	// And the shape it does carry, checked rather than assumed: a card is
	// identifiers, statuses, a count and times.
	var raw struct {
		Server   map[string]any   `json:"server"`
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("the response is not the shape this test knows: %v", err)
	}
	if len(raw.Projects) != 1 {
		t.Fatalf("%d card(s); want 1", len(raw.Projects))
	}
	allowed := map[string]bool{
		"id": true, "name": true, "runtime": true, "agent": true,
		"attention": true, "actions": true, "updatedAt": true,
	}
	for field := range raw.Projects[0] {
		if !allowed[field] {
			t.Errorf("a card carries a field this test does not know about: %q", field)
		}
	}
}
