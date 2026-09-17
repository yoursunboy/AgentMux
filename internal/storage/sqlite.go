// Package storage owns the AgentMux SQLite metadata database.
//
// Boundaries this package holds:
//
//   - Every SQL statement lives here. Services and HTTP handlers never see a
//     *sql.DB, so a query cannot appear in a handler by accident.
//   - Schema changes go through migrations. There is no ad-hoc DDL at runtime.
//   - Times are stored as RFC3339 UTC text, so the database file stays
//     readable with any SQLite tool instead of holding opaque integers.
//   - SQLite holds metadata only. Terminal output is never stored here; an
//     unbounded output log is not a metadata concern.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Pure-Go SQLite. Chosen over a cgo driver because AgentMux must build on
	// Windows and Linux without a C toolchain.
	_ "modernc.org/sqlite"
)

const (
	driverName = "sqlite"

	// defaultBusyTimeoutMS is how long SQLite waits for a lock before giving
	// up. A self-hosted server has brief write bursts, not sustained
	// contention.
	defaultBusyTimeoutMS = 5000

	// timeFormat is the storage format for every timestamp column.
	timeFormat = time.RFC3339Nano
)

// Store is the AgentMux metadata store.
type Store struct {
	db   *sql.DB
	path string
}

// OpenOptions configures Open.
type OpenOptions struct {
	// Path is the database file. Missing parent directories are created.
	Path string

	// BusyTimeoutMS is how long SQLite waits for a lock. Zero means
	// defaultBusyTimeoutMS.
	BusyTimeoutMS int

	// MaxOpenConns bounds the pool. Zero means 1.
	//
	// SQLite serialises writers, so a single connection is the simplest
	// correct setting: it removes lock contention entirely rather than
	// relying on busy handling. Revisit if read concurrency ever matters.
	MaxOpenConns int
}

// Open opens the database, creating the file and its parent directory when
// they do not exist.
func Open(ctx context.Context, o OpenOptions) (*Store, error) {
	raw := strings.TrimSpace(o.Path)
	if raw == "" {
		return nil, errors.New("storage: database path must not be empty")
	}
	path, err := filepath.Abs(raw)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve %q: %w", raw, err)
	}
	path = filepath.Clean(path)

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("storage: create %s: %w", dir, err)
		}
	}

	timeout := o.BusyTimeoutMS
	if timeout <= 0 {
		timeout = defaultBusyTimeoutMS
	}
	conns := o.MaxOpenConns
	if conns <= 0 {
		conns = 1
	}

	db, err := sql.Open(driverName, dataSourceName(path, timeout))
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	// Never expire the connection: reconnecting would have to re-apply the
	// per-connection pragmas, and there is no benefit for a local file.
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: connect to %s: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

// Path is the absolute database file path.
func (s *Store) Path() string { return s.path }

// Close releases the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Projects returns the project repository.
func (s *Store) Projects() *ProjectStore { return &ProjectStore{db: s.db} }

// Settings returns the settings repository.
func (s *Store) Settings() *SettingStore { return &SettingStore{db: s.db} }

// dataSourceName builds the driver connection string.
//
// Pragmas are passed as DSN parameters rather than executed after opening, so
// that every connection the pool creates receives them and not just the first.
//
// A plain path is used rather than a file: URI, because a Windows path such as
// D:\AI\Projects\... is not a valid URI. The parameter values are fixed
// constants and are deliberately not URL-encoded: the driver matches the
// literal "_pragma=busy_timeout(5000)" form.
func dataSourceName(path string, busyTimeoutMS int) string {
	params := []string{
		fmt.Sprintf("_pragma=busy_timeout(%d)", busyTimeoutMS),
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(1)",
	}
	return path + "?" + strings.Join(params, "&")
}

// formatTime renders a timestamp for storage, in UTC.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

// parseTime reads a stored timestamp. An unparsable value is reported rather
// than silently becoming the zero time, because a zero time would make a
// project look like it was created in year 1.
func parseTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeFormat, value)
	if err != nil {
		// Tolerate a value written without the nanosecond part.
		if t2, err2 := time.Parse(time.RFC3339, value); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, fmt.Errorf("storage: parse timestamp %q: %w", value, err)
	}
	return t.UTC(), nil
}

// isUniqueViolation reports whether err is a uniqueness constraint failure.
//
// The check is on the message text because it is the one signal the pure-Go
// driver surfaces identically across versions. A false negative is harmless:
// the caller reports a storage failure instead of a duplicate, and the service
// layer already checks for an existing project before inserting.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
