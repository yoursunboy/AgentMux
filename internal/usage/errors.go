package usage

import (
	"errors"
	"fmt"
)

// Error codes produced by the usage recorder.
//
// They follow the shape every other package uses - a stable code, a message
// safe to show, optional structured detail - so that a caller can switch on the
// code without matching on a message that will be reworded.
const (
	// CodeInvalidEvent is an event that is not one of the five.
	//
	// It is a programming error rather than a user's: nothing reaches this
	// package from a request body, so a type outside the vocabulary means a
	// caller constructed one. It is refused rather than stored.
	CodeInvalidEvent = "usage_invalid_event"

	// CodeStorageFailure is a usage write or read that could not complete.
	//
	// The code string is the one every package uses for this, deliberately:
	// see the note in the storage layer, and the same constant in
	// internal/project, internal/task and the rest.
	CodeStorageFailure = "storage_failure"
)

// Error is a usage failure carrying a stable code.
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

// withDetail attaches structured context, for example the value that was
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

// IsCode reports whether err is a usage error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
