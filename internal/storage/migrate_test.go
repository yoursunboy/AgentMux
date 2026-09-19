package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/project"
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

	want := []string{"agent_events", "project_runtime", "projects", "schema_migrations", "settings"}
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
