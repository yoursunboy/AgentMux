package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/agent"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/task"
)

// This file is the runtime half of the API: the endpoints that start, stop and
// describe a project's persistent terminal session.
//
// The handlers stay as thin here as everywhere else. Every decision about what
// a session is called, where it runs, how big it is, and what is written down
// about it belongs to the runtime manager, and a handler that started making
// those decisions would be the second place they are made.
//
// # Where the diagnostics went
//
// Until Phase 3 this file also served /api/debug/*: endpoints that accepted raw
// terminal input and returned raw terminal output, off by default, so the
// runtime could be exercised before there was a terminal to exercise it with.
// Phase 4 is that terminal. Keeping them would have meant two APIs that deliver
// bytes to a pane, one of them unauthenticated by design and reachable by a
// single flag, and every test that used them would have been a test that proved
// the debug path rather than the product path. They are deleted rather than
// deprecated, and the tests that used them drive the real WebSocket protocol
// instead.

// Timeouts. Starting a session creates a tmux server and a pty, and
// reconciliation inspects every session on the socket, so these are wider than
// the metadata endpoints' budgets.
const (
	runtimeStartTimeout   = 30 * time.Second
	runtimeStopTimeout    = 20 * time.Second
	runtimeDestroyTimeout = 20 * time.Second
	runtimeReadTimeout    = 10 * time.Second

	// Starting an agent covers starting a runtime as well, so this budget has to
	// be wider than the two it wraps rather than wider than one: the runtime
	// manager's own bound on bringing a terminal up is runtimeStartTimeout
	// above, and its default bound on waiting for the agent process to appear is
	// fifteen seconds. Both are spent inside this one request, so a budget the
	// sum did not fit in would abandon a launch that was proceeding normally and
	// report it as an agent that never appeared - the opposite of what happened.
	agentStartTimeout = 60 * time.Second
	agentStopTimeout  = 30 * time.Second

	// Restarting an agent is a stop and a start in one request, so its budget is
	// the two of them together rather than a third number chosen beside them.
	// Both halves are spent inside this one request and each already has a bound
	// that was chosen for what it does - the stop waits out the runtime manager's
	// whole grace for the process to actually go, which is the point of the call,
	// and the start may have to bring a terminal up as well. A budget the sum did
	// not fit in would abandon a restart that was proceeding normally and report
	// it as a failure, which is the one reading of this call that must never be
	// wrong.
	agentRestartTimeout = agentStopTimeout + agentStartTimeout
)

// runtimeResponse is the body of the endpoints that return a runtime.
//
// The runtime is wrapped rather than returned bare so that a later phase can
// add siblings - a diagnostics block, a capability list - without changing the
// shape of every existing response.
type runtimeResponse struct {
	Runtime *session.Runtime `json:"runtime"`
}

// handleGetRuntime implements GET /api/projects/{id}/runtime.
//
// A project whose runtime has never been started is reported as stopped rather
// than as a 404. The resource exists and is stopped, which gives a client one
// shape to render instead of two.
func (s *Server) handleGetRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntime(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, runtimeReadTimeout)
	defer cancel()

	rt, err := s.runtime.Runtime(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, runtimeResponse{Runtime: rt})
}

// handleStartRuntime implements POST /api/projects/{id}/runtime/start.
//
// It is idempotent. Starting a runtime that is already running returns it
// unchanged rather than failing, because the caller's intent is already
// satisfied and a user who reloaded a page should not see an error for it.
func (s *Server) handleStartRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntime(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, runtimeStartTimeout)
	defer cancel()

	rt, err := s.runtime.Start(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, runtimeResponse{Runtime: rt})
}

// handleStopRuntime implements POST /api/projects/{id}/runtime/stop.
//
// Stop ends the work and keeps the session, so its scrollback survives and the
// terminal can be looked at again. Removing the session is DELETE, which is a
// different and irreversible thing.
func (s *Server) handleStopRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntime(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, runtimeStopTimeout)
	defer cancel()

	rt, err := s.runtime.Stop(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	// Stopping the terminal stops whatever was running in it, the agent
	// included. Observation of it ends here, and the attempt is closed as
	// cancelled because a stop is something a person asked for - which is the
	// same answer `agent/stop` gives.
	s.releaseAgent(ctx, r.PathValue("id"), agent.OutcomeCancelled)
	writeJSON(w, s.log, http.StatusOK, runtimeResponse{Runtime: rt})
}

// handleDestroyRuntime implements DELETE /api/projects/{id}/runtime.
//
// This is the destructive one: the session is really removed, along with
// anything still running inside it and its scrollback. It is a separate verb
// from stop for exactly that reason.
func (s *Server) handleDestroyRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntime(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, runtimeDestroyTimeout)
	defer cancel()

	if err := s.runtime.Destroy(ctx, r.PathValue("id")); err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	// The terminal the agent was running in no longer exists. An adapter left
	// listening for a runtime that is gone is a port held for the life of the
	// process and a goroutine that will never see anything again, so it is
	// detached rather than left for the next start to replace. The attempt is
	// failed rather than cancelled: the environment it was running in was
	// removed, which is not the same as somebody stopping it.
	s.releaseAgent(ctx, r.PathValue("id"), agent.OutcomeFailed)

	// The response describes what is left, which is a stopped runtime, so a
	// client does not have to guess what to render next.
	rt, err := s.runtime.Runtime(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, runtimeResponse{Runtime: rt})
}

// releaseAgent ends observation of a project's agent after the runtime it was
// in has been stopped or destroyed.
//
// It is a no-op on a server without a coordinator, and on a project whose agent
// was never observed - which is most of them. Nothing here is allowed to fail
// the request: the runtime operation the caller asked for has already
// succeeded, and reporting a failure now would describe a request that did what
// it was told as one that did not.
func (s *Server) releaseAgent(ctx context.Context, projectID string, outcome agent.Outcome) {
	if s.agents == nil {
		return
	}
	if err := s.agents.Release(ctx, projectID, outcome); err != nil {
		s.log.Warn("could not release the agent observing a runtime",
			"projectId", projectID, "outcome", outcome, "error", err)
	}
}

// agentResponse is the body of the endpoints that return an agent's state.
//
// It is wrapped like the runtime is, and for the same reason: this phase added
// the attempt beside the agent without changing the shape of what was already
// here, which is what the wrapper was for.
type agentResponse struct {
	Agent session.AgentStatus `json:"agent"`

	// Session is the attempt this start created, when the caller named a task.
	// It is absent otherwise, and absent after a stop that closed nothing.
	Session *task.AgentSession `json:"session,omitempty"`

	// Retired is the attempt a restart closed on its way to the new one.
	//
	// It is reported because a restart is one request that ends one attempt and
	// begins another, and a caller that could only see the new one would have no
	// way to know what became of the agent it replaced - or whether there was
	// one to replace. Absent for every request that is not a restart, and absent
	// from a restart in which there was nothing open to retire.
	Retired *task.AgentSession `json:"retired,omitempty"`
}

// handleGetAgent implements GET /api/projects/{id}/runtime/agent.
//
// The agent is also reported inside the runtime itself, under `runtime.agent`.
// This endpoint exists so that a client polling only the agent - a panel that
// shows whether Claude is up without re-reading the whole runtime - has one
// small thing to poll instead of a whole terminal description.
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntime(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, runtimeReadTimeout)
	defer cancel()

	agent, err := s.runtime.Agent(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, agentResponse{Agent: agent})
}

// startAgentRequest is the body of POST /api/projects/{id}/runtime/agent/start.
//
// It has one field and it is optional, so the body itself is optional: the call
// that says "make sure Claude is running here" is complete with nothing in it,
// which is what the workspace's Start button sends and has always sent.
type startAgentRequest struct {
	// TaskID is the task this is an attempt at.
	//
	// Naming one asks for the attempt to be recorded as well as the agent to be
	// started, and the two are genuinely different requests - one is "run
	// Claude here" and the other is "and this is the work it is doing". The
	// field is optional because a caller may want either, and a requirement
	// would make the second impossible to decline.
	TaskID string `json:"taskId"`
}

// handleStartAgent implements POST /api/projects/{id}/runtime/agent/start.
//
// # What it does now
//
// It starts a runtime when there is not one, launches Claude in it under a
// session id AgentMux chooses and a hook configuration pointing at an adapter
// this server is running, and - when a task is named - records the attempt and
// binds it. The observer is what makes any of it visible: an agent started
// without one runs exactly as it did before this phase and tells AgentMux
// nothing.
//
// The budget is wider than a runtime start's because it covers one too. A
// stopped project is a project whose runtime has never been up, and one call
// goes from that to a running agent, so the two bounds have to fit inside this
// one rather than each getting a request of its own.
func (s *Server) handleStartAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentCoordinator(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, agentStartTimeout)
	defer cancel()

	// An empty body is the ordinary case, not a malformed one. decodeJSON
	// reports it separately from a body that is not JSON so that a client which
	// posts nothing is not told it posted something wrong.
	var req startAgentRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	result, err := s.agents.Start(ctx, agent.StartInput{
		ProjectID: r.PathValue("id"),
		TaskID:    req.TaskID,
	})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, agentResponse{
		Agent:   result.Agent,
		Session: result.Session,
	})
}

// handleStopAgent implements POST /api/projects/{id}/runtime/agent/stop.
//
// It interrupts the agent and leaves the runtime alone, which is what Ctrl-C
// does at the terminal. Ending the runtime as well is the runtime's own stop,
// and destroying the terminal is DELETE on the runtime.
func (s *Server) handleStopAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentCoordinator(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, agentStopTimeout)
	defer cancel()

	result, err := s.agents.Stop(ctx, agent.StopInput{ProjectID: r.PathValue("id")})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, agentResponse{
		Agent:   result.Agent,
		Session: result.Session,
	})
}

// restartAgentRequest is the body of
// POST /api/projects/{id}/runtime/agent/restart.
//
// It is startAgentRequest, and it is a separate type rather than a shared one
// because the two are separate contracts: a field added for a restart and read
// by a start would be a change to the start endpoint nobody asked for. Both are
// one optional field today, and that is a coincidence of the current shape
// rather than a rule.
type restartAgentRequest struct {
	// TaskID is the task the *new* attempt is at. It means what it means on a
	// start, and it says nothing about the attempt being retired: that one keeps
	// the task it was already recorded against.
	TaskID string `json:"taskId"`
}

// handleRestartAgent implements POST /api/projects/{id}/runtime/agent/restart.
//
// # Why this is one request and not two
//
// A restart is a stop and a start, and the client could have made both calls.
// It must not, and this endpoint exists so that it does not have to.
//
// The two calls would not be one operation. Between them the project has no
// agent, which is a state a concurrent reader sees and a concurrent writer can
// act on - a Start arriving in that gap launches an agent the restart then
// launches over, and the project ends with two Claudes in it, which is the one
// arrangement this whole design exists to prevent. The gap is also where the
// stop's answer gets lost: a stop that timed out is a 409, and a client that
// then started anyway would be starting a second agent beside one that never
// went. Made here, both halves are one critical section, and the stop's answer
// is this request's answer.
//
// It is not a runtime restart. The terminal is not torn down, the session is
// not recreated and the scrollback is not lost - §六 of the phase brief - and
// the runtime keeps its name, so every client already following it carries on.
//
// # The budget
//
// It has to cover a stop that is allowed its whole grace and a start that may
// have to bring a terminal up as well, so it is the start's budget plus the
// stop's. A restart that is going to time out has to be reported as a timeout
// rather than as a request that ran out of time, for the same reason the
// individual endpoints have bounds of their own.
func (s *Server) handleRestartAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentCoordinator(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, agentRestartTimeout)
	defer cancel()

	var req restartAgentRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	result, err := s.agents.Restart(ctx, agent.RestartInput{
		ProjectID: r.PathValue("id"),
		TaskID:    req.TaskID,
	})
	if err != nil {
		// The retired attempt is reported even here. A restart that stopped the
		// agent and could not start a new one has still changed the project, and
		// a caller told only that the request failed would not know that - or
		// which record to look at for what used to be running.
		if result.Retired != nil {
			writeServiceErrorDetailed(w, s.log, err, map[string]any{
				"retired": map[string]any{
					"agentSessionId": result.Retired.ID,
					"status":         result.Retired.Status,
				},
			})
			return
		}
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, agentResponse{
		Agent:   result.Agent,
		Session: result.Session,
		Retired: result.Retired,
	})
}

// The order matters: a server on a host that cannot run a terminal is told so
// first, because that is the answer the user can act on. "This server was
// started without an agent coordinator" is a wiring fact about one process, and
// it is only worth saying once the environment is not the problem.
func (s *Server) requireAgentCoordinator(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireRuntime(w, r) {
		return false
	}
	if s.agents == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this server was started without an agent coordinator", nil)
		return false
	}
	return true
}

// requireRuntime refuses a runtime request on a host that cannot execute one.//
// The reason is the host adapter's, not this package's, because it is the part
// of AgentMux that knows the difference between "tmux is not installed" and
// "this server is running on the wrong side of the WSL boundary". The second
// has a fix - start the server inside the distribution - and a user who is
// told "tmux not found" while tmux is installed and working one command away
// has been told something true and useless.
func (s *Server) requireRuntime(w http.ResponseWriter, r *http.Request) bool {
	if s.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, session.CodeUnavailable,
			"this server was started without a terminal runtime", nil)
		return false
	}
	info := s.host.Info(r.Context())
	if info.RuntimeAvailable {
		return true
	}
	reason := strings.TrimSpace(info.RuntimeUnavailableReason)
	if reason == "" {
		reason = "the terminal runtime is not available in this environment"
	}
	writeError(w, http.StatusServiceUnavailable, session.CodeUnavailable, reason, nil)
	return false
}
