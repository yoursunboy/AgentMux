package controller

import (
	"errors"
	"fmt"
)

// Error codes produced by the aggregation.
//
// There is only one, and that is the point: this package reads other services
// and arranges what they say, so almost everything that can go wrong is a
// failure of a service underneath it and carries that service's own code. A
// catalogue of codes here would be a second vocabulary for failures this package
// did not cause.
const (
	// CodeUnavailable is an aggregation that could not read the projects it is
	// about.
	//
	// It is deliberately not raised for a projection that is missing or that
	// failed: those degrade to an `available: false` section in the response
	// rather than failing the request, because a dashboard that shows five of
	// seven things is worth more than an error page. docs/CONTROLLER_API.md §5.
	CodeUnavailable = "controller_unavailable"
)

// Error is an aggregation failure carrying a stable code.
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

// CodeOf returns the stable code for err, or "" when err carries no code.
//
// It reports the code this package produced. An error from a service underneath
// carries that service's code and is passed through by the HTTP layer's own
// chain, so nothing here has to know about it.
func CodeOf(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// IsCode reports whether err is an aggregation error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
