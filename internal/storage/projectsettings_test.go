package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests are about the table and the statements, not about the service.
//
// The service decides what a mode means and refuses values that are not modes;
// the store's job is to write one down and give it back, and to keep the shape
// the migration promised. The claim worth pinning here is that shape: four
// columns, and no fifth for a mode that is *in force* - which is the whole of
// the difference between this table and a runtime switch.
//
// Every test that writes a row creates its project first, because the table is
// foreign-keyed to `projects` and a setting for a project that does not exist is
// the one thing it must refuse. That a test has to do this at all is the
// constraint working.

// settingsAt builds a stored-shape settings row.
func settingsAt(id string, mode claude.PermissionMode, hour int) project.Settings {
	return project.Settings{
		ProjectID:      id,
		PermissionMode: mode,
		CreatedAt:      at(2026, time.September, 29, hour),
		UpdatedAt:      at(2026, time.September, 29, hour),
	}
}

// TestTheProjectSettingsTableHasExactlyFourColumns is the schema half of the// promise that this records a preference and not a state.
//
// It is asserted over PRAGMA rather than over the Go struct, because the struct
// is what a later change would edit and the table is what a later change would
// also have to edit. A `current_mode` column added to both would fail this test,
// which is the point: the reason there is no runtime switching is that nothing
// can read a running agent's mode, and the way to keep that honest is a table
// with nowhere to write a guess about one.
func TestTheProjectSettingsTableHasExactlyFourColumns(t *testing.T) {
	store := newTestStore(t)

	rows, err := store.db.Query(`PRAGMA table_info(project_settings)`)
	if err != nil {
		t.Fatalf("could not read the table's columns: %v", err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			t.Fatalf("could not scan a column: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the columns failed: %v", err)
	}

	want := "project_id,permission_mode,created_at,updated_at"
	if got := strings.Join(columns, ","); got != want {
		t.Errorf("project_settings columns = %s, want %s", got, want)
	}
}

// TestStoringAndReadingBackASetting round-trips each mode the build offers.
//
// All three rather than one, because the value is a string that crosses a
// driver boundary in both directions and a scan that read only the first mode
// would pass for the wrong reason.
func TestStoringAndReadingBackASetting(t *testing.T) {
	for _, mode := range claude.PermissionModes() {
		t.Run(string(mode), func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()

			const id = "p_0123456789abcdef0123"
			if err := store.Projects().Create(ctx, testProject(id, "App", `D:\AI\Projects\App`)); err != nil {
				t.Fatalf("creating the project failed: %v", err)
			}

			stored := settingsAt(id, mode, 10)
			if err := store.ProjectSettings().SaveSettings(ctx, stored); err != nil {
				t.Fatalf("SaveSettings returned an error: %v", err)
			}

			got, err := store.ProjectSettings().GetSettings(ctx, id)
			if err != nil {
				t.Fatalf("GetSettings returned an error: %v", err)
			}
			if got.PermissionMode != mode {
				t.Errorf("PermissionMode = %q, want %q", got.PermissionMode, mode)
			}
			if !got.CreatedAt.Equal(stored.CreatedAt) || !got.UpdatedAt.Equal(stored.UpdatedAt) {
				t.Errorf("timestamps = %s/%s, want %s/%s",
					got.CreatedAt, got.UpdatedAt, stored.CreatedAt, stored.UpdatedAt)
			}
		})
	}
}

// TestAProjectNobodyConfiguredHasNoRow is the default's storage half.
//
// The absence is reported rather than filled in, and - the half that is easy to
// get wrong - asking does not create anything. A read that wrote a default row
// would turn "nobody has configured this" into "somebody configured this to the
// default", and those are two different facts about a project.
func TestAProjectNobodyConfiguredHasNoRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const id = "p_0123456789abcdef0123"
	if err := store.Projects().Create(ctx, testProject(id, "App", `D:\AI\Projects\App`)); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}

	if _, err := store.ProjectSettings().GetSettings(ctx, id); !errors.Is(err, project.ErrSettingsNotFound) {
		t.Errorf("GetSettings error = %v, want ErrSettingsNotFound", err)
	}

	if got := countSettings(t, store, id); got != 0 {
		t.Errorf("reading left %d row(s) behind, want none", got)
	}
}

// TestSavingAgainReplacesTheModeAndKeepsWhenItWasFirstConfigured pins both
// halves of the upsert.
//
// The mode is the point of a save, so it moves; `created_at` is when the project
// was first configured, which reconfiguring it does not change. A store that
// replaced the whole row would make the second fact the time of the last edit
// instead, and nothing would read differently until somebody wanted to know when
// a project was set up.
func TestSavingAgainReplacesTheModeAndKeepsWhenItWasFirstConfigured(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const id = "p_0123456789abcdef0123"
	if err := store.Projects().Create(ctx, testProject(id, "App", `D:\AI\Projects\App`)); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}

	first := settingsAt(id, claude.PermissionManual, 10)
	if err := store.ProjectSettings().SaveSettings(ctx, first); err != nil {
		t.Fatalf("the first SaveSettings returned an error: %v", err)
	}

	second := settingsAt(id, claude.PermissionAcceptEdits, 11)
	if err := store.ProjectSettings().SaveSettings(ctx, second); err != nil {
		t.Fatalf("the second SaveSettings returned an error: %v", err)
	}

	got, err := store.ProjectSettings().GetSettings(ctx, id)
	if err != nil {
		t.Fatalf("GetSettings returned an error: %v", err)
	}
	if got.PermissionMode != claude.PermissionAcceptEdits {
		t.Errorf("PermissionMode = %q, want %q", got.PermissionMode, claude.PermissionAcceptEdits)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt = %s, want it unchanged at %s", got.CreatedAt, first.CreatedAt)
	}
	if !got.UpdatedAt.Equal(second.UpdatedAt) {
		t.Errorf("UpdatedAt = %s, want %s", got.UpdatedAt, second.UpdatedAt)
	}
	if got := countSettings(t, store, id); got != 1 {
		t.Errorf("the project has %d settings rows, want exactly 1", got)
	}
}

// TestReadingTheSettingsOfSeveralProjects is the console's read.
//
// A project with no row is absent from the result rather than present with a
// zero value: "this project is configured with the empty string" is not a fact
// this build has, and a caller that received one would have to know to discard
// it. An identifier that names no project at all is absent for the same reason,
// and it costs nothing because nothing draws a card for it.
func TestReadingTheSettingsOfSeveralProjects(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	configured := "p_0123456789abcdef0123"
	unconfigured := "p_0123456789abcdef0124"
	// A host path is unique across projects, so each one needs its own.
	if err := store.Projects().Create(ctx, testProject(configured, "App", `D:\AI\Projects\App`)); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}
	if err := store.Projects().Create(ctx, testProject(unconfigured, "Site", `D:\AI\Projects\Site`)); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}
	if err := store.ProjectSettings().SaveSettings(ctx,
		settingsAt(configured, claude.PermissionBypass, 10)); err != nil {
		t.Fatalf("SaveSettings returned an error: %v", err)
	}

	settings, err := store.ProjectSettings().SettingsByProjects(ctx,
		[]string{configured, unconfigured, "p_ffffffffffffffffffff"})
	if err != nil {
		t.Fatalf("SettingsByProjects returned an error: %v", err)
	}
	if len(settings) != 1 {
		t.Fatalf("SettingsByProjects returned %d row(s), want 1: %v", len(settings), settings)
	}
	if settings[0].ProjectID != configured {
		t.Errorf("the row is for %q, want %q", settings[0].ProjectID, configured)
	}
	if settings[0].PermissionMode != claude.PermissionBypass {
		t.Errorf("PermissionMode = %q, want %q", settings[0].PermissionMode, claude.PermissionBypass)
	}

	// No identifiers is no query, and an empty result rather than an error: a
	// listing that has nothing to ask for has not failed.
	empty, err := store.ProjectSettings().SettingsByProjects(ctx, nil)
	if err != nil {
		t.Fatalf("SettingsByProjects(nil) returned an error: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("SettingsByProjects(nil) returned %d row(s), want none", len(empty))
	}
}

// TestASettingBelongsToAProject records the foreign key, from the side that
// refuses.
//
// A setting for a project that does not exist is unreachable through every
// endpoint this API has, so the table does not accept one. It is asserted here
// rather than left to the migration's comment because it is the constraint that
// makes a delete tidy, and a constraint nobody exercises is a comment with SQL
// around it.
func TestASettingBelongsToAProject(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.ProjectSettings().SaveSettings(ctx,
		settingsAt("p_ffffffffffffffffffff", claude.PermissionManual, 10))
	if err == nil {
		t.Fatal("a setting was stored for a project that does not exist")
	}
	var failure *project.Error
	if !errors.As(err, &failure) {
		t.Fatalf("the error is %v, want a *project.Error", err)
	}
	if failure.Code != project.CodeStorageFailure {
		t.Errorf("the error carries %q, want %q", failure.Code, project.CodeStorageFailure)
	}
}

// TestDeletingAProjectRemovesItsSettings is the other half of the same
// constraint. `project_runtime`, `agent_events` and `tasks` all cascade the same
// way; what is left behind when a project goes is nothing that named it.
func TestDeletingAProjectRemovesItsSettings(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const id = "p_0123456789abcdef0123"
	if err := store.Projects().Create(ctx, testProject(id, "App", `D:\AI\Projects\App`)); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}
	if err := store.ProjectSettings().SaveSettings(ctx,
		settingsAt(id, claude.PermissionAcceptEdits, 10)); err != nil {
		t.Fatalf("SaveSettings returned an error: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
		t.Fatalf("deleting the project failed: %v", err)
	}

	if got := countSettings(t, store, id); got != 0 {
		t.Errorf("%d settings row(s) survived the deletion of their project", got)
	}
}

// countSettings is how many settings rows a project has.
func countSettings(t *testing.T, store *Store, projectID string) int {
	t.Helper()

	var count int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM project_settings WHERE project_id = ?`, projectID).Scan(&count); err != nil {
		t.Fatalf("counting the settings rows failed: %v", err)
	}
	return count
}
