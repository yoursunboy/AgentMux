package project

import (
	"errors"
	"fmt"
)

// Error codes produced by the project model.
//
// These are part of the API contract: the HTTP layer maps them to status
// codes and the frontend switches on them to decide what to tell the user.
// Adding a code is fine; changing the meaning of an existing one is not.
const (
	// CodeInvalidInput is a malformed request body.
	CodeInvalidInput = "invalid_input"

	// CodeInvalidName is a project name that cannot be used as a directory
	// name.
	CodeInvalidName = "invalid_project_name"

	// CodePathNotFound is a path that does not exist.
	CodePathNotFound = "path_not_found"

	// CodeNotADirectory is a path that exists but is a file.
	CodeNotADirectory = "path_not_a_directory"

	// CodePathNotAccessible is a path that exists but cannot be read, for
	// example because of directory permissions.
	CodePathNotAccessible = "path_not_accessible"

	// CodeOutsideProjectsRoot is a path outside every configured Projects
	// Root. AgentMux refuses to manage such a path.
	CodeOutsideProjectsRoot = "path_outside_projects_root"

	// CodePathIsProjectsRoot is an attempt to register a Projects Root itself
	// as a project.
	CodePathIsProjectsRoot = "path_is_projects_root"

	// CodeAlreadyRegistered is a path that is already registered.
	CodeAlreadyRegistered = "project_already_registered"

	// CodeTargetExists is a New Project target that already exists and is not
	// empty.
	CodeTargetExists = "path_already_exists"

	// CodeGitUnavailable means git is not installed or not on PATH.
	CodeGitUnavailable = "git_unavailable"

	// CodeGitInitFailed means git ran and failed.
	CodeGitInitFailed = "git_init_failed"

	// CodeRuntimePathMappingFailed means the HostAdapter could not translate
	// the host path into a runtime path.
	CodeRuntimePathMappingFailed = "runtime_path_mapping_failed"

	// CodeNotFound is an unknown project identifier.
	CodeNotFound = "project_not_found"

	// CodeStorageFailure is a failure inside the metadata store.
	CodeStorageFailure = "storage_failure"
)

// Error is a project-model failure carrying a stable code.
type Error struct {
	// Code is one of the Code* constants.
	Code string

	// Message is safe to show to a user.
	Message string

	// Details carries structured context, for example the conflicting path.
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

// withDetail attaches structured context, for example the path that clashed.
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

// IsCode reports whether err is a project error with the given code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
