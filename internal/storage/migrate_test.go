package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/task"
	"github.com/kutonlagos/agentmux/migrations"
)

// latestVersion is the highest embedded migration version, read from the
// migrations themselves rather than written down here. A hard-coded number
// turns every later phase into a test edit, which trains a reader to update
// the expectation without reading it.
func latestVersion(t *testing.T) int {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All returned an error: %v", err)
	}
	return all[len(all)-1].Version
}

// TestMigrateAppliesTheInitialSchema pins the first migration's identity. The
// version is part of the on-disk contract: changing it would make every
// existing database re-run the schema.
func TestMigrateAppliesTheInitialSchema(t *testing.T) {
	store := newTestStore(t)

	result, err := store.Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("a second Migrate applied %v, want nothing", result.Applied)
	}
	if want := latestVersion(t); result.Version != want {
		t.Errorf("Version = %d, want %d", result.Version, want)
	}

	// The migrations ran inside newTestStore; check each is recorded once.
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All returned an error: %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("could not count applied migrations: %v", err)
	}
	if count != len(all) {
		t.Errorf("schema_migrations holds %d rows, want %d", count, len(all))
	}
	// Version 1 is the initial schema, and it must stay version 1: changing it
	// would make every existing database try to create its tables again.
	var initial int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&initial); err != nil {
		t.Fatalf("could not check the initial schema's version: %v", err)
	}
	if initial != 1 {
		t.Error("the initial schema is not recorded at version 1")
	}
}

// TestMigrateIsIdempotent is the property that makes it safe to call on every
// start.
func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentmux.db")

	store, err := Open(context.Background(), OpenOptions{Path: path})
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	defer store.Close()

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All returned an error: %v", err)
	}

	first, err := store.Migrate(context.Background())
	if err != nil {
		t.Fatalf("the first Migrate returned an error: %v", err)
	}
	if len(first.Applied) != len(all) {
		t.Fatalf("the first Migrate applied %v, want all %d migrations", first.Applied, len(all))
	}
	if !strings.HasSuffix(first.Applied[0], "initial_schema") {
		t.Errorf("the first migration is %q, want the initial schema", first.Applied[0])
	}

	second, err := store.Migrate(context.Background())
	if err != nil {
		t.Fatalf("the second Migrate returned an error: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("the second Migrate applied %v, want nothing", second.Applied)
	}
	if second.Version != first.Version {
		t.Errorf("Version changed from %d to %d across a no-op migration", first.Version, second.Version)
	}
}

func TestSchemaVersionBeforeAndAfter(t *testing.T) {
	store, err := Open(context.Background(), OpenOptions{Path: filepath.Join(t.TempDir(), "a.db")})
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion returned an error: %v", err)
	}
	if version != 0 {
		t.Errorf("SchemaVersion() = %d before migrating, want 0", version)
	}

	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}
	want := latestVersion(t)
	if version, err = store.SchemaVersion(context.Background()); err != nil || version != want {
		t.Errorf("SchemaVersion() = %d, %v after migrating, want %d", version, err, want)
	}
}

// TestMigrateCreatesTheExpectedTables records the shape of the schema.
//
// The Phase 1 assertion that project_runtime must not exist yet is gone
// because the table now has a real writer. The reason it was withheld is worth
// keeping in mind for the next table: a schema added early is read and written
// with placeholder values long before the layer that gives it meaning exists,
// and those placeholders outlive the phase that excused them.
func TestMigrateCreatesTheExpectedTables(t *testing.T) {
	store := newTestStore(t)

	tables := tableNames(t, store)

	want := []string{
		"agent_actions", "agent_attention", "agent_events", "agent_sessions",
		"agent_states", "project_runtime", "projects", "schema_migrations",
		"settings", "tasks", "usage_events",
	}
	if strings.Join(tables, ",") != strings.Join(want, ",") {
		t.Errorf("tables = %v, want %v", tables, want)
	}
}

// tableNames lists the tables in the database, without SQLite's own.
func tableNames(t *testing.T, store *Store) []string {
	t.Helper()

	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("could not list tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("could not scan a table name: %v", err)
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the table list failed: %v", err)
	}
	return tables
}

// TestHostPathIndexIsUniqueAndCaseInsensitive proves the schema enforces the
// rule the repository relies on, rather than the repository alone.
func TestHostPathIndexIsUniqueAndCaseInsensitive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Projects().Create(ctx, testProject("p_1", "App", `C:\AI\Projects\App`)); err != nil {
		t.Fatalf("the first Create returned an error: %v", err)
	}
	err := store.Projects().Create(ctx, testProject("p_2", "Other", `c:\ai\projects\app`))
	if err == nil {
		t.Fatal("storing a differently-cased host path succeeded, want a rejection")
	}
	if !project.IsCode(err, project.CodeAlreadyRegistered) {
		t.Errorf("the second Create failed with %v, want %q", err, project.CodeAlreadyRegistered)
	}
}

// TestMigrationFilesAreWellFormed checks the embedded set directly, so a
// malformed file is caught without opening a database.
func TestMigrationFilesAreWellFormed(t *testing.T) {
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All returned an error: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("migrations.All returned nothing")
	}

	seen := make(map[int]bool, len(all))
	previous := 0
	for _, migration := range all {
		if migration.Version <= previous {
			t.Errorf("migration %s is not in ascending order", migration)
		}
		previous = migration.Version
		if seen[migration.Version] {
			t.Errorf("version %d appears twice", migration.Version)
		}
		seen[migration.Version] = true

		if migration.Name == "" {
			t.Errorf("migration %s has no name", migration.Source)
		}
		if !strings.HasSuffix(migration.Source, ".sql") {
			t.Errorf("migration %s is not a .sql file", migration.Source)
		}
		if strings.TrimSpace(migration.SQL) == "" {
			t.Errorf("migration %s is empty", migration.Source)
		}
		if got, want := migration.String(), strings.TrimSuffix(migration.Source, ".sql"); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

// TestMigrateUpgradesAFullDatabaseWithoutDisturbingIt covers the projections.
//
// The earlier upgrade test brings a database to Phase 7.1, when only projects,
// runtime records and events existed. This one brings a database to the phase
// immediately before the projection - every table this build already had, each
// with a row in it - and then migrates. What it checks is not just that the new
// table appears, but that nothing else moved: an additive migration that
// rewrote a neighbour would be a migration that cost somebody their history.
func TestMigrateUpgradesAFullDatabaseWithoutDisturbingIt(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, OpenOptions{Path: newDatabasePath(t)})
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	defer store.Close()

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All returned an error: %v", err)
	}

	// Bring the database to the version before agent_states by hand.
	if _, err := store.db.ExecContext(ctx, schemaMigrationsTable); err != nil {
		t.Fatalf("creating schema_migrations failed: %v", err)
	}
	// Two versions back, so that the upgrade under test is the pair this phase
	// added rather than one of them.
	target := latestVersion(t) - 2
	for _, m := range all {
		if m.Version > target {
			break
		}
		if _, err := store.db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatalf("applying %s failed: %v", m, err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, formatTime(at(2026, time.September, 1, 0)),
		); err != nil {
			t.Fatalf("recording %s failed: %v", m, err)
		}
	}

	// One row in every table this build already had.
	const (
		projectID = "p_0123456789abcdef0123"
		taskID    = "task_0123456789abcdef0123"
		sessionID = "sess_0123456789abcdef0123"
		eventID   = "evt_0123456789abcdef01234567"
	)
	if err := store.Projects().Create(ctx, testProject(projectID, "App", `D:\AI\Projects\App`)); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}
	if err := store.Runtimes().Save(ctx, session.Record{
		ProjectID: projectID,
		Session:   "amx-" + projectID,
		State:     session.StateRunning,
		Cols:      120,
		Rows:      30,
		CreatedAt: at(2026, time.September, 1, 0),
		UpdatedAt: at(2026, time.September, 1, 0),
	}); err != nil {
		t.Fatalf("saving the runtime record failed: %v", err)
	}
	if err := store.Tasks().CreateTask(ctx, &task.Task{
		ID: taskID, ProjectID: projectID, Title: "Fix the viewer",
		Status:    task.StatusCreated,
		CreatedAt: at(2026, time.September, 1, 0),
		UpdatedAt: at(2026, time.September, 1, 0),
	}); err != nil {
		t.Fatalf("creating the task failed: %v", err)
	}
	if err := store.Tasks().CreateSession(ctx, &task.AgentSession{
		ID: sessionID, TaskID: taskID, Status: task.StatusSessionCreated,
		CreatedAt: at(2026, time.September, 1, 0),
	}); err != nil {
		t.Fatalf("creating the session failed: %v", err)
	}
	if err := store.Events().Create(ctx, eventForTest(eventID, projectID)); err != nil {
		t.Fatalf("creating the event failed: %v", err)
	}

	// The upgrade.
	result, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}
	want := []string{"0008_agent_actions", "0009_usage_events"}
	if len(result.Applied) != len(want) {
		t.Fatalf("Migrate applied %v; want %v", result.Applied, want)
	}
	for i := range want {
		if !strings.HasPrefix(result.Applied[i], want[i]) {
			t.Errorf("Migrate applied %q; want %q", result.Applied[i], want[i])
		}
	}

	// Every row is still there, unchanged.
	if _, err := store.Projects().GetByID(ctx, projectID); err != nil {
		t.Errorf("the project did not survive the upgrade: %v", err)
	}
	if _, err := store.Runtimes().Get(ctx, projectID); err != nil {
		t.Errorf("the runtime record did not survive the upgrade: %v", err)
	}
	if _, err := store.Tasks().GetTask(ctx, taskID); err != nil {
		t.Errorf("the task did not survive the upgrade: %v", err)
	}
	if _, err := store.Tasks().GetSession(ctx, sessionID); err != nil {
		t.Errorf("the session did not survive the upgrade: %v", err)
	}
	if _, err := store.Events().List(ctx, event.Query{ProjectID: projectID, Limit: 10}); err != nil {
		t.Errorf("the event did not survive the upgrade: %v", err)
	}

	// And the new tables are empty rather than populated with a guess. A
	// migration that seeded rows would be a migration inventing history.
	states, err := store.AgentStates().Count(ctx)
	if err != nil {
		t.Fatalf("counting the state table failed: %v", err)
	}
	if states != 0 {
		t.Errorf("the upgrade left %d agent state(s); a schema change does not invent history", states)
	}
	attentionRows, err := store.Attention().CountAttention(ctx)
	if err != nil {
		t.Fatalf("counting the attention table failed: %v", err)
	}
	if attentionRows != 0 {
		t.Errorf("the upgrade left %d attention row(s); a schema change does not invent history", attentionRows)
	}
	actions, err := store.Attention().CountActions(ctx)
	if err != nil {
		t.Fatalf("counting the action table failed: %v", err)
	}
	if actions != 0 {
		t.Errorf("the upgrade left %d action(s); a schema change does not invent history", actions)
	}
}

// TestMigrateIsIdempotentOverAFullDatabase runs it twice.
func TestMigrateIsIdempotentOverAFullDatabase(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("the first Migrate returned an error: %v", err)
	}
	second, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("the second Migrate returned an error: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("the second Migrate applied %v; want nothing", second.Applied)
	}
	if first.Version != second.Version {
		t.Errorf("schema version moved from %d to %d on a second Migrate", first.Version, second.Version)
	}
}
