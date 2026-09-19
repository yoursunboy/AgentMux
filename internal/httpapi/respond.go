package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
)

// Error codes produced by the HTTP layer itself. Codes produced by the project
// model are defined in the project package and passed through unchanged, so a
// client can switch on one vocabulary across the whole API: a storage failure
// is reported as project.CodeStorageFailure, not a second spelling of it.
const (
	CodeNotFound       = "not_found"
	CodeInternal       = "internal_error"
	CodeInvalidRequest = "invalid_request"

	// CodeForbidden means the request came from somewhere this server will not
	// serve. It is used by the WebSocket upgrade, where the caller is a browser
	// page on another origin rather than a client that got a detail wrong.
	CodeForbidden = "forbidden"
)

// APIError is the body of a failing response.
//
// Every failure uses this shape, including failures the router produces, so a
// client never has to guess whether a body is JSON or an HTML error page.
type APIError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// errorEnvelope wraps APIError so a success body can never be mistaken for a
// failure.
type errorEnvelope struct {
	Error APIError `json:"error"`
}

// writeJSON writes a successful JSON response.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Encoding our own response failed, so the response cannot be saved.
		// Report a plain 500 rather than a half-written body.
		if log != nil {
			log.Error("could not encode response", "error", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"could not encode the response"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError writes a failing JSON response.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	body, err := json.Marshal(errorEnvelope{Error: APIError{
		Code:    code,
		Message: message,
		Details: details,
	}})
	if err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeServiceError maps a service failure onto a status code and a stable
// error code.
//
// The underlying cause is logged but not returned: a driver message or a
// filesystem path in an error body is noise to a user and can leak more about
// the host than the client needs.
func writeServiceError(w http.ResponseWriter, log *slog.Logger, err error) {
	code := codeOf(err)
	if code == "" {
		if log != nil {
			log.Error("request failed", "error", err)
		}
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"the server encountered an unexpected error", nil)
		return
	}

	status := statusForCode(code)
	if status >= http.StatusInternalServerError && log != nil {
		log.Error("request failed", "code", code, "error", err)
	}
	writeError(w, status, code, messageFor(err, code), detailsOf(err))
}

// codeOf returns the stable code carried by err, whichever layer produced it.
//
// Three packages define codes - the project model, the runtime, and the event
// log - and all are passed through to the client unchanged, so a client
// switches on one vocabulary across the whole API. The HTTP layer adds only the
// codes for failures it produces itself.
func codeOf(err error) string {
	if code := project.CodeOf(err); code != "" {
		return code
	}
	if code := session.CodeOf(err); code != "" {
		return code
	}
	return event.CodeOf(err)
}

// detailsOf returns the structured context attached to err, if any.
func detailsOf(err error) map[string]any {
	var projectErr *project.Error
	if errors.As(err, &projectErr) {
		return projectErr.Details
	}
	var sessionErr *session.Error
	if errors.As(err, &sessionErr) {
		return sessionErr.Details
	}
	var eventErr *event.Error
	if errors.As(err, &eventErr) {
		return eventErr.Details
	}
	return nil
}

// messageFor returns the user-facing message for a failure.
func messageFor(err error, code string) string {
	var projectErr *project.Error
	if errors.As(err, &projectErr) && projectErr.Message != "" {
		return projectErr.Message
	}
	var sessionErr *session.Error
	if errors.As(err, &sessionErr) && sessionErr.Message != "" {
		return sessionErr.Message
	}
	var eventErr *event.Error
	if errors.As(err, &eventErr) && eventErr.Message != "" {
		return eventErr.Message
	}
	switch code {
	// One clause, because project.CodeStorageFailure, session.CodeStorageFailure
	// and event.CodeStorageFailure are the same string: the shared vocabulary
	// means a caller cannot tell which store failed, and does not need to.
	case project.CodeStorageFailure:
		return "the metadata store could not complete the request"
	default:
		return "the request could not be completed"
	}
}

// statusForCode maps a service error code to an HTTP status.
//
// The mapping is explicit rather than derived from a prefix, so that adding a
// code forces a decision about what it means to a client.
//
// Some codes are deliberately shared between the layers and appear here
// once: project.CodeStorageFailure, session.CodeStorageFailure and
// event.CodeStorageFailure are all "storage_failure", and project.CodeNotFound
// and session.CodeProjectNotFound are both "project_not_found". That is the
// point of the shared vocabulary - a client that switches on "storage_failure"
// does not need to know which layer produced it.
func statusForCode(code string) int {
	switch code {
	case project.CodeInvalidInput,
		project.CodeInvalidName,
		project.CodeNotADirectory,
		project.CodePathIsProjectsRoot,
		// An event that the event model refuses: a malformed source, a type that
		// is not a dotted name, or a payload that is too large or that names a
		// credential. Nothing in this phase writes an event from a request, so
		// this is reachable only through a cursor or a query parameter - but the
		// mapping belongs here rather than being implied by a 500.
		event.CodeInvalidEvent,
		session.CodeInvalidSize:
		return http.StatusBadRequest

	case project.CodePathNotFound,
		project.CodeNotFound,
		// An event id that no stored event carries, which is what a pagination
		// cursor is: asking to page from a row that does not exist is a request
		// about a resource that is not there.
		event.CodeNotFound,
		session.CodeNotFound:
		return http.StatusNotFound

	case project.CodePathNotAccessible,
		project.CodeOutsideProjectsRoot:
		return http.StatusForbidden

	case project.CodeAlreadyRegistered,
		project.CodeTargetExists,
		// The project is fine; its runtime is already in the state the caller
		// asked for, or is in one that makes the call meaningless. That is a
		// conflict with the current state, not a bad request and not a missing
		// resource.
		session.CodeAlreadyRunning,
		session.CodeNotRunning,
		// An agent cannot be typed into a terminal whose foreground process is
		// another program. The runtime is fine and the request is fine; the
		// session is busy, which is a state, and states change.
		session.CodeAgentTerminalBusy:
		return http.StatusConflict

	case project.CodeRuntimePathMappingFailed,
		// The agent started, and not where the project is. Nothing the caller
		// sends would change that, and nothing about the server is broken: the
		// project's runtime path and the directory the process ended up in are
		// two strings that have to name the same place, and on this machine
		// they do not. That is the same shape of failure as a host path with no
		// runtime equivalent, so it gets the same answer.
		session.CodeAgentWrongDirectory:
		return http.StatusUnprocessableEntity

	case project.CodeGitUnavailable,
		// runtime_unavailable is the structural case: this server cannot host a
		// terminal runtime at all, whatever the user does with tmux. 503 rather
		// than 500 because the service is genuinely unavailable rather than
		// broken, and because the message says how to fix it.
		session.CodeUnavailable,
		session.CodeBackendUnavailable,
		// agent_unavailable is the same case one level up: there is no coding
		// agent to host here. Either none is installed, or one is installed and
		// could not be resolved, or the server is on the wrong side of the WSL
		// boundary, and the message says which. Offering 500 would tell a client
		// to report a bug where the honest answer is "not on this machine".
		session.CodeAgentUnavailable:
		return http.StatusServiceUnavailable

	case project.CodeGitInitFailed,
		// One clause for three layers: an event log that cannot be read is the
		// same failure as a metadata store that cannot be read, and the three
		// CodeStorageFailure constants are one string. A timeline that cannot be
		// read is the server failing to do something it said it would do, and
		// there is nothing the caller could send differently.
		project.CodeStorageFailure,
		session.CodeStartFailed,
		session.CodeStopFailed,
		session.CodeDestroyFailed,
		session.CodeInputFailed,
		session.CodeResizeFailed,
		session.CodeBackendFailure,
		// The agent was started and did not appear, or was asked to stop and the
		// request could not be delivered. Both are the server failing to do
		// something it said it would do, which is a 500 and not the caller's
		// problem to solve.
		session.CodeAgentLaunchFailed,
		session.CodeAgentStopFailed:
		return http.StatusInternalServerError

	default:
		return http.StatusInternalServerError
	}
}
