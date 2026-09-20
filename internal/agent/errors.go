package agent

import (
	"errors"
	"fmt"
)

// Error codes produced by the coordinator.
//
// They follow the shape every other package uses - a stable code, a message
// safe to show, optional structured detail - so a caller can switch on the code
// without matching on a message that will be reworded.
const (
	// CodeInvalidInput is a request that cannot name what it is about.
	CodeInvalidInput = "agent_invalid_input"

	// CodeAgentRunning is a request to record an attempt at something that is
	// already running and was not started by AgentMux.
	//
	// It is a refusal and not a retry, because the thing being refused is not a
	// race: the running process has a session id AgentMux never chose and no
	// hook configuration pointing at a receiver, so there is no observation to
	// bind an attempt to. Recording one anyway would put an attempt into the
	// task history with nothing behind it.
	CodeAgentRunning = "agent_already_running"

	// CodeAgentNotFound is an operation on a runtime that is not being observed.
	CodeAgentNotFound = "agent_not_bound"

	// CodeSettingsFailure is a hook configuration document that could not be
	// written or removed.
	CodeSettingsFailure = "agent_settings_failure"

	// CodeTaskNotFound is a session requested against a task that does not
	// exist.
	CodeTaskNotFound = "agent_task_not_found"

	// CodeTaskMismatch is a task that exists, but not in the project the request
	// named.
	//
	// It is separate from CodeTaskNotFound because it is a different mistake
	// with a different fix: the task is real and the caller knew about it, they
	// simply asked for it under the wrong project. Answering "not found" would
	// tell them to look for a task they already have.
	CodeTaskMismatch = "agent_task_mismatch"
)

// Error is a coordinator failure carrying a stable code.
type Error struct {
	// Code is one of the Code* constants.
	Code string

	// Message is safe to show to a user.
	Message string

	// Details carries structured context, for example the offending field.
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

// withDetail attaches structured context, for example the field that was
// rejected.
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

// IsCode reports whether err is a coordinator error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
