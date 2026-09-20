// Package project implements the AgentMux project model.
//
// The model has three levels, and conflating them is the mistake this package
// exists to prevent:
//
//	Projects Root  D:\AI\Projects                            broad, user-controlled
//	Collection     D:\AI\Projects\2026 AgentMux              organisational folder
//	Project        D:\AI\Projects\2026 AgentMux\AgentMux     registered unit
//
// A collection is not a project. A first-level directory under a root is not
// automatically a project. Only explicit registration creates one, and after
// registration the stored host path is authoritative - AgentMux never
// re-derives a project's location from its folder name.
package project

import (
	"fmt"
	"strings"
	"time"
)

// Status values reported for a project.
//
// A project's status is never stored. It is computed from what the terminal
// runtime knows at the moment of the read, so a project cannot be reported as
// running by a server that has lost track of its session - which is exactly
// what a stored status would do after a crash.
const (
	StatusStopped      = "stopped"
	StatusStarting     = "starting"
	StatusRunning      = "running"
	StatusStopping     = "stopping"
	StatusReconnecting = "reconnecting"
	StatusError        = "error"
	StatusOrphan       = "orphan"
)

// RuntimeState reports what the terminal runtime knows about a project.
//
// The project model needs one thing from the runtime and nothing else, so this
// is one method wide. Declaring it here rather than importing the runtime
// keeps the dependency pointing one way: the runtime knows about projects,
// projects know only that somebody can answer this question.
type RuntimeState interface {
	// ProjectStatus returns the project status for a project, or "" when the
	// runtime has nothing to say about it - which is the ordinary case for a
	// project whose terminal has never been started.
	ProjectStatus(projectID string) string
}

// Project is a registered development directory managed by AgentMux.
//
// Runtime metadata (canonical PTY size, output sequence, controller lease)
// deliberately does not appear here. It belongs to the session layer and is
// reached through RuntimeState.
type Project struct {
	// ID is the stable identity. It never changes, including on rename.
	ID string `json:"id"`

	// Name is the display name. It may change freely.
	Name string `json:"name"`

	// HostPath is the authoritative project location in host form.
	HostPath string `json:"hostPath"`

	// RuntimePath is the same location as the runtime sees it, resolved
	// through the HostAdapter at registration time and stored thereafter.
	RuntimePath string `json:"runtimePath"`

	// CollectionPath is the collection/group folder directly containing this
	// project, or "" when it is a direct child of its Projects Root.
	CollectionPath string `json:"collectionPath"`

	// Status is derived, never stored. It comes from the runtime.
	Status string `json:"status"`

	// PinnedSlot reserves a workspace panel position. Nil means unpinned.
	PinnedSlot *int `json:"pinnedSlot"`

	// Archived projects stay registered but are hidden from the workspace.
	Archived bool `json:"archived"`

	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	LastOpenedAt *time.Time `json:"lastOpenedAt"`
}

// SessionPrefix is the namespace every AgentMux runtime session name carries.
//
// It exists so that AgentMux can tell its own sessions apart from any others
// sharing a tmux server: a name outside this namespace is not AgentMux's to
// report, interrupt, or destroy.
const SessionPrefix = "amx-"

// SessionNameFor is the one place a session name is spelled.
//
// Everything that creates, looks up, or reclaims a session calls this rather
// than building the string itself, because two spellings of the same name are
// two different sessions, and the failure that produces - a project whose
// terminal is running but unreachable - is invisible until it matters.
func SessionNameFor(projectID string) string {
	return SessionPrefix + projectID
}

// SessionName is the runtime session identifier for this project.
//
// The name is derived from the stable ID, never from the display name, so
// that renaming a project cannot orphan a running session.
func (p *Project) SessionName() string {
	return SessionNameFor(p.ID)
}

// ValidRuntimeID reports whether id is a runtime identifier this server could
// have produced.
//
// A runtime id is a session name, and a session name is the prefix followed by
// a project id - so the body is checked against ValidID rather than the whole
// string being checked for the prefix. The difference matters to a caller
// storing the value: "amx-" is not a runtime, and neither is "amx-1", and a
// check that only asked about the prefix would accept both.
//
// It answers a question about the name and not about the world. Nothing here
// says a runtime with this id is running, or ever ran: a runtime is destroyed
// and forgotten while the record of what used it remains, and this is the
// shape that record has to have.
func ValidRuntimeID(id string) bool {
	body, ok := strings.CutPrefix(id, SessionPrefix)
	return ok && ValidID(body)
}

// String renders a project for logs without exposing anything sensitive.
func (p *Project) String() string {
	return fmt.Sprintf("project(%s %q host=%s)", p.ID, p.Name, p.HostPath)
}

// Clone returns a deep copy, so that callers cannot mutate stored state by
// holding on to a pointer.
func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	cp := *p
	if p.PinnedSlot != nil {
		slot := *p.PinnedSlot
		cp.PinnedSlot = &slot
	}
	if p.LastOpenedAt != nil {
		at := *p.LastOpenedAt
		cp.LastOpenedAt = &at
	}
	return &cp
}
