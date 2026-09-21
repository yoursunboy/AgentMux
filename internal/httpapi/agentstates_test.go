package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/agentstate"
	"github.com/kutonlagos/agentmux/internal/task"
)

// These tests are about the two state endpoints, and about the one that is
// deliberately absent.
//
// The states themselves are produced by the real chain: a test starts an agent
// through the API, the attempt's own lifecycle events are written by the task
// service, they are projected as they are stored, and the state is read back
// over HTTP. Nothing here constructs a state by hand, because a test that did
// would be testing the encoding rather than the projection.

// startAnAttempt drives the chain far enough to produce an attempt and its
// events. The agent launch itself fails - see TestStartingAnAgentOnAStopped
// ProjectBringsTheRuntimeUp - which is enough: the attempt was created, bound
// and closed, and every one of those steps wrote an event.
func (h *harness) startAnAttempt(t *testing.T, projectID string) *task.AgentSession {
	t.Helper()

	created := h.call(http.MethodPost, "/api/projects/"+projectID+"/tasks", `{"title":"Fix the viewer"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("creating a task: status = %d, body was %s", created.Code, created.Body)
	}
	tk := decode[taskResponse](t, created).Task

	h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/agent/start",
		`{"taskId":"`+tk.ID+`"}`)

	sessions := decode[sessionListResponse](t,
		h.call(http.MethodGet, "/api/tasks/"+tk.ID+"/sessions", ""))
	if sessions.Count != 1 {
		t.Fatalf("the task has %d attempt(s); want 1", sessions.Count)
	}
	return sessions.Sessions[0]
}

// ---------------------------------------------------------------------------
// GET /api/sessions/{id}/state
// ---------------------------------------------------------------------------

func TestGettingAnAgentState(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	recorder := h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/state", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	state := decode[agentstate.AgentState](t, recorder)

	if state.AgentSessionID != attempt.ID {
		t.Errorf("agentSessionId = %q; want %q", state.AgentSessionID, attempt.ID)
	}
	if state.ProjectID != projectID {
		t.Errorf("projectId = %q; want %q", state.ProjectID, projectID)
	}
	// The launch failed and the coordinator closed the attempt as FAILED, so
	// the agent's state followed.
	if state.Status != agentstate.StatusFailed {
		t.Errorf("status = %q; want %q - the attempt was closed as failed",
			state.Status, agentstate.StatusFailed)
	}
	if state.LastEvent != task.TypeSessionStatusChanged {
		t.Errorf("lastEvent = %q; want %q", state.LastEvent, task.TypeSessionStatusChanged)
	}
	if state.LastEventAt.IsZero() || state.UpdatedAt.IsZero() {
		t.Errorf("the state carries no times: %+v", state)
	}
}

// TestAnAttemptNothingHasObservedHasNoState is the 404, and it is the ordinary
// answer rather than a failure: an attempt that was created and never started
// has no state because no event produced one.
func TestAnAttemptNothingHasObservedHasNoState(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "checkout-service")

	created := h.call(http.MethodPost, "/api/projects/"+projectID+"/tasks", `{"title":"Fix the viewer"}`)
	tk := decode[taskResponse](t, created).Task
	attempt := decode[sessionResponse](t, h.call(http.MethodPost, "/api/tasks/"+tk.ID+"/sessions", "")).Session

	// The attempt exists and is CREATED, and its own creation was an event -
	// so there is a state, and it says CREATED.
	recorder := h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/state", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	state := decode[agentstate.AgentState](t, recorder)
	if state.Status != agentstate.StatusCreated {
		t.Errorf("status = %q; want %q", state.Status, agentstate.StatusCreated)
	}

	// An attempt that does not exist has none at all.
	missing := h.call(http.MethodGet, "/api/sessions/sess_00000000000000000000/state", "")
	h.wantError(t, missing, http.StatusNotFound, agentstate.CodeNotFound)
}

// TestAgentStateIsRefusedOnAServerWithoutAProjection is the third branch: a
// server built without one explains itself rather than answering with a state
// that was never computed.
func TestAgentStateIsRefusedOnAServerWithoutAProjection(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{
		agent:              pinnedAgent{spec: testAgentSpec},
		runtimeAvailable:   true,
		withoutAgentStates: true,
	})
	projectID, _ := h.registerProject(t, "checkout-service")

	h.wantError(t,
		h.call(http.MethodGet, "/api/sessions/sess_00000000000000000000/state", ""),
		http.StatusServiceUnavailable, CodeInternal)
	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/agent-states", ""),
		http.StatusServiceUnavailable, CodeInternal)
}

// TestAnAgentStateCannotBeWritten is §十五. There is no endpoint that sets one,
// and its absence is the design: a state is what the events add up to, and one
// that a client could set would be a second place the truth lives.
//
// The status is not asserted as 405. This server serves the built frontend from
// a catch-all route, so a request that no method pattern matches falls through
// to the static handler and is answered 404 - which is why no route in this API
// answers 405 to a wrong method. What matters is what §十五 asks for: the write
// is refused, and the state is exactly where the events left it.
func TestAnAgentStateCannotBeWritten(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	before := decode[agentstate.AgentState](t,
		h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/state", ""))

	path := "/api/sessions/" + attempt.ID + "/state"
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		recorder := h.call(method, path, `{"status":"COMPLETED"}`)
		if recorder.Code < 400 {
			t.Errorf("%s %s answered %d; want an error - a state is derived and cannot be set",
				method, path, recorder.Code)
		}
	}

	after := decode[agentstate.AgentState](t,
		h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/state", ""))
	if after != before {
		t.Errorf("a write attempt changed the state:\n before = %+v\n after  = %+v", before, after)
	}
	if after.Status == agentstate.StatusCompleted {
		t.Error("a client managed to set a status the events did not produce")
	}
}

// ---------------------------------------------------------------------------
// GET /api/projects/{id}/agent-states
// ---------------------------------------------------------------------------

func TestListingAProjectsAgentStates(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	otherID, _ := h.registerProject(t, "other-service")

	first := h.startAnAttempt(t, projectID)
	second := h.startAnAttempt(t, projectID)
	h.startAnAttempt(t, otherID)

	recorder := h.call(http.MethodGet, "/api/projects/"+projectID+"/agent-states", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	list := decode[agentStateListResponse](t, recorder)

	if list.Count != 2 {
		t.Fatalf("the project has %d agent state(s); want 2: %v", list.Count, list)
	}
	seen := map[string]bool{}
	for _, state := range list.States {
		if state.ProjectID != projectID {
			t.Errorf("the listing returned a state from %q", state.ProjectID)
		}
		seen[state.AgentSessionID] = true
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Errorf("the listing is missing an attempt: %v", seen)
	}

	// The other project's states are its own.
	other := decode[agentStateListResponse](t,
		h.call(http.MethodGet, "/api/projects/"+otherID+"/agent-states", ""))
	if other.Count != 1 {
		t.Errorf("the other project has %d agent state(s); want 1", other.Count)
	}
	for _, state := range other.States {
		if state.ProjectID != otherID {
			t.Errorf("the other project's listing returned a state from %q", state.ProjectID)
		}
	}
}

func TestListingAgentStatesForAnUnknownProjectIsNotFound(t *testing.T) {
	h := newHarness(t)

	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/p_00000000000000000000/agent-states", ""),
		http.StatusNotFound, "project_not_found")
}

func TestListingAgentStatesForAProjectWithNoAttemptsIsEmpty(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "checkout-service")

	recorder := h.call(http.MethodGet, "/api/projects/"+projectID+"/agent-states", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	list := decode[agentStateListResponse](t, recorder)
	if list.Count != 0 {
		t.Errorf("count = %d; want 0", list.Count)
	}
	if list.States == nil {
		t.Error("states = null; want an empty list, so a client renders one shape")
	}
}

func TestListingAgentStatesRejectsABadLimit(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "checkout-service")

	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/agent-states?limit=0", ""),
		http.StatusBadRequest, CodeInvalidRequest)
	h.wantError(t,
		h.call(http.MethodGet, "/api/projects/"+projectID+"/agent-states?limit=abc", ""),
		http.StatusBadRequest, CodeInvalidRequest)
}

// ---------------------------------------------------------------------------
// The projection is what the endpoints read
// ---------------------------------------------------------------------------

// TestTheEndpointsReadTheProjectionRatherThanTheLog checks that the state
// endpoints answer from the projected row, not by recomputing one. A state that
// was recomputed per request would be a state whose cost grew with the age of
// the installation, which is the thing this phase exists to avoid.
func TestTheEndpointsReadTheProjectionRatherThanTheLog(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	// Move the row behind the API's back, which is only possible because a test
	// can reach the store. If the endpoint recomputed, it would not notice.
	moved := agentstate.AgentState{
		AgentSessionID: attempt.ID,
		ProjectID:      projectID,
		RuntimeID:      attempt.RuntimeID,
		Status:         agentstate.StatusWaitingPermission,
		LastEvent:      "agent.permission_requested",
		// An hour later, so the conditional upsert accepts it whatever the
		// order the attempt's own events were written in.
		LastEventAt: attempt.CreatedAt.Add(time.Hour),
		UpdatedAt:   attempt.CreatedAt.Add(time.Hour),
	}
	if written, err := h.store.AgentStates().Upsert(context.Background(), moved); err != nil || !written {
		t.Fatalf("moving the state failed: written=%v err=%v", written, err)
	}

	state := decode[agentstate.AgentState](t,
		h.call(http.MethodGet, "/api/sessions/"+attempt.ID+"/state", ""))
	if state.Status != agentstate.StatusWaitingPermission {
		t.Errorf("status = %q; want %q - the endpoint must read the projected row",
			state.Status, agentstate.StatusWaitingPermission)
	}
	if state.LastEvent != "agent.permission_requested" {
		t.Errorf("lastEvent = %q; the endpoint must read the projected row", state.LastEvent)
	}
}
