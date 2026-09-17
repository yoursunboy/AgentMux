// Package session owns the persistent terminal runtime.
//
// A session is a terminal that outlives everything that talks to it: the
// browser, the AgentMux server process, and the request that created it. That
// property is the product, not a detail - a coding agent that dies because a
// laptop lid closed is useless.
//
// The package is split so the parts can be understood, tested, and replaced
// one at a time:
//
//	Backend       talks to whatever owns the PTY (tmux today)
//	Manager       maps projects onto sessions and owns their state
//	Subscription  a live output stream from one session
//
// Nothing here knows about HTTP. Nothing here knows about tmux except tmux.go
// and control.go.
//
// # Where this runs
//
// The backend needs a POSIX host: tmux, the PTY, and the agent it will host
// are Linux processes. On Windows that means the AgentMux server must itself
// run inside WSL rather than reaching into it from outside, so that a
// terminal's bytes, signals, and lifetime all live in one place. A Windows
// server refuses to start a runtime and says why; it does not pretend.
package session

import (
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of one project's terminal runtime.
//
// The set is deliberately small. Agent-level states - waiting for input,
// finished, asking permission - are properties of the program running inside
// the session, not of the session, and belong to the Claude Hooks phase.
type State string

// Runtime states.
const (
	// StateStopped means no work is running for this project. The session may
	// or may not still exist; SessionAlive on the Runtime says which.
	StateStopped State = "STOPPED"

	// StateStarting means a session is being created or re-attached.
	StateStarting State = "STARTING"

	// StateRunning means a session exists and AgentMux is reading its output.
	StateRunning State = "RUNNING"

	// StateStopping means the runtime is being interrupted or torn down.
	StateStopping State = "STOPPING"

	// StateError means the runtime was wanted but could not be brought up.
	StateError State = "ERROR"

	// StateOrphan means a session exists that no registered project claims.
	// AgentMux reports it and does not touch it.
	StateOrphan State = "ORPHAN"
)

// Settled reports whether a state is a resting point rather than a transition.
// Only settled states are written to the database, because a transition
// recorded on disk would be a lie the moment the process stopped.
func (s State) Settled() bool {
	return s == StateStopped || s == StateRunning || s == StateError
}

// Runtime is the state of one project's terminal runtime, as reported to a
// client.
//
// It mixes two sources on purpose: the durable record AgentMux wrote down, and
// what the backend says right now. The record is authoritative about intent -
// the size the user chose, whether they stopped it - and the backend is
// authoritative about liveness.
type Runtime struct {
	ProjectID string `json:"projectId"`
	Backend   string `json:"backend"`
	Session   string `json:"session"`

	State State `json:"state"`

	// SessionAlive reports whether the session exists in the backend right
	// now, which is not the same as State: a stopped runtime keeps its session
	// so its scrollback survives.
	SessionAlive bool `json:"sessionAlive"`

	// Cols and Rows are the canonical geometry. They are AgentMux's decision,
	// not a side effect of whichever client happened to attach last.
	Cols int `json:"cols"`
	Rows int `json:"rows"`

	// Sequence is the highest output sequence number produced so far. It lets
	// a reconnecting client ask for what it missed.
	Sequence uint64 `json:"sequence"`

	// StartedAt is when the session was created. Zero when there is none.
	StartedAt time.Time `json:"startedAt,omitempty"`

	UpdatedAt  time.Time  `json:"updatedAt"`
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`

	// Message explains a state that needs explaining, for example why a start
	// failed. Empty when there is nothing to say.
	Message string `json:"message,omitempty"`
}

// Chunk is one piece of terminal output.
//
// The sequence number is per project and strictly increasing, which is what
// makes reconnect, history, and multi-client delivery possible later without
// changing this shape.
type Chunk struct {
	ProjectID string    `json:"projectId"`
	Sequence  uint64    `json:"sequence"`
	Data      []byte    `json:"data"`
	Timestamp time.Time `json:"timestamp"`
}

// SessionSpec describes a session to create.
type SessionSpec struct {
	// Name is the session identity. It is derived from the project id and
	// never from the project name, so renaming a project cannot orphan a
	// running session.
	Name string

	// Dir is the working directory the session starts in. It must be the
	// project's runtime path: starting a session at a Projects Root or at the
	// server's own directory would put the agent in the wrong repository.
	Dir string

	// Command is what the session runs. Empty means the configured shell.
	Command []string

	// Cols and Rows are the initial canonical size.
	Cols int
	Rows int
}

// Session is what a backend knows about one session.
type Session struct {
	Name string `json:"name"`

	// Dir is the directory the session's process is actually in, read back
	// from the runtime rather than assumed from the spec.
	Dir string `json:"dir,omitempty"`

	Cols int `json:"cols"`
	Rows int `json:"rows"`

	Created time.Time `json:"createdAt"`
}

// Record is the part of a runtime that must survive the server process.
//
// Everything else - whether the session is alive right now, how much output
// has flowed, when it started - is read back from the runtime, which is the
// only authority on it. Copying those here would create a second source of
// truth to drift out of step with the first.
type Record struct {
	ProjectID string
	Backend   string
	Session   string
	State     State

	Cols int
	Rows int

	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSeenAt *time.Time
}

// Error codes produced by the runtime.
//
// These are part of the API contract exactly like the project codes: the HTTP
// layer maps them to statuses and the frontend switches on them.
const (
	// CodeUnavailable means the terminal runtime cannot execute in this
	// environment at all.
	CodeUnavailable = "runtime_unavailable"

	// CodeBackendUnavailable means the runtime exists but the backend cannot
	// use it, for example tmux is not installed.
	CodeBackendUnavailable = "runtime_backend_unavailable"

	// CodeNotFound means no runtime is recorded for the project.
	CodeNotFound = "runtime_not_found"

	// CodeAlreadyRunning means the runtime is already up.
	CodeAlreadyRunning = "runtime_already_running"

	// CodeNotRunning means the operation needs a live session and there is
	// none.
	CodeNotRunning = "runtime_not_running"

	// CodeStartFailed means the session could not be created.
	CodeStartFailed = "runtime_start_failed"

	// CodeStopFailed means the running work could not be interrupted.
	CodeStopFailed = "runtime_stop_failed"

	// CodeDestroyFailed means the session could not be removed.
	CodeDestroyFailed = "runtime_destroy_failed"

	// CodeInputFailed means terminal input could not be delivered.
	CodeInputFailed = "runtime_input_failed"

	// CodeResizeFailed means the canonical size could not be applied.
	CodeResizeFailed = "runtime_resize_failed"

	// CodeInvalidSize means a requested terminal size is not usable.
	CodeInvalidSize = "invalid_terminal_size"

	// CodeInvalidInput means a malformed request reached the runtime.
	CodeInvalidInput = "invalid_input"

	// CodeBackendFailure is an unexpected failure inside the backend.
	CodeBackendFailure = "runtime_backend_failure"

	// CodeStorageFailure is a failure writing runtime metadata.
	CodeStorageFailure = "storage_failure"

	// CodeProjectNotFound means the project the runtime was requested for does
	// not exist.
	CodeProjectNotFound = "project_not_found"
)

// Error is a runtime failure carrying a stable code.
//
// It mirrors project.Error deliberately: a client switches on one vocabulary
// across the whole API, and both types carry the same three fields.
type Error struct {
	Code    string
	Message string
	Details map[string]any
	Err     error
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

// withDetail attaches structured context.
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

// IsCode reports whether err is a runtime error with the given code.
func IsCode(err error, code string) bool { return CodeOf(err) == code }
