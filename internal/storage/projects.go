package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// projectColumns is the column list every project read uses. Listing columns
// explicitly means adding one to the table cannot silently change the meaning
// of an existing read.
const projectColumns = `id, name, host_path, runtime_path, collection_path,
	pinned_slot, archived, created_at, updated_at, last_opened_at`

// ProjectStore is the SQLite-backed project repository.
type ProjectStore struct {
	db *sql.DB
}

// ProjectStore must satisfy the contract the project service depends on.
var _ project.Repository = (*ProjectStore)(nil)

// Create stores a new project.
func (r *ProjectStore) Create(ctx context.Context, p *project.Project) error {
	if p == nil {
		return errors.New("storage: project must not be nil")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO projects (
			id, name, host_path, runtime_path, collection_path,
			pinned_slot, archived, created_at, updated_at, last_opened_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID,
		p.Name,
		p.HostPath,
		p.RuntimePath,
		p.CollectionPath,
		nullableInt(p.PinnedSlot),
		boolToInt(p.Archived),
		formatTime(p.CreatedAt),
		formatTime(p.UpdatedAt),
		nullableTime(p.LastOpenedAt),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return &project.Error{
				Code:    project.CodeAlreadyRegistered,
				Message: fmt.Sprintf("%s is already registered", p.HostPath),
				Details: map[string]any{"hostPath": p.HostPath},
				Err:     err,
			}
		}
		return &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not store the project",
			Err:     err,
		}
	}
	return nil
}

// Update stores changes to an existing project.
func (r *ProjectStore) Update(ctx context.Context, p *project.Project) error {
	if p == nil {
		return errors.New("storage: project must not be nil")
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE projects SET
			name            = ?,
			host_path       = ?,
			runtime_path    = ?,
			collection_path = ?,
			pinned_slot     = ?,
			archived        = ?,
			updated_at      = ?,
			last_opened_at  = ?
		WHERE id = ?`,
		p.Name,
		p.HostPath,
		p.RuntimePath,
		p.CollectionPath,
		nullableInt(p.PinnedSlot),
		boolToInt(p.Archived),
		formatTime(p.UpdatedAt),
		nullableTime(p.LastOpenedAt),
		p.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return &project.Error{
				Code:    project.CodeAlreadyRegistered,
				Message: fmt.Sprintf("%s is already registered", p.HostPath),
				Details: map[string]any{"hostPath": p.HostPath},
				Err:     err,
			}
		}
		return &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not update the project",
			Err:     err,
		}
	}
	return affectedOrNotFound(result, p.ID)
}

// GetByID returns a project by identifier.
func (r *ProjectStore) GetByID(ctx context.Context, id string) (*project.Project, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if err != nil {
		return nil, wrapScanError(err, "id "+id)
	}
	return p, nil
}

// GetByHostPath returns a project by host path.
//
// The comparison is COLLATE NOCASE so that on a case-insensitive filesystem a
// path that differs only in letter casing resolves to the existing project
// rather than looking like a new one.
func (r *ProjectStore) GetByHostPath(ctx context.Context, hostPath string) (*project.Project, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE host_path = ? COLLATE NOCASE`, hostPath)
	p, err := scanProject(row)
	if err != nil {
		return nil, wrapScanError(err, "host path "+hostPath)
	}
	return p, nil
}

// List returns projects matching the filter.
//
// Ordering is stable - name, then identifier - so that a client refreshing a
// list does not see panels reshuffle. Slot ordering arrives with the workspace
// grid in Phase 5.
func (r *ProjectStore) List(ctx context.Context, filter project.ListFilter) ([]*project.Project, error) {
	query := `SELECT ` + projectColumns + ` FROM projects`
	if !filter.IncludeArchived {
		query += ` WHERE archived = 0`
	}
	query += ` ORDER BY name COLLATE NOCASE ASC, id ASC`

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not list projects",
			Err:     err,
		}
	}
	defer rows.Close()

	projects := make([]*project.Project, 0, 16)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, wrapScanError(err, "project row")
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not read projects",
			Err:     err,
		}
	}
	return projects, nil
}

// scanRow is the subset of *sql.Row and *sql.Rows that scanning needs.
type scanRow interface {
	Scan(dest ...any) error
}

func scanProject(row scanRow) (*project.Project, error) {
	var (
		p          project.Project
		pinnedSlot sql.NullInt64
		archived   int
		createdAt  string
		updatedAt  string
		lastOpened sql.NullString
	)
	if err := row.Scan(
		&p.ID,
		&p.Name,
		&p.HostPath,
		&p.RuntimePath,
		&p.CollectionPath,
		&pinnedSlot,
		&archived,
		&createdAt,
		&updatedAt,
		&lastOpened,
	); err != nil {
		return nil, err
	}

	p.Archived = archived != 0
	if pinnedSlot.Valid {
		slot := int(pinnedSlot.Int64)
		p.PinnedSlot = &slot
	}

	var err error
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	if lastOpened.Valid && lastOpened.String != "" {
		at, err := parseTime(lastOpened.String)
		if err != nil {
			return nil, err
		}
		p.LastOpenedAt = &at
	}
	return &p, nil
}

// wrapScanError converts a scan failure into a project-model error. A missing
// row becomes project.ErrNotFound, which is what the service layer expects.
func wrapScanError(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return project.ErrNotFound
	}
	return &project.Error{
		Code:    project.CodeStorageFailure,
		Message: "could not read " + what,
		Err:     err,
	}
}

func affectedOrNotFound(result sql.Result, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not confirm the update",
			Err:     err,
		}
	}
	if affected == 0 {
		return project.ErrNotFound
	}
	return nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
