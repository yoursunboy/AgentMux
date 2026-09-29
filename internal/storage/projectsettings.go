package storage

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/project"
)

// projectSettingsColumns lists the columns every read names explicitly.
//
// Listing columns explicitly means adding one to the table cannot silently
// change the meaning of an existing read.
const projectSettingsColumns = `project_id, permission_mode, created_at, updated_at`

// ProjectSettingsStore stores each project's launch configuration.
type ProjectSettingsStore struct {
	db *sql.DB
}

var _ project.SettingsRepository = (*ProjectSettingsStore)(nil)

// GetSettings returns a project's settings, or project.ErrSettingsNotFound.
//
// The absence is not translated here. A project nobody has configured is an
// ordinary state rather than a failure, and only the service knows that - so
// storage reports what it found and the decision is made where it belongs.
func (r *ProjectSettingsStore) GetSettings(ctx context.Context, projectID string) (project.Settings, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+projectSettingsColumns+` FROM project_settings WHERE project_id = ?`, projectID)
	s, err := scanProjectSettings(row)
	if errors.Is(err, sql.ErrNoRows) {
		return project.Settings{}, project.ErrSettingsNotFound
	}
	if err != nil {
		return project.Settings{}, &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not read the project's settings",
			Err:     err,
		}
	}
	return s, nil
}

// SaveSettings writes a project's settings, inserting or replacing.
//
// It is an upsert because a caller that has decided what a project should be
// configured with should not also have to know whether this is the first time
// it has been configured. There is exactly one row per project, so there is
// nothing an insert-only path would be protecting.
//
// It does not move created_at when it replaces a row: when a project was first
// configured is not something that configuring it again changes.
func (r *ProjectSettingsStore) SaveSettings(ctx context.Context, s project.Settings) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_settings (project_id, permission_mode, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			permission_mode = excluded.permission_mode,
			updated_at      = excluded.updated_at`,
		s.ProjectID, string(s.PermissionMode), formatTime(s.CreatedAt), formatTime(s.UpdatedAt))
	if err != nil {
		return &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not store the project's settings",
			Err:     err,
		}
	}
	return nil
}

// SettingsByProjects returns the settings of each of the given projects.
//
// # Why this is one query
//
// The console draws a card for every project at once and each card carries a
// mode, so reading them a project at a time would be a query per project - the
// N+1 docs/CONTROLLER_API.md §13 is explicitly not allowed to commit. The `IN`
// narrows to the projects that were asked for, and the primary key makes that
// an index lookup rather than a scan.
//
// A project with no settings row is simply absent from the result, not an
// error. Which of these absences means "the default" is the service's answer to
// give, so storage reports the rows it found and nothing more.
func (r *ProjectSettingsStore) SettingsByProjects(ctx context.Context, projectIDs []string) ([]project.Settings, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}

	statement := `SELECT ` + projectSettingsColumns + ` FROM project_settings
		WHERE project_id IN (` + placeholders(len(projectIDs)) + `)`

	rows, err := r.db.QueryContext(ctx, statement, argsOf(projectIDs)...)
	if err != nil {
		return nil, &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not list settings for several projects",
			Err:     err,
		}
	}
	defer rows.Close()

	out := make([]project.Settings, 0, len(projectIDs))
	for rows.Next() {
		s, err := scanProjectSettings(rows)
		if err != nil {
			return nil, &project.Error{
				Code:    project.CodeStorageFailure,
				Message: "could not read a project's settings",
				Err:     err,
			}
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, &project.Error{
			Code:    project.CodeStorageFailure,
			Message: "could not list settings for several projects",
			Err:     err,
		}
	}
	return out, nil
}

// scanProjectSettings reads one row.
//
// The mode is scanned into a string and converted, rather than scanned straight
// into the named type. Scanning into a type the driver has never heard of works
// only by reflection over a string kind, and that is a property of the driver
// rather than something this code should depend on.
func scanProjectSettings(row rowScanner) (project.Settings, error) {
	var (
		s       project.Settings
		mode    string
		created string
		updated string
	)
	if err := row.Scan(&s.ProjectID, &mode, &created, &updated); err != nil {
		return project.Settings{}, err
	}
	s.PermissionMode = claude.PermissionMode(mode)

	var err error
	if s.CreatedAt, err = parseTime(created); err != nil {
		return project.Settings{}, err
	}
	if s.UpdatedAt, err = parseTime(updated); err != nil {
		return project.Settings{}, err
	}
	return s, nil
}
