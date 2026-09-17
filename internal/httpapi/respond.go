package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/kutonlagos/agentmux/internal/project"
)

// Error codes produced by the HTTP layer itself. Codes produced by the project
// model are defined in the project package and passed through unchanged, so a
// client can switch on one vocabulary across the whole API: a storage failure
// is reported as project.CodeStorageFailure, not a second spelling of it.
const (
	CodeNotFound       = "not_found"
	CodeInternal       = "internal_error"
	CodeInvalidRequest = "invalid_request"
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
	code := project.CodeOf(err)
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

	var projectErr *project.Error
	details := map[string]any(nil)
	if errors.As(err, &projectErr) {
		details = projectErr.Details
	}
	writeError(w, status, code, messageFor(err, code), details)
}

// messageFor returns the user-facing message for a failure.
func messageFor(err error, code string) string {
	var projectErr *project.Error
	if errors.As(err, &projectErr) && projectErr.Message != "" {
		return projectErr.Message
	}
	switch code {
	case project.CodeStorageFailure:
		return "the metadata store could not complete the request"
	default:
		return "the request could not be completed"
	}
}

// statusForCode maps a project-model error code to an HTTP status.
//
// The mapping is explicit rather than derived from a prefix, so that adding a
// code forces a decision about what it means to a client.
func statusForCode(code string) int {
	switch code {
	case project.CodeInvalidInput,
		project.CodeInvalidName,
		project.CodeNotADirectory,
		project.CodePathIsProjectsRoot:
		return http.StatusBadRequest

	case project.CodePathNotFound,
		project.CodeNotFound:
		return http.StatusNotFound

	case project.CodePathNotAccessible,
		project.CodeOutsideProjectsRoot:
		return http.StatusForbidden

	case project.CodeAlreadyRegistered,
		project.CodeTargetExists:
		return http.StatusConflict

	case project.CodeRuntimePathMappingFailed:
		return http.StatusUnprocessableEntity

	case project.CodeGitUnavailable:
		return http.StatusServiceUnavailable

	case project.CodeGitInitFailed:
		return http.StatusInternalServerError

	case project.CodeStorageFailure:
		return http.StatusInternalServerError

	default:
		return http.StatusInternalServerError
	}
}
