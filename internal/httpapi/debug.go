package httpapi

import (
	"net/http"
	"time"
)

// This file is the whole of the diagnostic surface, and this comment is the
// argument for why it may exist.
//
// Phase 4 deleted /api/debug. Those endpoints accepted raw terminal input and
// listed every session on the machine, they were unauthenticated, and a build
// that had them switched off still had them. docs/API.md says so where the
// endpoints used to be, and TestTheDiagnosticSurfaceIsGone runs the deletion as
// its criterion.
//
// One endpoint comes back, and it is not one of those. GET /api/debug/runtime
// accepts no input, holds no state, and answers four numbers and a boolean
// about this process. It cannot be used to type into a terminal, to start or
// stop anything, or to learn what any session contains: there is no field here
// for terminal output and no field here for input history, which is the same
// structural argument internal/usage makes about its table. A count of open
// sockets tells an operator that the server has leaked connections; it tells
// them nothing about what went through them.
//
// It is registered only when the server is configured with debug on, so the
// path 404s on an ordinary installation exactly as the deleted surface does -
// the route table is built from the configuration at construction, and there is
// no build in which this handler exists and answers.
//
// What is deliberately absent, and would be the shape of a mistake here:
// session names or ids, project names or paths, the socket directory, the
// database path, the tmux version string, counts of anything a person typed.

// debugTimeout bounds the probes this endpoint makes.
//
// It is longer than the five seconds the REST surface uses, because this
// endpoint exists to be asked on a machine that is already misbehaving: a
// server under enough load to make tmux slow to answer is exactly the server
// somebody runs this against, and a diagnostic that times out and explains
// nothing has failed at the one moment it was needed.
const debugTimeout = 15 * time.Second

// debugRuntimeResponse is the body of GET /api/debug/runtime.
//
// Every field is a count or a boolean. There is no string here that came from
// anywhere but this program, which is the property that makes the endpoint safe
// to leave reachable in a debug build: nothing a user typed, ran, or received
// can reach this struct.
type debugRuntimeResponse struct {
	// RuntimeCount is how many project runtimes this server is monitoring
	// right now - that is, how many it holds a live control subscription for.
	RuntimeCount int `json:"runtimeCount"`

	// TmuxAvailable is the dependency probe alone. It is separate from whether
	// a terminal can actually run, because an operator diagnosing a runtime
	// that will not start needs to know whether the missing piece is tmux.
	TmuxAvailable bool `json:"tmuxAvailable"`

	// ActiveSessions is how many sessions the runtime backend actually has. It
	// is a different fact from RuntimeCount and the difference is the
	// diagnostic: sessions without runtimes are ones left behind by a process
	// that died, and runtimes without sessions are ones whose session is gone
	// while the server still believes in it.
	//
	// It is zero when the backend cannot be listed, which is the honest answer
	// on a machine with no tmux - and TmuxAvailable says which of the two it is.
	ActiveSessions int `json:"activeSessions"`

	// WebSocketConnections is how many browser terminal sockets are open, and
	// Subscriptions how many of those are attached to a session. A connection
	// with no subscription is a viewer on a project it has not selected.
	WebSocketConnections int `json:"websocketConnections"`
	Subscriptions        int `json:"subscriptions"`
}

// handleDebugRuntime implements GET /api/debug/runtime.
//
// It is registered only in a debug build. Nothing it reports can fail the
// request: a probe that cannot answer is reported as a zero, because the whole
// value of this endpoint is that it answers on a machine where other things are
// not answering.
func (s *Server) handleDebugRuntime(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, debugTimeout)
	defer cancel()

	response := debugRuntimeResponse{
		RuntimeCount:  s.runtime.MonitorStats().Active,
		TmuxAvailable: s.tmuxAvailable(ctx),
	}

	// The hub is optional - a server built without one answers the real-time
	// endpoint with an explanation rather than a panic - so its numbers are
	// read the same way, and a missing hub reports zero sockets.
	if s.terminal != nil {
		stats := s.terminal.Stats()
		response.WebSocketConnections = stats.Connections
		response.Subscriptions = stats.Subscriptions
	}

	// A failure to list sessions is not reported as an error. The endpoint has
	// said what it knows either way, and an error here would turn "the tmux
	// server is not running" - a diagnosis - into a blank response.
	if sessions, err := s.runtime.Sessions(ctx); err != nil {
		s.log.WarnContext(ctx, "the debug endpoint could not list sessions", "error", err)
	} else {
		response.ActiveSessions = len(sessions)
	}

	writeJSON(w, s.log, http.StatusOK, response)
}
