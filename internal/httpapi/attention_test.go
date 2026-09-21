package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/attention"
)

// These tests are about the three attention routes and about the routes that are
// deliberately absent.
//
// The rows are produced by the real chain, both projections installed in the
// order a running server installs them. A test that built a level by hand would
// be testing the encoding rather than the projection.

// ---------------------------------------------------------------------------
// GET /api/sessions/{id}/attention
// ---------------------------------------------------------------------------

// TestGettingTheAttentionOfAFailedAttempt walks the whole chain: an agent is
// started through the API, the attempt is created and closed as FAILED, the
// events are projected as they are stored, and the attention is read back.
func TestGettingTheAttentionOfAFailedAttempt(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	recorder := h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/attention", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	a := decode[attention.Attention](t, recorder)

	if a.AgentSessionID != attempt.ID {
		t.Errorf("agentSessionId = %q; want %q", a.AgentSessionID, attempt.ID)
	}
	if a.ProjectID != projectID {
		t.Errorf("projectId = %q; want %q", a.ProjectID, projectID)
	}
	// The launch failed and the coordinator closed the attempt as FAILED, so
	// the level followed.
	if a.Level != attention.LevelWarning {
		t.Errorf("level = %q; want %q - the attempt was closed as failed",
			a.Level, attention.LevelWarning)
	}
	// The launch never reached Claude, so this is the attempt's own failure
	// rather than the agent's - see the note on attentionFor.
	if a.Reason != attention.ReasonAttemptFailed {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonAttemptFailed)
	}
}

// TestAnAttemptThatHasNotStartedIsAtNone is the row that makes every attempt
// answerable: a level rather than an absence.
func TestAnAttemptThatHasNotStartedIsAtNone(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "checkout-service")

	created := h.call(http.MethodPost, "/api/projects/"+projectID+"/tasks", `{"title":"Fix the viewer"}`)
	tk := decode[taskResponse](t, created).Task
	attempt := decode[sessionResponse](t, h.call(http.MethodPost, "/api/tasks/"+tk.ID+"/sessions", "")).Session

	recorder := h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/attention", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	a := decode[attention.Attention](t, recorder)
	if a.Level != attention.LevelNone {
		t.Errorf("level = %q; want %q", a.Level, attention.LevelNone)
	}
	if a.Reason != attention.ReasonAttemptCreated {
		t.Errorf("reason = %q; want %q", a.Reason, attention.ReasonAttemptCreated)
	}
}

func TestAttentionForAnAttemptTheLogNeverMentionedIsNotFound(t *testing.T) {
	h := newHarness(t)

	h.wantError(t,
		h.call(http.MethodGet, "/api/sessions/sess_00000000000000000000/attention", ""),
		http.StatusNotFound, attention.CodeNotFound)
}

// ---------------------------------------------------------------------------
// GET /api/projects/{id}/attention
// ---------------------------------------------------------------------------

func TestListingAProjectsAttention(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	otherID, _ := h.registerProject(t, "other-service")

	h.startAnAttempt(t, projectID)
	h.startAnAttempt(t, projectID)
	h.startAnAttempt(t, otherID)

	recorder := h.call(http.MethodGet, "/api/projects/"+projectID+"/attention", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	list := decode[attentionListResponse](t, recorder)

	if list.Count != 2 {
		t.Fatalf("the project has %d attention row(s); want 2", list.Count)
	}
	for _, a := range list.Attention {
		if a.ProjectID != projectID {
			t.Errorf("the listing returned a row from %q", a.ProjectID)
		}
	}

	other := decode[attentionListResponse](t,
		h.call(http.MethodGet, "/api/projects/"+otherID+"/attention", ""))
	if other.Count != 1 {
		t.Errorf("the other project has %d attention row(s); want 1", other.Count)
	}
}

func TestListingAttentionForAnUnknownProjectIsNotFound(t *testing.T) {
	h := newHarness(t)

	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/p_00000000000000000000/attention", ""),
		http.StatusNotFound, "project_not_found")
}

func TestListingAttentionForAQuietProjectIsEmpty(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "checkout-service")

	recorder := h.call(http.MethodGet, "/api/projects/"+projectID+"/attention", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	list := decode[attentionListResponse](t, recorder)
	if list.Count != 0 {
		t.Errorf("count = %d; want 0", list.Count)
	}
	if list.Attention == nil {
		t.Error("attention = null; want an empty list, so a client renders one shape")
	}
}

// ---------------------------------------------------------------------------
// GET /api/projects/{id}/actions
// ---------------------------------------------------------------------------

// TestListingAProjectsActions is the queue. The chain that fails produces a
// VIEW_FAILURE, which is the one action a test can reach without a real Claude.
func TestListingAProjectsActions(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	recorder := h.call(http.MethodGet, "/api/projects/"+projectID+"/actions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	list := decode[actionListResponse](t, recorder)

	if list.Count != 1 {
		t.Fatalf("the project has %d action(s); want 1: %+v", list.Count, list.Actions)
	}
	action := list.Actions[0]
	if action.Type != attention.ActionViewFailure {
		t.Errorf("action type = %s; want VIEW_FAILURE", action.Type)
	}
	if action.Status != attention.ActionPending {
		t.Errorf("action status = %s; want PENDING", action.Status)
	}
	if action.AgentSessionID != attempt.ID {
		t.Errorf("action attempt = %q; want %q", action.AgentSessionID, attempt.ID)
	}
	if action.ProjectID != projectID {
		t.Errorf("action project = %q; want %q", action.ProjectID, projectID)
	}
	if action.ResolvedAt != nil {
		t.Errorf("a pending action carries a resolution time: %v", action.ResolvedAt)
	}
}

func TestListingActionsForAnUnknownProjectIsNotFound(t *testing.T) {
	h := newHarness(t)

	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/p_00000000000000000000/actions", ""),
		http.StatusNotFound, "project_not_found")
}

func TestListingActionsRejectsABadLimit(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "checkout-service")

	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/actions?limit=0", ""),
		http.StatusBadRequest, CodeInvalidRequest)
	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/attention?limit=0", ""),
		http.StatusBadRequest, CodeInvalidRequest)
}

// ---------------------------------------------------------------------------
// What is deliberately absent
// ---------------------------------------------------------------------------

// TestAnActionCannotBeAnswered is §十八's rule, and it is the boundary this
// phase stops at.
//
// A PERMISSION_REQUEST action records that Claude asked for something. An
// endpoint that answered it would make AgentMux a participant in a Claude
// session rather than an observer of one, and there is no such endpoint.
func TestAnActionCannotBeAnswered(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startAnAttempt(t, projectID)

	before := decode[actionListResponse](t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/actions", ""))
	if before.Count == 0 {
		t.Fatal("no action was raised, so there is nothing to try to answer")
	}
	actionID := before.Actions[0].ID

	// Every route an answer could plausibly be sent to.
	paths := []string{
		"/api/actions/" + actionID,
		"/api/actions/" + actionID + "/resolve",
		"/api/actions/" + actionID + "/answer",
		"/api/projects/" + projectID + "/actions/" + actionID,
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
			recorder := h.call(method, path, `{"decision":"allow"}`)
			if recorder.Code < 400 {
				t.Errorf("%s %s answered %d; want an error - this phase defines no way "+
					"to answer an action", method, path, recorder.Code)
			}
		}
	}

	after := decode[actionListResponse](t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/actions", ""))
	if after.Count != before.Count || after.Actions[0].Status != attention.ActionPending {
		t.Errorf("the queue changed after attempts to answer it:\n before = %+v\n after  = %+v",
			before.Actions, after.Actions)
	}
}

// TestAttentionIsRefusedOnAServerWithoutTheProjection is the branch a server
// built without one takes.
func TestAttentionIsRefusedOnAServerWithoutTheProjection(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:            pinnedAgent{spec: testAgentSpec},
		runtimeAvailable: true,
		withoutAttention: true,
	})
	projectID, _ := h.registerProject(t, "checkout-service")

	h.wantError(t,
		h.call(http.MethodGet, "/api/sessions/sess_00000000000000000000/attention", ""),
		http.StatusServiceUnavailable, CodeInternal)
	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/attention", ""),
		http.StatusServiceUnavailable, CodeInternal)
	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/actions", ""),
		http.StatusServiceUnavailable, CodeInternal)
}

// TestTheAttentionRoutesReadTheProjection checks that the endpoints answer from
// the projected rows rather than recomputing them.
func TestTheAttentionRoutesReadTheProjection(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	// Move the row behind the API's back. A recomputing endpoint would not
	// notice.
	moved := attention.Attention{
		AgentSessionID: attempt.ID,
		ProjectID:      projectID,
		Level:          attention.LevelActionRequired,
		Reason:         attention.ReasonPermissionRequested,
		UpdatedAt:      attempt.CreatedAt.Add(time.Hour),
	}
	if _, err := h.store.Attention().UpsertAttention(context.Background(), moved); err != nil {
		t.Fatalf("moving the attention failed: %v", err)
	}

	got := decode[attention.Attention](t,
		h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/attention", ""))
	if got.Level != attention.LevelActionRequired {
		t.Errorf("level = %q; want %q - the endpoint must read the projected row",
			got.Level, attention.LevelActionRequired)
	}
}
