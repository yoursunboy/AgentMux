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
	"time"
)

// Status values reported for a project.
//
// Phase 1 has no terminal runtime, so every project is StatusStopped. The
// runtime-backed values are declared now because they are part of the API
// contract the frontend renders; they are never produced yet.
const (
	StatusStopped      = "stopped"
	StatusRunning      = "running"
	StatusReconnecting = "reconnecting"
)

// Project is a registered development directory managed by AgentMux.
//
// Runtime metadata (canonical PTY size, output sequence, controller lease)
// deliberately does not appear here. It belongs to the session layer and
// arrives in Phase 2.
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

	// Status is derived, not stored. Phase 1 always reports StatusStopped.
	Status string `json:"status"`

	// PinnedSlot reserves a workspace panel position. Nil means unpinned.
	PinnedSlot *int `json:"pinnedSlot"`

	// Archived projects stay registered but are hidden from the workspace.
	Archived bool `json:"archived"`

	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	LastOpenedAt *time.Time `json:"lastOpenedAt"`
}

// SessionName is the runtime session identifier for this project.
//
// The name is derived from the stable ID, never from the display name, so
// that renaming a project cannot orphan a running session. Phase 2 creates
// sessions with this name; Phase 1 only fixes the rule.
func (p *Project) SessionName() string {
	return "amx-" + p.ID
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
