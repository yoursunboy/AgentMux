package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// newTestStore opens a migrated database inside the test's temp directory.
//
// Every test gets its own file, so nothing here can touch a real AgentMux data
// directory or a real project.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return newTestStoreAt(t, filepath.Join(t.TempDir(), "agentmux.db"))
}

func newTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(context.Background(), OpenOptions{Path: path})
	if err != nil {
		t.Fatalf("Open(%q) returned an error: %v", path, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("closing the store failed: %v", err)
		}
	})
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}
	return store
}

// at is a convenience for building the timestamps the tests store.
func at(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
}

func TestOpenCreatesTheFileAndItsParentDirectory(t *testing.T) {
	// Two levels of directory that do not exist yet, because the data
	// directory is created on first run.
	path := filepath.Join(t.TempDir(), "data", "nested", "agentmux.db")

	store, err := Open(context.Background(), OpenOptions{Path: path})
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("the database file was not created: %v", err)
	}
	if store.Path() != path {
		t.Errorf("Path() = %q, want %q", store.Path(), path)
	}
}

// TestOpenResolvesARelativePath records that the store reports an absolute
// path, so a log line or an API response never depends on the process working
// directory.
func TestOpenResolvesARelativePath(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(context.Background(), OpenOptions{Path: filepath.Join(dir, "relative.db")})
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	defer store.Close()

	if !filepath.IsAbs(store.Path()) {
		t.Errorf("Path() = %q, want an absolute path", store.Path())
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	for _, path := range []string{"", "   ", "\t"} {
		if store, err := Open(context.Background(), OpenOptions{Path: path}); err == nil {
			store.Close()
			t.Errorf("Open(%q) succeeded, want a rejection", path)
		}
	}
}

// TestOpenLeavesExistingDataAlone guards the upgrade path: opening a database
// that already holds projects must not reset it.
func TestOpenLeavesExistingDataAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentmux.db")

	first := newTestStoreAt(t, path)
	if err := first.Projects().Create(context.Background(), testProject("p_1", "App", "/data/App")); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	second, err := Open(context.Background(), OpenOptions{Path: path})
	if err != nil {
		t.Fatalf("reopening returned an error: %v", err)
	}
	defer second.Close()

	projects, err := second.Projects().List(context.Background(), project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("reopening the database left %d projects, want 1", len(projects))
	}
}

// TestCloseIsIdempotent matters because main defers a close and also closes on
// the shutdown path.
func TestCloseIsIdempotent(t *testing.T) {
	store, err := Open(context.Background(), OpenOptions{Path: filepath.Join(t.TempDir(), "a.db")})
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("the first Close returned an error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("the second Close returned an error: %v", err)
	}

	var nilStore *Store
	if err := nilStore.Close(); err != nil {
		t.Errorf("closing a nil store returned an error: %v", err)
	}
}

// TestDataDirectoryIsSelfContained records where the schema lives. Migrations
// are embedded, so a built binary needs no .sql files next to it.
func TestDataDirectoryIsSelfContained(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreAt(t, filepath.Join(dir, "agentmux.db"))

	if version, err := store.SchemaVersion(context.Background()); err != nil || version == 0 {
		t.Fatalf("SchemaVersion() = %d, %v; want a migrated schema", version, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0001_initial_schema.sql")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the store must not depend on migration files on disk")
	}
}

func TestDataSourceNameCarriesEveryPragma(t *testing.T) {
	dsn := dataSourceName(`D:\AI\Projects\x\agentmux.db`, 5000)

	// A plain path, not a file: URI, because a Windows path is not a valid URI.
	if !strings.HasPrefix(dsn, `D:\AI\Projects\x\agentmux.db?`) {
		t.Errorf("dataSourceName = %q, want the path followed by its parameters", dsn)
	}
	for _, want := range []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(1)",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dataSourceName = %q, want it to contain %q", dsn, want)
		}
	}
}

// TestJournalModeIsActuallyWAL proves the pragma reached the connection rather
// than being silently dropped by the driver.
func TestJournalModeIsActuallyWAL(t *testing.T) {
	store := newTestStore(t)

	var mode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("could not read journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestFormatAndParseTimeRoundTrip(t *testing.T) {
	original := time.Date(2026, 9, 17, 12, 30, 45, 123456789, time.UTC)

	parsed, err := parseTime(formatTime(original))
	if err != nil {
		t.Fatalf("parseTime returned an error: %v", err)
	}
	if !parsed.Equal(original) {
		t.Errorf("round trip produced %v, want %v", parsed, original)
	}
}

// TestFormatTimeNormalisesToUTC keeps stored timestamps comparable regardless
// of the zone the server process happens to run in.
func TestFormatTimeNormalisesToUTC(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	local := time.Date(2026, 9, 17, 20, 0, 0, 0, zone)

	stored := formatTime(local)
	if !strings.HasSuffix(stored, "Z") {
		t.Errorf("formatTime = %q, want a UTC value", stored)
	}
	if stored != "2026-09-17T12:00:00Z" {
		t.Errorf("formatTime = %q, want %q", stored, "2026-09-17T12:00:00Z")
	}
}

func TestParseTimeAcceptsBothSpellings(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"nanosecond form", "2026-09-17T12:00:00.5Z", time.Date(2026, 9, 17, 12, 0, 0, 500000000, time.UTC)},
		{"second form", "2026-09-17T12:00:00Z", at(2026, time.September, 17, 12)},
		{"empty", "", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTime(tt.value)
			if err != nil {
				t.Fatalf("parseTime(%q) returned an error: %v", tt.value, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("parseTime(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestParseTimeRefusesGarbage records the deliberate choice: a corrupt
// timestamp is reported rather than becoming the zero time, which would make a
// project look like it was created in year 1.
func TestParseTimeRefusesGarbage(t *testing.T) {
	for _, value := range []string{"not a time", "17/09/2026", "2026-13-45T99:00:00Z"} {
		if got, err := parseTime(value); err == nil {
			t.Errorf("parseTime(%q) = %v, want an error", value, got)
		}
	}
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"an unrelated error", errors.New("disk is full"), false},
		{"the driver's wording", errors.New("constraint failed: UNIQUE constraint failed: projects.host_path"), true},
		{"the shorter wording", errors.New("UNIQUE constraint failed"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUniqueViolation(tt.err); got != tt.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNullableHelpers(t *testing.T) {
	if got := nullableInt(nil); got != nil {
		t.Errorf("nullableInt(nil) = %v, want nil", got)
	}
	slot := 4
	if got := nullableInt(&slot); got != 4 {
		t.Errorf("nullableInt(&4) = %v, want 4", got)
	}
	if got := nullableTime(nil); got != nil {
		t.Errorf("nullableTime(nil) = %v, want nil", got)
	}
	when := at(2026, time.September, 17, 12)
	if got := nullableTime(&when); got != formatTime(when) {
		t.Errorf("nullableTime = %v, want %q", got, formatTime(when))
	}
	if boolToInt(true) != 1 || boolToInt(false) != 0 {
		t.Error("boolToInt must map true to 1 and false to 0")
	}
}
