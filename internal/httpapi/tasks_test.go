package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/task"
)

// This file is the task and agent session API end to end: a request in, a
// handler, the real service, the real repository, real SQLite, and a response
// out. Everything below the HTTP layer is the production wiring, so a test that
// passes here is a statement about the server rather than about a double.

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// createTask posts a task to a project and returns it.
func (h *harness) createTask(t *testing.T, projectID, title string) *task.Task {
	t.Helper()
	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/tasks",
		`{"title":`+jsonString(title)+`}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("creating a task returned status %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[taskResponse](t, recorder).Task
	if created == nil {
		t.Fatal("the response carries no task")
	}
	return created
}

// createSession posts an attempt at a task and returns it.
func (h *harness) createSession(t *testing.T, taskID string) *task.AgentSession {
	t.Helper()
	recorder := h.call(http.MethodPost, "/api/tasks/"+taskID+"/sessions", "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("creating a session returned status %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[sessionResponse](t, recorder).Session
	if created == nil {
		t.Fatal("the response carries no session")
	}
	return created
}

// patch sends a PATCH and returns the recorder. It exists so that the tests
// about a refusal read as one line rather than three.
func (h *harness) patch(target, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.call(http.MethodPatch, target, body)
}

// runtimeIDForTest is a runtime identifier in the shape this server produces:
// the session prefix followed by a project id.
func runtimeIDForTest(projectID string) string { return "amx-" + projectID }

// ---------------------------------------------------------------------------
// POST /api/projects/{id}/tasks
// ---------------------------------------------------------------------------

func TestCreateTaskOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	recorder := h.call(http.MethodPost, "/api/projects/"+p.ID+"/tasks",
		`{"title":"Implement Controller Viewer"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body was %s", recorder.Code, recorder.Body.String())
	}

	tk := decode[taskResponse](t, recorder).Task
	if tk == nil {
		t.Fatal("the response carries no task")
	}
	if !task.ValidID(tk.ID) {
		t.Errorf("id = %q, which is not a task id", tk.ID)
	}
	if tk.ProjectID != p.ID {
		t.Errorf("projectId = %q, want %q", tk.ProjectID, p.ID)
	}
	if tk.Title != "Implement Controller Viewer" {
		t.Errorf("title = %q, want the title that was sent", tk.Title)
	}
	if tk.Status != task.StatusCreated {
		t.Errorf("status = %q, want %q", tk.Status, task.StatusCreated)
	}
	if tk.CreatedAt.IsZero() || tk.UpdatedAt.IsZero() {
		t.Error("the server produced no timestamps")
	}
	if tk.CompletedAt != nil {
		t.Errorf("completedAt = %v, want nil for new work", tk.CompletedAt)
	}
}

// TestCreateTaskStartsNothing is §二十九, asserted at the only layer where it
// could be violated.
//
// Creating a task records that somebody wants work done. It must not start a
// runtime, create an attempt or launch anything - the two halves of AgentMux
// meet at the runtime API and joining them is a later phase.
func TestCreateTaskStartsNothing(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	tk := h.createTask(t, p.ID, "Implement Controller Viewer")

	// No attempt exists.
	recorder := h.call(http.MethodGet, "/api/tasks/"+tk.ID+"/sessions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing sessions returned status %d: %s", recorder.Code, recorder.Body.String())
	}
	if body := decode[sessionListResponse](t, recorder); body.Count != 0 || len(body.Sessions) != 0 {
		t.Errorf("a new task has %d attempts; creating a task must start nothing", body.Count)
	}

	// No runtime was started.
	recorder = h.call(http.MethodGet, "/api/projects/"+p.ID+"/runtime", "")
	if recorder.Code == http.StatusOK {
		if body := decode[runtimeResponse](t, recorder); body.Runtime != nil && body.Runtime.SessionAlive {
			t.Errorf("creating a task started a runtime: %+v", body.Runtime)
		}
	}
}

func TestCreateTaskRequiresATitle(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	for name, body := range map[string]string{
		"absent":       `{}`,
		"empty":        `{"title":""}`,
		"whitespace":   `{"title":"   "}`,
		"not a string": `{"title":42}`,
		"null":         `{"title":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.call(http.MethodPost, "/api/projects/"+p.ID+"/tasks", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body was %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Code == http.StatusCreated {
				t.Fatal("a task with no usable title was created")
			}
		})
	}
}

// TestCreateTaskRejectsATitleThatNamesItsOwnProject records the one-field body:
// the project is in the path, and a body that also names one is refused rather
// than silently resolved by a rule nobody wrote down.
func TestCreateTaskRejectsAProjectInTheBody(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	recorder := h.call(http.MethodPost, "/api/projects/"+p.ID+"/tasks",
		`{"title":"work","projectId":"p_ffffffffffffffffffff"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body was %s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateTaskUnderAnUnknownProject(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodPost, "/api/projects/p_ffffffffffffffffffff/tasks", `{"title":"work"}`)
	h.wantError(t, recorder, http.StatusNotFound, task.CodeProjectNotFound)
}

func TestCreateTaskUnderAMalformedProjectID(t *testing.T) {
	h := newHarness(t)

	// A project id is "p_" followed by 20 hex characters. "not-a-project" is not
	// one, and the refusal is about the shape of the path rather than about a
	// row that does not exist.
	recorder := h.call(http.MethodPost, "/api/projects/not-a-project/tasks", `{"title":"work"}`)
	if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 400 or 404; body was %s", recorder.Code, recorder.Body.String())
	}
}

// TestCreateTaskRejectsATitleTooLong is the payload guard §四十 asks for: a
// title is ordinary user input, bounded, and an oversized one is refused rather
// than stored.
func TestCreateTaskRejectsATitleTooLong(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	recorder := h.call(http.MethodPost, "/api/projects/"+p.ID+"/tasks",
		`{"title":`+jsonString(strings.Repeat("x", task.MaxTitleLength+1))+`}`)
	h.wantError(t, recorder, http.StatusBadRequest, task.CodeInvalidTitle)
}

// ---------------------------------------------------------------------------
// GET /api/projects/{id}/tasks
// ---------------------------------------------------------------------------

func TestListTasksOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	first := h.createTask(t, p.ID, "first")
	second := h.createTask(t, p.ID, "second")

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	body := decode[taskListResponse](t, recorder)
	if body.Count != 2 || len(body.Tasks) != 2 {
		t.Fatalf("count = %d with %d tasks, want 2", body.Count, len(body.Tasks))
	}
	// Newest first. The two were created in the same millisecond often enough
	// that the ordering rule rather than a fixed sequence is what is asserted:
	// a listing that returned them in either order would make this test flaky,
	// so both orders are checked against the rule instead.
	if body.Tasks[0].ID != second.ID && body.Tasks[0].ID != first.ID {
		t.Errorf("the listing returned %q, which is neither task", body.Tasks[0].ID)
	}
	if body.Tasks[0].CreatedAt.Before(body.Tasks[1].CreatedAt) {
		t.Error("the listing is oldest first; a task list is newest first")
	}
}

// TestTaskListsAreIsolatedByProject is §三十二's isolation check at the API
// level.
func TestTaskListsAreIsolatedByProject(t *testing.T) {
	h := newHarness(t)
	a := h.register("2026 AgentMux/AgentMux")
	b := h.register("2026 AgentMux/Other")

	h.createTask(t, a.ID, "a task for A")
	h.createTask(t, b.ID, "a task for B")

	recorder := h.call(http.MethodGet, "/api/projects/"+a.ID+"/tasks", "")
	body := decode[taskListResponse](t, recorder)
	if body.Count != 1 {
		t.Fatalf("project A's listing holds %d tasks, want 1", body.Count)
	}
	if body.Tasks[0].ProjectID != a.ID {
		t.Errorf("project A's listing returned a task belonging to %q", body.Tasks[0].ProjectID)
	}
	if body.Tasks[0].Title != "a task for A" {
		t.Errorf("project A's listing returned the wrong task: %q", body.Tasks[0].Title)
	}
}

func TestListTasksUnderAnUnknownProject(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodGet, "/api/projects/p_ffffffffffffffffffff/tasks", "")
	h.wantError(t, recorder, http.StatusNotFound, task.CodeProjectNotFound)
}

func TestListTasksFiltersByStatus(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	created := h.createTask(t, p.ID, "not started")
	running := h.createTask(t, p.ID, "in progress")
	if rec := h.patch("/api/tasks/"+running.ID, `{"status":"RUNNING"}`); rec.Code != http.StatusOK {
		t.Fatalf("moving the task to RUNNING returned %d: %s", rec.Code, rec.Body.String())
	}

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks?status=RUNNING", "")
	body := decode[taskListResponse](t, recorder)
	if body.Count != 1 || body.Tasks[0].ID != running.ID {
		t.Errorf("the RUNNING listing returned %d tasks, want only %s", body.Count, running.ID)
	}

	recorder = h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks?status=CREATED", "")
	body = decode[taskListResponse](t, recorder)
	if body.Count != 1 || body.Tasks[0].ID != created.ID {
		t.Errorf("the CREATED listing returned %d tasks, want only %s", body.Count, created.ID)
	}
}

// TestListTasksRejectsAnUnknownStatus answers with the vocabulary rather than
// with an empty page, which a client cannot tell from "none of those".
func TestListTasksRejectsAnUnknownStatus(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks?status=DONE", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body was %s", recorder.Code, recorder.Body.String())
	}
	body := h.wantError(t, recorder, http.StatusBadRequest, task.CodeInvalidInput)
	if field, _ := body.Error.Details["field"].(string); field != "status" {
		t.Errorf("details = %v; the refusal should point at the field that was wrong", body.Error.Details)
	}
	// The vocabulary goes in the message, because it is the same list every
	// time and a client wanting it structured can ask the server what exists.
	for _, status := range task.TaskStatuses() {
		if !strings.Contains(body.Error.Message, status) {
			t.Errorf("the message is %q; it does not name %s", body.Error.Message, status)
		}
	}
}

func TestListTasksReturnsAnEmptyArrayNotNull(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks", "")
	if body := recorder.Body.String(); !strings.Contains(body, `"tasks":[]`) {
		t.Errorf("body = %s; an empty listing must be an empty array, not null", body)
	}
}

func TestListTasksLimit(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	for _, title := range []string{"one", "two", "three"} {
		h.createTask(t, p.ID, title)
	}

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks?limit=2", "")
	if body := decode[taskListResponse](t, recorder); body.Count != 2 {
		t.Errorf("limit=2 returned %d tasks, want 2", body.Count)
	}

	// Above the ceiling is clamped rather than refused: a caller asking for a
	// thousand wants as much as it can have.
	recorder = h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks?limit=100000", "")
	if recorder.Code != http.StatusOK {
		t.Errorf("limit above the ceiling returned %d, want 200", recorder.Code)
	}

	for _, bad := range []string{"0", "-1", "many", "2.5"} {
		recorder = h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks?limit="+bad, "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("limit=%s returned %d, want 400", bad, recorder.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// GET and PATCH /api/tasks/{id}
// ---------------------------------------------------------------------------

func TestGetTaskOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	created := h.createTask(t, p.ID, "Implement Controller Viewer")

	recorder := h.call(http.MethodGet, "/api/tasks/"+created.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	got := decode[taskResponse](t, recorder).Task
	if got == nil || got.ID != created.ID {
		t.Errorf("the response carries %+v, want the task that was created", got)
	}
}

func TestGetTaskUnderAnUnknownID(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodGet, "/api/tasks/task_ffffffffffffffffffffffff", "")
	h.wantError(t, recorder, http.StatusNotFound, task.CodeTaskNotFound)
}

// TestATaskIsNotFoundAcrossProjectBoundaries records the shape of the answer: a
// task is reached by its own id and carries its project with it, so "a task in
// another project" and "no such task" are the same 404 rather than a leak of one
// project's ids into another's namespace.
func TestATaskIsNotFoundAcrossProjectBoundaries(t *testing.T) {
	h := newHarness(t)
	a := h.register("2026 AgentMux/AgentMux")
	b := h.register("2026 AgentMux/Other")

	tk := h.createTask(t, a.ID, "a task for A")

	// Project B's listing does not hold it.
	recorder := h.call(http.MethodGet, "/api/projects/"+b.ID+"/tasks", "")
	if body := decode[taskListResponse](t, recorder); body.Count != 0 {
		t.Errorf("project B's listing holds %d of project A's tasks", body.Count)
	}
	// And it is not reachable under B's path either.
	recorder = h.call(http.MethodPost, "/api/projects/"+b.ID+"/tasks", `{"title":"b"}`)
	if body := decode[taskResponse](t, recorder); body.Task.ProjectID != b.ID {
		t.Errorf("a task created under B belongs to %q", body.Task.ProjectID)
	}
	if tk.ProjectID != a.ID {
		t.Errorf("the original task's project changed to %q", tk.ProjectID)
	}
}

func TestUpdateTaskStatusOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Implement Controller Viewer")

	recorder := h.patch("/api/tasks/"+tk.ID, `{"status":"RUNNING"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	updated := decode[taskResponse](t, recorder).Task
	if updated.Status != task.StatusRunning {
		t.Errorf("status = %q, want %q", updated.Status, task.StatusRunning)
	}
	if !updated.UpdatedAt.After(tk.UpdatedAt) && !updated.UpdatedAt.Equal(tk.UpdatedAt) {
		t.Errorf("updatedAt went backwards: %v then %v", tk.UpdatedAt, updated.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(tk.CreatedAt) {
		t.Errorf("createdAt changed from %v to %v", tk.CreatedAt, updated.CreatedAt)
	}
}

func TestCompletingATaskSetsCompletedAt(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Implement Controller Viewer")

	if rec := h.patch("/api/tasks/"+tk.ID, `{"status":"RUNNING"}`); rec.Code != http.StatusOK {
		t.Fatalf("moving to RUNNING returned %d: %s", rec.Code, rec.Body.String())
	}
	recorder := h.patch("/api/tasks/"+tk.ID, `{"status":"COMPLETED"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("completing returned %d: %s", recorder.Code, recorder.Body.String())
	}

	updated := decode[taskResponse](t, recorder).Task
	if updated.CompletedAt == nil {
		t.Fatal("completedAt is null after the task completed")
	}
	if updated.CompletedAt.Location() != nil && updated.CompletedAt.UTC().IsZero() {
		t.Error("completedAt is the zero time")
	}
}

// TestCompletedIsFinalOverHTTP is §二十二: COMPLETED → RUNNING is refused by
// every request this server accepts.
func TestCompletedIsFinalOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Implement Controller Viewer")

	for _, to := range []string{"RUNNING"} {
		if rec := h.patch("/api/tasks/"+tk.ID, `{"status":"`+to+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("moving to %s returned %d: %s", to, rec.Code, rec.Body.String())
		}
	}
	if rec := h.patch("/api/tasks/"+tk.ID, `{"status":"COMPLETED"}`); rec.Code != http.StatusOK {
		t.Fatalf("completing returned %d: %s", rec.Code, rec.Body.String())
	}

	recorder := h.patch("/api/tasks/"+tk.ID, `{"status":"RUNNING"}`)
	h.wantError(t, recorder, http.StatusConflict, task.CodeInvalidTransition)

	// And it really did not move.
	recorder = h.call(http.MethodGet, "/api/tasks/"+tk.ID, "")
	if got := decode[taskResponse](t, recorder).Task; got.Status != task.StatusCompleted {
		t.Errorf("status = %q after a refused transition, want %q", got.Status, task.StatusCompleted)
	}
}

func TestUpdateTaskRetitles(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Fix the viewer")

	recorder := h.patch("/api/tasks/"+tk.ID, `{"title":"Fix the terminal viewer"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	if got := decode[taskResponse](t, recorder).Task; got.Title != "Fix the terminal viewer" {
		t.Errorf("title = %q, want the new title", got.Title)
	}
}

func TestUpdateTaskRefusesAnEmptyPatch(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Fix the viewer")

	h.wantError(t, h.patch("/api/tasks/"+tk.ID, `{}`), http.StatusBadRequest, CodeInvalidRequest)
}

// TestUpdateTaskRefusesToClearAField is what the optionalString type exists for:
// an explicit null is a request to unset something that cannot be unset, and it
// must not be silently read as "leave it alone".
func TestUpdateTaskRefusesToClearAField(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Fix the viewer")

	for _, body := range []string{`{"title":null}`, `{"status":null}`} {
		h.wantError(t, h.patch("/api/tasks/"+tk.ID, body), http.StatusBadRequest, CodeInvalidRequest)
	}
}

func TestUpdateTaskRejectsAnUnknownStatus(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Fix the viewer")

	recorder := h.patch("/api/tasks/"+tk.ID, `{"status":"DONE"}`)
	h.wantError(t, recorder, http.StatusBadRequest, task.CodeInvalidInput)
}

func TestUpdateTaskUnderAnUnknownID(t *testing.T) {
	h := newHarness(t)

	recorder := h.patch("/api/tasks/task_ffffffffffffffffffffffff", `{"status":"RUNNING"}`)
	h.wantError(t, recorder, http.StatusNotFound, task.CodeTaskNotFound)
}

// TestThereIsNoDeleteOnATask or on an attempt records the deliberate absence.
func TestThereIsNoDeleteOnATask(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Fix the viewer")

	for _, target := range []string{"/api/tasks/" + tk.ID, "/api/projects/" + p.ID + "/tasks"} {
		recorder := h.call(http.MethodDelete, target, "")
		if recorder.Code != http.StatusMethodNotAllowed && recorder.Code != http.StatusNotFound {
			t.Errorf("DELETE %s returned %d; this API has no delete for a task", target, recorder.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Agent sessions
// ---------------------------------------------------------------------------

func TestCreateSessionOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Implement Controller Viewer")

	recorder := h.call(http.MethodPost, "/api/tasks/"+tk.ID+"/sessions", "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body was %s", recorder.Code, recorder.Body.String())
	}

	sess := decode[sessionResponse](t, recorder).Session
	if sess == nil {
		t.Fatal("the response carries no session")
	}
	if !task.ValidSessionID(sess.ID) {
		t.Errorf("id = %q, which is not a session id", sess.ID)
	}
	if sess.TaskID != tk.ID {
		t.Errorf("taskId = %q, want %q", sess.TaskID, tk.ID)
	}
	if sess.Status != task.StatusSessionCreated {
		t.Errorf("status = %q, want %q", sess.Status, task.StatusSessionCreated)
	}
	// §二十八: a session is created with no runtime, and the window between the
	// two requests is a state the model names rather than hides.
	if sess.RuntimeID != "" {
		t.Errorf("runtimeId = %q; a session is created before its runtime", sess.RuntimeID)
	}
	if sess.StartedAt != nil {
		t.Errorf("startedAt = %v; an attempt that has not run has no start time", sess.StartedAt)
	}
	if sess.CreatedAt.IsZero() {
		t.Error("the server produced no creation time")
	}
}

// TestCreateSessionRefusesARuntimeInTheBody records that the two-step creation
// is enforced rather than conventional.
func TestCreateSessionRefusesARuntimeInTheBody(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")

	recorder := h.call(http.MethodPost, "/api/tasks/"+tk.ID+"/sessions",
		`{"runtimeId":"amx-`+p.ID+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; a session is created before its runtime", recorder.Code)
	}
}

func TestCreateSessionUnderAnUnknownTask(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodPost, "/api/tasks/task_ffffffffffffffffffffffff/sessions", "")
	h.wantError(t, recorder, http.StatusNotFound, task.CodeTaskNotFound)
}

func TestSessionListsAreIsolatedByTask(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	a := h.createTask(t, p.ID, "task A")
	b := h.createTask(t, p.ID, "task B")

	mine := h.createSession(t, a.ID)
	h.createSession(t, b.ID)

	recorder := h.call(http.MethodGet, "/api/tasks/"+a.ID+"/sessions", "")
	body := decode[sessionListResponse](t, recorder)
	if body.Count != 1 || body.Sessions[0].ID != mine.ID {
		t.Errorf("task A's listing returned %d attempts, want only %s", body.Count, mine.ID)
	}
}

// TestTwoAttemptsAtOneTask is §六 over HTTP: a second attempt at the same goal is
// a second session, and neither replaces the other.
func TestTwoAttemptsAtOneTask(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "Implement Controller Viewer")

	first := h.createSession(t, tk.ID)
	cancelled := h.patch("/api/sessions/"+first.ID, `{"status":"CANCELLED"}`)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancelling the first attempt returned %d: %s", cancelled.Code, cancelled.Body.String())
	}
	second := h.createSession(t, tk.ID)

	recorder := h.call(http.MethodGet, "/api/tasks/"+tk.ID+"/sessions", "")
	body := decode[sessionListResponse](t, recorder)
	if body.Count != 2 {
		t.Fatalf("the task has %d attempts, want 2", body.Count)
	}
	ids := []string{body.Sessions[0].ID, body.Sessions[1].ID}
	if !(ids[0] == second.ID && ids[1] == first.ID) && !(ids[0] == first.ID && ids[1] == second.ID) {
		t.Errorf("the listing returned %v, want %s and %s", ids, first.ID, second.ID)
	}
	// The first attempt is still there and still says what happened to it.
	recorder = h.call(http.MethodGet, "/api/sessions/"+first.ID, "")
	if got := decode[sessionResponse](t, recorder).Session; got.Status != task.StatusSessionCancelled {
		t.Errorf("the first attempt's status = %q, want %q", got.Status, task.StatusSessionCancelled)
	}
}

func TestGetSessionOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	created := h.createSession(t, tk.ID)

	recorder := h.call(http.MethodGet, "/api/sessions/"+created.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	if got := decode[sessionResponse](t, recorder).Session; got == nil || got.ID != created.ID {
		t.Errorf("the response carries %+v, want the session that was created", got)
	}
}

func TestGetSessionUnderAnUnknownID(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodGet, "/api/sessions/sess_ffffffffffffffffffffffff", "")
	h.wantError(t, recorder, http.StatusNotFound, task.CodeSessionNotFound)
}

func TestSessionStatusOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	recorder := h.patch("/api/sessions/"+sess.ID, `{"status":"RUNNING"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	updated := decode[sessionResponse](t, recorder).Session
	if updated.Status != task.StatusSessionRunning {
		t.Errorf("status = %q, want %q", updated.Status, task.StatusSessionRunning)
	}
	if updated.StartedAt == nil {
		t.Error("startedAt is null for a session that is running")
	}

	recorder = h.patch("/api/sessions/"+sess.ID, `{"status":"COMPLETED"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("completing returned %d: %s", recorder.Code, recorder.Body.String())
	}
	updated = decode[sessionResponse](t, recorder).Session
	if updated.EndedAt == nil {
		t.Error("endedAt is null for a session that has ended")
	}
	if updated.StartedAt == nil {
		t.Error("startedAt was cleared by a later transition")
	}
}

// TestASessionCannotRunAgainAfterItFinishes is the session half of "COMPLETED
// is final". A second attempt is a second session.
func TestASessionCannotRunAgainAfterItFinishes(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	for _, to := range []string{"RUNNING", "COMPLETED"} {
		if rec := h.patch("/api/sessions/"+sess.ID, `{"status":"`+to+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("moving to %s returned %d: %s", to, rec.Code, rec.Body.String())
		}
	}

	h.wantError(t, h.patch("/api/sessions/"+sess.ID, `{"status":"RUNNING"}`),
		http.StatusConflict, task.CodeInvalidTransition)
}

func TestSessionStatusRejectsWaiting(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	// WAITING belongs to the task. It is not a session status, and asking for it
	// is refused with the vocabulary rather than silently accepted.
	recorder := h.patch("/api/sessions/"+sess.ID, `{"status":"WAITING"}`)
	body := h.wantError(t, recorder, http.StatusBadRequest, task.CodeInvalidInput)

	// The message quotes the value that was refused and lists what exists. The
	// count is what says WAITING is not on the list: it appears once, in
	// quotation marks, and a message that had added it to the vocabulary would
	// mention it twice.
	if got := strings.Count(body.Error.Message, "WAITING"); got != 1 {
		t.Errorf("the message is %q, which names WAITING %d times; want once, as the refusal",
			body.Error.Message, got)
	}
	for _, status := range task.AgentSessionStatuses() {
		if !strings.Contains(body.Error.Message, status) {
			t.Errorf("the message is %q; it does not name %s", body.Error.Message, status)
		}
	}
	if field, _ := body.Error.Details["field"].(string); field != "status" {
		t.Errorf("details = %v; the refusal should point at the field that was wrong", body.Error.Details)
	}
}

func TestAttachRuntimeOverHTTP(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	recorder := h.patch("/api/sessions/"+sess.ID, `{"status":"RUNNING"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("starting the attempt returned %d: %s", recorder.Code, recorder.Body.String())
	}

	// The runtime is attached afterwards, and it need not exist: a runtime can
	// be destroyed while the attempt that used it remains, which is why the
	// column is not a foreign key.
	recorder = h.patch("/api/sessions/"+sess.ID, `{"runtimeId":"`+runtimeIDForTest(p.ID)+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("attaching the runtime returned %d: %s", recorder.Code, recorder.Body.String())
	}
	updated := decode[sessionResponse](t, recorder).Session
	if updated.RuntimeID != runtimeIDForTest(p.ID) {
		t.Errorf("runtimeId = %q, want %q", updated.RuntimeID, runtimeIDForTest(p.ID))
	}
	if updated.Status != task.StatusSessionRunning {
		t.Errorf("status = %q; attaching a runtime is not a status change", updated.Status)
	}
}

func TestAttachRuntimeRefusesToRebind(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	other := h.register("2026 AgentMux/Other")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	if rec := h.patch("/api/sessions/"+sess.ID, `{"runtimeId":"`+runtimeIDForTest(p.ID)+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("the first attach returned %d: %s", rec.Code, rec.Body.String())
	}

	recorder := h.patch("/api/sessions/"+sess.ID, `{"runtimeId":"`+runtimeIDForTest(other.ID)+`"}`)
	h.wantError(t, recorder, http.StatusConflict, task.CodeConflict)
}

func TestAttachRuntimeRejectsANonRuntime(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	for _, bad := range []string{"amx-", "amx-1", p.ID, ""} {
		body, err := json.Marshal(map[string]string{"runtimeId": bad})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		recorder := h.patch("/api/sessions/"+sess.ID, string(body))
		if recorder.Code == http.StatusOK {
			t.Errorf("runtimeId %q was accepted; only a runtime identifier may be attached", bad)
		}
	}
}

func TestAttachRuntimeRefusesToDetach(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	recorder := h.patch("/api/sessions/"+sess.ID, `{"runtimeId":null}`)
	h.wantError(t, recorder, http.StatusBadRequest, CodeInvalidRequest)
}

func TestUpdateSessionUnderAnUnknownID(t *testing.T) {
	h := newHarness(t)

	recorder := h.patch("/api/sessions/sess_ffffffffffffffffffffffff", `{"status":"RUNNING"}`)
	h.wantError(t, recorder, http.StatusNotFound, task.CodeSessionNotFound)
}

func TestThereIsNoDeleteOnASession(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	sess := h.createSession(t, tk.ID)

	recorder := h.call(http.MethodDelete, "/api/sessions/"+sess.ID, "")
	if recorder.Code != http.StatusMethodNotAllowed && recorder.Code != http.StatusNotFound {
		t.Errorf("DELETE /api/sessions/%s returned %d; this API has no delete for an attempt",
			sess.ID, recorder.Code)
	}
}

// ---------------------------------------------------------------------------
// The server without a task service
// ---------------------------------------------------------------------------

// TestTaskRoutesExplainThemselvesWhenTheServiceIsAbsent is the optional-
// collaborator rule: a server built without a task model answers 503 rather
// than panicking or reporting an empty list.
func TestTaskRoutesExplainThemselvesWhenTheServiceIsAbsent(t *testing.T) {
	h := newHarnessWithoutTasks(t)
	p := h.register("2026 AgentMux/AgentMux")

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID+"/tasks", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body was %s", recorder.Code, recorder.Body.String())
	}
	if message := decode[errorResponse](t, recorder).Error.Message; !strings.Contains(message, "task") {
		t.Errorf("the message is %q; it should say which model is missing", message)
	}
}

// ---------------------------------------------------------------------------
// Concurrency, through the API
// ---------------------------------------------------------------------------

// TestConcurrentStatusChangesThroughTheAPI is §三十四 at the layer a client
// actually reaches. Two requests race for the same transition; exactly one may
// win, and the task must not end in a state neither of them asked for.
func TestConcurrentStatusChangesThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	p := h.register("2026 AgentMux/AgentMux")
	tk := h.createTask(t, p.ID, "work")
	if rec := h.patch("/api/tasks/"+tk.ID, `{"status":"RUNNING"}`); rec.Code != http.StatusOK {
		t.Fatalf("moving to RUNNING returned %d: %s", rec.Code, rec.Body.String())
	}

	const workers = 8
	start := make(chan struct{})
	results := make(chan int, workers)
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			results <- h.patch("/api/tasks/"+tk.ID, `{"status":"COMPLETED"}`).Code
		}()
	}
	close(start)

	accepted, refused, other := 0, 0, 0
	for i := 0; i < workers; i++ {
		switch code := <-results; {
		case code == http.StatusOK:
			accepted++
		case code == http.StatusConflict:
			refused++
		default:
			other++
			t.Errorf("a concurrent transition answered %d, want 200 or 409", code)
		}
	}
	// The service answers a request that asks for the status the task already
	// holds with the task as it stands, so several requests can be accepted and
	// only one of them wrote. What must never happen is a request failing for
	// some third reason, or a task left in a state nobody asked for.
	if accepted == 0 {
		t.Error("no concurrent transition was accepted")
	}
	if accepted+refused+other != workers {
		t.Errorf("counted %d outcomes for %d requests", accepted+refused+other, workers)
	}

	recorder := h.call(http.MethodGet, "/api/tasks/"+tk.ID, "")
	got := decode[taskResponse](t, recorder).Task
	if got.Status != task.StatusCompleted {
		t.Errorf("the task ended as %q, want %q", got.Status, task.StatusCompleted)
	}
	if got.CompletedAt == nil {
		t.Error("the winning transition did not record a completion time")
	}
}
