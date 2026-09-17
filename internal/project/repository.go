package project

import (
	"context"
	"errors"
)

// ErrNotFound is returned by a Repository when no project matches the lookup.
//
// Storage implementations return this rather than a storage-specific error so
// that the service layer can decide what a missing record means without
// knowing which database is behind it.
var ErrNotFound = errors.New("project not found")

// ListFilter narrows a project listing.
type ListFilter struct {
	// IncludeArchived includes archived projects. The workspace excludes them
	// by default; project management includes them.
	IncludeArchived bool
}

// Repository is the persistence contract the project service depends on.
//
// It is declared here, in the package that consumes it, rather than in the
// storage package. The service states what it needs; storage satisfies it.
// That keeps the dependency pointing one way: storage imports project, never
// the reverse.
type Repository interface {
	// Create stores a new project. It reports an error whose Code is
	// CodeAlreadyRegistered when the host path is already taken.
	Create(ctx context.Context, p *Project) error

	// Update stores changes to an existing project.
	Update(ctx context.Context, p *Project) error

	// GetByID returns a project by identifier, or ErrNotFound.
	GetByID(ctx context.Context, id string) (*Project, error)

	// GetByHostPath returns a project by host path, comparing the way the
	// host filesystem compares paths, or ErrNotFound.
	GetByHostPath(ctx context.Context, hostPath string) (*Project, error)

	// List returns projects matching the filter.
	List(ctx context.Context, filter ListFilter) ([]*Project, error)
}
