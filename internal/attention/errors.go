package attention

import (
	"errors"
	"fmt"
)

// Error codes produced by the attention and action projections.
//
// They follow the shape every other package uses - a stable code, a message
// safe to show, optional structured detail - so a caller can switch on the code
// without matching on a message that will be reworded.
const (
	// CodeInvalidAttention is an attention or an action that cannot be stored:
	// a missing identifier, a level outside the vocabulary, or a reason that is
	// empty or too long.
	CodeInvalidAttention = "agent_attention_invalid"

	// CodeInvalidAction is the same for an action specifically, so that a
	// caller told its action was refused is not sent looking at its attention.
	CodeInvalidAction = "agent_action_invalid"

	// CodeNotFound is attention or an action asked for that no event has
	// produced.
	//
	// It is the ordinary answer for an attempt nothing has observed. Every
	// attempt that exists has an attention row, so this is reached only for an
	// attempt the log has never mentioned.
	CodeNotFound = "agent_attention_not_found"

	// CodeStorageFailure is a projection write or read that could not complete.
	CodeStorageFailure = "agent_attention_storage_failure"

	// CodeProjectionFailed is an event that could not be folded.
	//
	// It is reported rather than returned: the event is already stored by the
	// time a projection runs, and a level that could not be derived does not
	// un-happen the thing it was derived from.
	CodeProjectionFailed = "agent_attention_projection_failed"
)

// Error is a projection failure carrying a stable code.
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

// IsCode reports whether err is a projection error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
