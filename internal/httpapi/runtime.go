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

// requireAgentCoordinator checks the runtime environment and the coordinator.
//
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
