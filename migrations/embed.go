// Package migrations carries the versioned SQLite schema migrations.
//
// A migration file is named "<version>_<name>.sql", for example
// "0001_initial_schema.sql". Versions are unique and applied in ascending
// order. Applied versions are recorded in the schema_migrations table, so a
// migration never runs twice.
//
// Migrations are additive and forward-only. There are no down migrations:
// un-applying a metadata schema is not a workflow AgentMux supports, and a
// half-reverted database is worse than a newer one.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// files holds the migration bodies. They are embedded rather than read from
// disk so that a built binary always carries the schema it expects, with no
// deployment step that could ship a binary and its migrations out of sync.
//
//go:embed *.sql
var files embed.FS

// Migration is one versioned schema change.
type Migration struct {
	// Version orders the migrations. It is the numeric prefix of the file
	// name.
	Version int

	// Name is the descriptive part of the file name.
	Name string

	// Source is the file the migration was read from.
	Source string

	// SQL is the migration body. A body may contain several statements; the
	// SQLite driver executes them as a batch.
	SQL string
}

// String renders the migration as "0001_initial_schema".
func (m Migration) String() string {
	return fmt.Sprintf("%04d_%s", m.Version, m.Name)
}

// All returns every migration in ascending version order.
//
// It returns an error for an unparsable file name or a duplicated version,
// because either would make the applied-schema order ambiguous.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: read embedded files: %w", err)
	}

	out := make([]Migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		migration, err := parse(name)
		if err != nil {
			return nil, err
		}
		if previous, dup := seen[migration.Version]; dup {
			return nil, fmt.Errorf("migrations: version %d is used by both %s and %s",
				migration.Version, previous, name)
		}
		seen[migration.Version] = name
		out = append(out, migration)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("migrations: no .sql files found")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func parse(fileName string) (Migration, error) {
	base := strings.TrimSuffix(fileName, ".sql")
	prefix, name, ok := strings.Cut(base, "_")
	if !ok {
		return Migration{}, fmt.Errorf("migrations: %s must be named <version>_<name>.sql", fileName)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return Migration{}, fmt.Errorf("migrations: %s has a non-numeric version prefix: %w", fileName, err)
	}
	if version <= 0 {
		return Migration{}, fmt.Errorf("migrations: %s must use a version greater than zero", fileName)
	}
	body, err := files.ReadFile(fileName)
	if err != nil {
		return Migration{}, fmt.Errorf("migrations: read %s: %w", fileName, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return Migration{}, fmt.Errorf("migrations: %s is empty", fileName)
	}
	return Migration{
		Version: version,
		Name:    name,
		Source:  fileName,
		SQL:     string(body),
	}, nil
}
