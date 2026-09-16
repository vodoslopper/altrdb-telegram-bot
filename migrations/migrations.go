// Package migrations embeds the versioned SQL schema of altrdb-telegram-bot.
//
// Files are named NNNN_description.sql and applied in lexical order, each
// exactly once, recorded in the schema_migrations table. Adding a change means
// adding a new file; never edit an already released one.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var sqlFiles embed.FS

// Migration is one versioned schema change.
type Migration struct {
	Version int
	Name    string // file name, e.g. "0001_init.sql"
	SQL     string
}

// All returns every embedded migration, ordered by version.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(sqlFiles, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	out := make([]Migration, 0, len(names))
	seen := make(map[int]string, len(names))
	for _, name := range names {
		version, err := versionOf(name)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", prev, name, version)
		}
		seen[version] = name

		body, err := sqlFiles.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("migration %s is empty", name)
		}
		out = append(out, Migration{Version: version, Name: name, SQL: string(body)})
	}
	return out, nil
}

// versionOf parses the leading "NNNN_" of a migration file name.
func versionOf(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("migration %q must be named NNNN_description.sql", name)
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, fmt.Errorf("migration %q has a non-numeric version: %w", name, err)
	}
	return v, nil
}
