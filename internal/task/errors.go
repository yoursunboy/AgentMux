package task

import (
	"errors"
	"fmt"
)

// Error codes produced by the task model.
//
// These are part of the API contract: the HTTP layer maps them to status codes
// and a client switches on them. Adding a code is fine; changing the meaning of
// an existing one is not.
const (
	// CodeInvalidInput is a malformed request: a missing title, a title that
	// is too long, a status that is not one of the declared ones.
	CodeInvalidInput = "invalid_input"

	// CodeInvalidTitle is a title that cannot be stored: empty, too long, not
	// valid UTF-8, or carrying control characters.
	CodeInvalidTitle = "invalid_task_title"

	// CodeInvalidTransition is a status change the lifecycle does not allow,
	// for example COMPLETED to RUNNING.
	CodeInvalidTransition = "invalid_status_transition"

	// CodeTaskNotFound is an unknown task identifier.
	CodeTaskNotFound = "task_not_found"

	// CodeSessionNotFound is an unknown agent session identifier.
	CodeSessionNotFound = "session_not_found"

	// CodeProjectNotFound is a task naming a project that does not exist.
	//
	// It is spelled the same as project.CodeNotFound and session's equivalent,
	// because it means the same thing to a client: the project you named is not
	// here. A client switching on this code does not need to know which layer
	// produced it.
	CodeProjectNotFound = "project_not_found"

	// CodeConflict means somebody else changed the same row first.
	//
	// It is produced by the conditional update the service uses instead of a
	// lock: the status the caller validated its transition against is no longer
	// the stored status, so the transition was never applied. The caller is told
	// it lost a race rather than being handed a state it did not ask for.
	CodeConflict = "status_conflict"

	// CodeStorageFailure is a failure inside the metadata store.
	CodeStorageFailure = "storage_failure"
)

// Error is a task-model failure carrying a stable code.
type Error struct {
	// Code is one of the Code* constants.
	Code string

	// Message is safe to show to a user.
	Message string

	// Details carries structured context, for example the status that was
	// refused or the one the row actually holds.
	Details map[string]any

	// Err is the underlying cause, when there is one.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// newError builds an Error with no underlying cause.
func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// wrapError builds an Error around an underlying cause.
func wrapError(err error, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// withDetail attaches structured context, for example the status that was
// refused.
func (e *Error) withDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 1)
	}
	e.Details[key] = value
	return e
}

// CodeOf returns the stable code for err, or "" when err carries no code.
func CodeOf(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// IsCode reports whether err is a task error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}

// ErrNotFound is returned by a Repository when a lookup matches nothing.
//
// Storage implementations return this rather than a storage-specific error so
// that the service layer can decide what a missing row means without knowing
// which database is behind it.
var ErrNotFound = errors.New("task not found")

// ErrSessionNotFound is returned by a Repository when a session lookup matches
// nothing. It is separate from ErrNotFound because a task and a session are
// different resources and a caller that confused them should be told which one
// was missing.
var ErrSessionNotFound = errors.New("agent session not found")

// ErrStatusConflict is returned by a Repository when a conditional status
// update matched no row, which means the row no longer holds the status the
// caller validated its transition against.
var ErrStatusConflict = errors.New("status changed underneath the caller")
