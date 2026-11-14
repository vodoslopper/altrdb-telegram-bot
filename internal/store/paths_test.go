package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabasePathsPreserveURICharacters(t *testing.T) {
	for _, name := range []string{"question?.db", "fragment#.db", "percent%3f.db", "space name.db"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), name)
			st, err := Open(ctx, path, Options{Migrate: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetMeta(ctx, "marker", name); err != nil {
				st.Close()
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("literal database path missing: %v", err)
			}
			st, err = Open(ctx, path, Options{Migrate: true})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if got, err := st.GetMeta(ctx, "marker"); err != nil || got != name {
				t.Fatalf("reopened wrong database: %q, %v", got, err)
			}
		})
	}
}
