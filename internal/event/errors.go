package event

import (
	"errors"
	"fmt"
)

// Error codes produced by the event model.
//
// These are part of the API contract: the HTTP layer maps them to status codes
// and a client switches on them. Adding a code is fine; changing the meaning of
// an existing one is not.
const (
	// CodeInvalidEvent is a malformed event: an unknown source, a type that is
	// not a dotted name, a payload that is too large or that names a
	// credential.
	CodeInvalidEvent = "invalid_event"

	// CodeNotFound is an event identifier that no stored event carries. It is
	// produced when a pagination cursor cannot be resolved.
	CodeNotFound = "event_not_found"

	// CodeStorageFailure is a failure inside the metadata store.
	CodeStorageFailure = "storage_failure"
)

// Error is an event-model failure carrying a stable code.
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

// IsCode reports whether err is an event error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}

// ErrNotFound is returned by a Repository when a lookup matches nothing.
//
// Storage implementations return this rather than a storage-specific error so
// that the service layer can decide what a missing row means without knowing
// which database is behind it.
var ErrNotFound = errors.New("event not found")
