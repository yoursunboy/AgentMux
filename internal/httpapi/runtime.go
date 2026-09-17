package httpapi

import (
	"net/http"
	"strconv"
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

// Timeouts. Starting a session creates a tmux server and a pty, and
// reconciliation inspects every session on the socket, so these are wider than
// the metadata endpoints' budgets.
const (
	runtimeStartTimeout   = 30 * time.Second
	runtimeStopTimeout    = 20 * time.Second
	runtimeDestroyTimeout = 20 * time.Second
	runtimeReadTimeout    = 10 * time.Second
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

// requireRuntime refuses a runtime request on a host that cannot execute one.
//
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

// Debug endpoints. They exist so the runtime can be exercised end to end before
// there is a Web Terminal to exercise it with, they are off unless the
// configuration turns them on, and they are expected to be deleted when Phase 4
// provides the real thing. See docs/RUNTIME.md.

// debugOutputResponse is the body of GET /api/debug/projects/{id}/runtime/output.
type debugOutputResponse struct {
	ProjectID string `json:"projectId"`

	// Chunks are the buffered output after the requested sequence number.
	//
	// Chunk.Data is a byte slice, which encoding/json renders as base64, so a
	// client gets the terminal's bytes exactly rather than a lossy string.
	Chunks []session.Chunk `json:"chunks"`

	// Sequence is the highest sequence number produced so far, which is what a
	// client passes back as ?since= to resume.
	Sequence uint64 `json:"sequence"`

	// Session and Snapshot are only filled in when ?snapshot= is given.
	//
	// Snapshot is tmux's own rendering of the visible pane with its ANSI
	// sequences kept. It is a diagnostic: it is a redraw of the screen, not a
	// stream, and no part of the product reads it. It is a byte slice for the
	// same reason Chunk.Data is - the pane contains escape sequences and
	// possibly text that is not valid UTF-8, and an encoding that round-trips
	// only text would mangle exactly the output this endpoint exists to inspect.
	Session  *session.Session `json:"session,omitempty"`
	Snapshot []byte           `json:"snapshot,omitempty"`
}

// handleDebugRuntimeOutput implements GET /api/debug/projects/{id}/runtime/output.
func (s *Server) handleDebugRuntimeOutput(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, runtimeReadTimeout)
	defer cancel()

	projectID := r.PathValue("id")
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	chunks, err := s.runtime.History(ctx, projectID, since)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}

	response := debugOutputResponse{ProjectID: projectID, Chunks: chunks}
	if response.Chunks == nil {
		response.Chunks = []session.Chunk{}
	}

	// The runtime is read after the history so that the sequence number
	// reported is never lower than the last chunk returned.
	if rt, err := s.runtime.Runtime(ctx, projectID); err == nil {
		response.Sequence = rt.Sequence
	}
	if r.URL.Query().Get("snapshot") != "" {
		if described, err := s.runtime.Describe(ctx, projectID); err == nil {
			response.Session = described
			if snapshot, err := s.runtime.Snapshot(ctx, projectID); err == nil {
				response.Snapshot = snapshot
			}
		}
	}
	writeJSON(w, s.log, http.StatusOK, response)
}

// debugInputRequest is the body of POST /api/debug/projects/{id}/runtime/input.
//
// Both forms are accepted because they answer different questions. Text is what
// a person types; bytes are what a terminal receives, and a test that wants to
// deliver an escape sequence, a control character, or a byte that is not valid
// UTF-8 has to be able to say so exactly. Merging them into one string field
// would quietly restrict the runtime to the subset of input that survives a
// round trip through JSON text.
type debugInputRequest struct {
	// Text is delivered as its UTF-8 bytes.
	Text string `json:"text"`

	// Bytes are delivered exactly as given.
	Bytes []byte `json:"bytes"`

	// Enter appends a carriage return. It applies to whichever of Text and
	// Bytes was given.
	Enter bool `json:"enter"`

	// Keys are command lines, each typed and then run.
	Keys []string `json:"keys"`
}

// handleDebugRuntimeInput implements POST /api/debug/projects/{id}/runtime/input.
func (s *Server) handleDebugRuntimeInput(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, runtimeReadTimeout)
	defer cancel()

	var req debugInputRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	// Text and bytes in one body have no defined order - they are two fields,
	// not a sequence - so rather than pick one and hope, this refuses. A
	// diagnostic endpoint that silently reordered a test's input would send the
	// test's author looking in the wrong place.
	if req.Text != "" && len(req.Bytes) > 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"give either text or bytes, not both: text is delivered as UTF-8 and bytes are delivered exactly, "+
				"and a body carrying both leaves their order undefined", nil)
		return
	}

	projectID := r.PathValue("id")
	data := req.Bytes
	if req.Text != "" {
		data = []byte(req.Text)
	}
	if req.Enter {
		data = append(append([]byte(nil), data...), '\r')
	}

	if len(data) > 0 {
		if err := s.runtime.Input(ctx, projectID, data); err != nil {
			writeServiceError(w, s.log, err)
			return
		}
	}
	for _, line := range req.Keys {
		if err := s.runtime.Launch(ctx, projectID, line); err != nil {
			writeServiceError(w, s.log, err)
			return
		}
	}

	rt, err := s.runtime.Runtime(ctx, projectID)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, runtimeResponse{Runtime: rt})
}

// debugResizeRequest is the body of POST /api/debug/projects/{id}/runtime/resize.
type debugResizeRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// handleDebugRuntimeResize implements POST /api/debug/projects/{id}/runtime/resize.
//
// It calls the same method the terminal UI will call in a later phase, so the
// resize path is exercised for real rather than only through the manager's
// tests.
func (s *Server) handleDebugRuntimeResize(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, runtimeReadTimeout)
	defer cancel()

	var req debugResizeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}
	rt, err := s.runtime.Resize(ctx, r.PathValue("id"), req.Cols, req.Rows)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, runtimeResponse{Runtime: rt})
}

// debugRuntimesResponse is the body of GET /api/debug/runtimes.
type debugRuntimesResponse struct {
	// Backend names the session backend in use, so a reader knows which
	// runtime the sessions below belong to.
	Backend string `json:"backend"`

	// SocketDir is where the project runtimes live. It is reported because
	// since Phase 2.5 there is no single server to inspect: a session is only
	// meaningful together with the socket it is on, and a reader who wants to
	// look at one themselves needs to know where to look.
	SocketDir string `json:"socketDir"`

	// Sessions are every AgentMux session on every socket in the socket
	// directory, including ones no registered project claims.
	Sessions []session.SessionRef `json:"sessions"`

	// Orphans are the sessions from the most recent reconciliation that no
	// registered project claims. AgentMux reports them and does not touch them.
	Orphans []session.Orphan `json:"orphans"`
}

// handleDebugRuntimes implements GET /api/debug/runtimes.
func (s *Server) handleDebugRuntimes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, runtimeReadTimeout)
	defer cancel()

	sessions, err := s.runtime.Sessions(ctx)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	if sessions == nil {
		sessions = []session.SessionRef{}
	}
	orphans := s.runtime.Orphans()
	if orphans == nil {
		orphans = []session.Orphan{}
	}
	writeJSON(w, s.log, http.StatusOK, debugRuntimesResponse{
		Backend:   s.runtime.BackendName(),
		SocketDir: s.runtime.SocketDir(),
		Sessions:  sessions,
		Orphans:   orphans,
	})
}

// handleDebugReconcile implements POST /api/debug/reconcile.
//
// The same reconciliation the server runs at startup, on demand, which is what
// makes "what would a restart make of this" answerable without a restart.
func (s *Server) handleDebugReconcile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, runtimeStartTimeout)
	defer cancel()

	report, err := s.runtime.Reconcile(ctx)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, report)
}

// parseSince reads the ?since= resume point. An absent value means zero, which
// asks for the whole buffered history.
func parseSince(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	since, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errInvalidSince
	}
	return since, nil
}

// errInvalidSince reports a malformed resume point.
var errInvalidSince = errString("since must be a non-negative integer")

// errString is a constant error message.
type errString string

func (e errString) Error() string { return string(e) }
