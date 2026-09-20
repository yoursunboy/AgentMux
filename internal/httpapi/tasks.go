package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/task"
)

// This file is the task and agent session API.
//
// What it is not is as much of the design as what it is:
//
//   - There is no DELETE on either resource. Nothing in this build removes a
//     task or an attempt, and a task that was cancelled is a record of work
//     somebody decided not to do - which is worth keeping. The foreign keys in
//     0004 and 0005 decide what would happen the day something does delete;
//     this phase does not open that door.
//   - There is no endpoint that starts anything. Creating a task records that
//     somebody wants work done. It starts no runtime, launches no agent and
//     writes no prompt: the two halves of AgentMux meet at the runtime API,
//     which already exists, and joining them is a later phase.
//   - A session is created with no runtime, and one is attached afterwards.
//     That window is a state the model names rather than hides, and POST here
//     deliberately does not accept a runtimeId - a caller that wants both makes
//     two requests, and the first one is true on its own. See
//     docs/TASK_MODEL.md §4.
//
// The project half of the path is the project the work belongs to, and the
// project is resolved by the service before anything is written, so a task
// cannot be created under a project that does not exist. A request body that
// tries to name one is refused by the unknown-field check rather than quietly
// ignored - see createTaskRequest.

// tasksTimeout bounds a task or session request.
//
// These are metadata operations over a local SQLite file, not runtime ones, so
// they get the metadata budget rather than the longer one a process launch
// needs.
const tasksTimeout = 10 * time.Second

// taskResponse is the body of the endpoints that return a single task.
type taskResponse struct {
	Task *task.Task `json:"task"`
}

// taskListResponse is the body of GET /api/projects/{id}/tasks.
type taskListResponse struct {
	Tasks []*task.Task `json:"tasks"`
	Count int          `json:"count"`
}

// sessionResponse is the body of the endpoints that return a single session.
type sessionResponse struct {
	Session *task.AgentSession `json:"session"`
}

// sessionListResponse is the body of GET /api/tasks/{id}/sessions.
type sessionListResponse struct {
	Sessions []*task.AgentSession `json:"sessions"`
	Count    int                  `json:"count"`
}

// createTaskRequest is the body of POST /api/projects/{id}/tasks.
//
// It has one field, and the project is not one of them. The project is in the
// path, and a body that also names one is an unknown field and refused. That is
// the point: two places to say which project a task belongs to is one place too
// many, and the disagreement between them would have to be resolved by a rule
// nobody wrote down.
type createTaskRequest struct {
	Title string `json:"title"`
}

// handleCreateTask implements POST /api/projects/{id}/tasks.
//
// The task begins in CREATED, with no session. Nothing is started.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	var req createTaskRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	t, err := s.tasks.CreateTask(ctx, task.CreateTaskInput{
		ProjectID: r.PathValue("id"),
		Title:     req.Title,
	})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, taskResponse{Task: t})
}

// handleListTasks implements GET /api/projects/{id}/tasks.
//
// The project is resolved before the list is read, so an unknown id is answered
// as a missing project rather than as a project with no tasks. Those two answers
// mean different things, and an empty list is the one a client cannot tell apart
// from a project that exists and has nothing in it.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	limit, ok := limitFrom(w, r)
	if !ok {
		return
	}

	tasks, err := s.tasks.ListTasks(ctx, task.ListTasksInput{
		ProjectID: r.PathValue("id"),
		// Passed through untrimmed-but-trimmed and unmodified: a status the model
		// does not know is refused with the list it does know, which is a more
		// useful answer than a silently empty page.
		Status: strings.TrimSpace(r.URL.Query().Get("status")),
		Limit:  limit,
	})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, taskListResponse{Tasks: tasks, Count: len(tasks)})
}

// handleGetTask implements GET /api/tasks/{id}.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	t, err := s.tasks.GetTask(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, taskResponse{Task: t})
}

// updateTaskRequest is the body of PATCH /api/tasks/{id}.
//
// Both fields are optional and both are pointers into a Present flag, because
// three things have to be distinguishable here: a field that was not sent, a
// field sent as a string, and a field sent as null. A plain *string collapses
// the first and the third, and the third is a mistake worth reporting rather
// than treating as "leave it alone".
type updateTaskRequest struct {
	Title  optionalString `json:"title"`
	Status optionalString `json:"status"`
}

// handleUpdateTask implements PATCH /api/tasks/{id}.
//
// The status is changed through this endpoint rather than through a POST to a
// /status sub-resource, because a status is a property of a task and not a
// resource of its own. A PATCH also makes the rule visible in the request: the
// caller says what the task should become, not what should happen to it.
//
// It is the only route that changes a task's status, and it cannot bypass the
// lifecycle: the service checks the transition against the status it reads and
// writes conditionally on that status still holding. A COMPLETED task cannot be
// moved back to RUNNING by any request this server accepts.
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	var req updateTaskRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	in := task.UpdateTaskInput{}
	if req.Title.Present {
		if req.Title.Value == nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				"title must be a string; a task title cannot be cleared", nil)
			return
		}
		in.Title = req.Title.Value
	}
	if req.Status.Present {
		if req.Status.Value == nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("status must be a string; a task status is one of %s",
					strings.Join(task.TaskStatuses(), ", ")), nil)
			return
		}
		in.Status = req.Status.Value
	}
	if in.Title == nil && in.Status == nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"the request must set title, status, or both", nil)
		return
	}

	t, err := s.tasks.UpdateTask(ctx, r.PathValue("id"), in)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, taskResponse{Task: t})
}

// createSessionRequest is the body of POST /api/tasks/{id}/sessions, and it has
// no fields.
//
// The task is in the path and the runtime is attached by a later request, so an
// attempt needs nothing else to exist. The body is still decoded rather than
// ignored, so that a client which sends a field this API does not accept is
// told so - and the body is optional, because "start an attempt at this task"
// is a complete request with nothing in it.
type createSessionRequest struct{}

// handleCreateSession implements POST /api/tasks/{id}/sessions.
//
// The session begins in CREATED with no runtime. That is not an oversight and it
// is not a gap to be filled by a transaction: creating an attempt and starting a
// process are two acts, and the second one launches something that can fail on
// its own. A single transaction across both would hold a write lock open for the
// length of a process launch and would make a tmux that refused to start undo
// the record that somebody wanted this work done at all.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	var req createSessionRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	sess, err := s.tasks.CreateSession(ctx, task.CreateSessionInput{
		TaskID: r.PathValue("id"),
	})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, sessionResponse{Session: sess})
}

// handleListSessions implements GET /api/tasks/{id}/sessions.
//
// One task's attempts, newest first. The task is resolved first, so an unknown
// id is a 404 about the task rather than an empty list of attempts.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	limit, ok := limitFrom(w, r)
	if !ok {
		return
	}

	sessions, err := s.tasks.ListSessions(ctx, task.ListSessionsInput{
		TaskID: r.PathValue("id"),
		Limit:  limit,
	})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK,
		sessionListResponse{Sessions: sessions, Count: len(sessions)})
}

// handleGetSession implements GET /api/sessions/{id}.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	sess, err := s.tasks.GetSession(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, sessionResponse{Session: sess})
}

// updateSessionRequest is the body of PATCH /api/sessions/{id}.
type updateSessionRequest struct {
	Status optionalString `json:"status"`

	// RuntimeID binds the attempt to the runtime it ran in.
	//
	// It is set here rather than at creation because the two are different
	// facts recorded at different times: the attempt exists first, and the
	// runtime it used is known when one is started. It may be set once - see
	// the repository's AttachSessionRuntime - and it is deliberately not
	// checked for existence: a runtime can be destroyed while the attempt that
	// used it remains, which is the whole reason it is not a foreign key.
	RuntimeID optionalString `json:"runtimeId"`
}

// handleUpdateSession implements PATCH /api/sessions/{id}.
//
// Two fields, and the order they are applied in is a decision.
//
// The status is written first, because it is the write that can lose a race: the
// service validates the transition against the status it read and the update is
// conditional on that status still holding. A caller told it lost a race is told
// so before a runtime has been attached on its behalf, which keeps the refusal
// honest - nothing about the request was applied.
//
// The reverse order would be worse in the case that matters: a runtime attached
// to an attempt whose status change then failed would leave a link recording
// that this attempt ran in a runtime, established by a request that was refused.
func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireTasks(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, tasksTimeout)
	defer cancel()

	var req updateSessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	id := r.PathValue("id")
	sess, err := s.tasks.GetSession(ctx, id)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}

	switch {
	case req.Status.Present && req.Status.Value == nil:
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("status must be a string; an agent session status is one of %s",
				strings.Join(task.AgentSessionStatuses(), ", ")), nil)
		return
	case req.RuntimeID.Present && req.RuntimeID.Value == nil:
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"runtimeId must be a string; a runtime is attached, never detached", nil)
		return
	case req.RuntimeID.Present && strings.TrimSpace(*req.RuntimeID.Value) == "":
		// A blank runtime id is refused here rather than by the service, because
		// the no-op comparison below would otherwise swallow it: a session that
		// has no runtime would be told it now has the one it asked for, and
		// nothing would have been recorded. There is no runtime whose id is the
		// empty string, so this is never a request anybody made on purpose.
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"runtimeId must name a runtime; a runtime id is its session name, "+
				"which is \"amx-\" followed by a project id", nil)
		return
	case !req.Status.Present && !req.RuntimeID.Present:
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"the request must set status, runtimeId, or both", nil)
		return
	}

	if req.Status.Present && *req.Status.Value != sess.Status {
		if sess, err = s.tasks.UpdateSessionStatus(ctx, id, *req.Status.Value); err != nil {
			writeServiceError(w, s.log, err)
			return
		}
	}
	if req.RuntimeID.Present && *req.RuntimeID.Value != sess.RuntimeID {
		if sess, err = s.tasks.AttachSessionRuntime(ctx, id, *req.RuntimeID.Value); err != nil {
			writeServiceError(w, s.log, err)
			return
		}
	}
	writeJSON(w, s.log, http.StatusOK, sessionResponse{Session: sess})
}

// optionalString is a string field that can tell "not sent" from "sent as
// null".
//
// encoding/json decodes both an absent field and an explicit null into a nil
// pointer, and the two are different here: absent means leave the field alone,
// while null means the caller asked to clear something that cannot be cleared.
// Reporting the second as the first would silently accept a request to unset a
// task's title.
type optionalString struct {
	// Present is true when the field appeared in the body at all.
	Present bool

	// Value is the string that was sent, or nil for an explicit null.
	Value *string
}

// UnmarshalJSON records that the field was sent, then decodes it.
func (v *optionalString) UnmarshalJSON(data []byte) error {
	v.Present = true
	if strings.TrimSpace(string(data)) == "null" {
		v.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v.Value = &s
	return nil
}

// limitFrom reads the `limit` query parameter, answering the request itself when
// it is malformed.
//
// The second result reports whether the caller should carry on. Zero means "the
// service's default": the parameter was absent, and a default is the service's
// to choose rather than this layer's to restate.
func limitFrom(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("limit must be a whole number, not %q", raw), nil)
		return 0, false
	}
	if value < 1 {
		// Zero is not "no limit" and is not "the default". A caller that asked
		// for nothing has made a mistake, and answering with a page would hide
		// it.
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("limit must be at least 1, not %d", value), nil)
		return 0, false
	}
	// Above the ceiling the value is passed through and the service clamps it,
	// which is where the ceiling is declared. A caller asking for a thousand
	// wants as much as it can have, not a refusal.
	return value, true
}

// requireTasks refuses a task request on a server built without a task service.
//
// The service is optional in Options for the same reason the event log and the
// terminal hub are: a test of the REST surface that is not about tasks should
// not have to build one. A server without it explains itself rather than
// panicking.
func (s *Server) requireTasks(w http.ResponseWriter) bool {
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this server was started without a task model", nil)
		return false
	}
	return true
}
