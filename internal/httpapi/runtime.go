package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/session"
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

	// Starting an agent waits for its process to appear, and stopping one waits
	// for it to go. Both waits are the runtime's, and both are bounded there;
	// these only have to be wider than the bounds they wrap.
	agentStartTimeout = 40 * time.Second
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
	// The response describes what is left, which is a stopped runtime, so a
	// client does not have to guess what to render next.
	rt, err := s.runtime.Runtime(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, runtimeResponse{Runtime: rt})
}

// agentResponse is the body of the endpoints that return an agent's state.
//
// It is wrapped like the runtime is, and for the same reason.
type agentResponse struct {
	Agent session.AgentStatus `json:"agent"`
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

// handleStartAgent implements POST /api/projects/{id}/runtime/agent/start.
//
// A project's runtime has to be running first. An agent with no terminal is a
// process nobody can see, interrupt, or read, and this product's whole premise
// is that a coding agent runs where its work can be watched.
func (s *Server) handleStartAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntime(w, r) {
		return
	}
	// The budget is wider than a runtime start's because it covers one too: an
	// agent can be asked for on a project whose runtime has never been up.
	ctx, cancel := s.contextWithTimeout(r, agentStartTimeout)
	defer cancel()

	agent, err := s.runtime.StartAgent(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, agentResponse{Agent: agent})
}

// handleStopAgent implements POST /api/projects/{id}/runtime/agent/stop.
//
// It interrupts the agent and leaves the runtime alone, which is what Ctrl-C
// does at the terminal. Ending the runtime as well is the runtime's own stop,
// and destroying the terminal is DELETE on the runtime.
func (s *Server) handleStopAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntime(w, r) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, agentStopTimeout)
	defer cancel()

	agent, err := s.runtime.StopAgent(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, agentResponse{Agent: agent})
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
