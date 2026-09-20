package claude

import (
	"errors"
	"fmt"
)

// Error codes produced by the adapter.
//
// They follow the shape the other packages use - a stable code, a message safe
// to show, optional structured detail - so that a caller can switch on the code
// without matching on a message that will be reworded.
const (
	// CodeInvalidConfig is a Start whose configuration cannot identify what is
	// being observed: a missing project or runtime, or an address that cannot
	// be bound.
	CodeInvalidConfig = "claude_adapter_config"

	// CodeNotStarted is an operation that needs a running adapter - Subscribe,
	// ConsumeStream, HookURL, HookSettings - asked of one that has not started
	// or has already stopped.
	CodeNotStarted = "claude_adapter_not_started"

	// CodeAlreadyStarted is a second Start on an adapter that is running.
	//
	// It is a refusal rather than an idempotent no-op. Start binds a port and
	// mints a hook path, so a second call would leave the first receiver
	// running with a path the caller no longer holds, and the hooks declared
	// against it would keep arriving at an adapter nobody is talking to.
	CodeAlreadyStarted = "claude_adapter_already_started"

	// CodeShutdownFailed is a Stop whose receiver did not shut down cleanly.
	//
	// It is reported but not fatal: the listener is closed directly whether or
	// not the graceful shutdown finished, so the address is released either way
	// and what the caller is being told is that a request may have been cut off.
	CodeShutdownFailed = "claude_adapter_shutdown_failed"

	// CodeStreamBroken is a stream-json read that could not continue: the
	// reader failed, or the caller's context ended.
	//
	// A single unreadable line is *not* this. A line that is not JSON, or one
	// longer than the reader will hold, is skipped and counted; ending the
	// stream over one of them would turn a stray debug line on stdout into a
	// lost turn. docs/CLAUDE_ADAPTER.md §4 says why that direction was chosen.
	CodeStreamBroken = "claude_stream_broken"
)

// Error is an adapter failure carrying a stable code.
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

// IsCode reports whether err is an adapter error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
