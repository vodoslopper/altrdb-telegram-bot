package migrations

import (
	"strings"
	"testing"
)

func TestAllReturnsOrderedMigrations(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations are embedded; the build must have lost the .sql files")
	}

	prev := 0
	seen := map[int]bool{}
	for _, m := range all {
		if m.Version <= prev {
			t.Errorf("migration %s has version %d, which does not follow %d", m.Name, m.Version, prev)
		}
		prev = m.Version
		if seen[m.Version] {
			t.Errorf("version %d appears twice", m.Version)
		}
		seen[m.Version] = true

		if !strings.HasSuffix(m.Name, ".sql") {
			t.Errorf("migration %s is not a .sql file", m.Name)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %s is empty", m.Name)
		}
		if m.Version <= 0 {
			t.Errorf("migration %s has a non-positive version", m.Name)
		}
	}
}

func TestInitialMigrationDefinesTheWholeSchema(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	first := all[0]
	if first.Version != 1 {
		t.Fatalf("the first migration is version %d, want 1", first.Version)
	}

	// Every table the application queries must be created by the schema. If a
	// new table is added without a migration, this is where it shows up.
	for _, table := range []string{
		"chats", "chat_maintainers", "chat_states", "chat_branches",
		"conversation_state", "rdb_branches", "rdb_states", "maintainers_cache",
		"task_snapshots", "notifications", "poll_state", "app_lock",
		"telegram_offset", "meta",
	} {
		if !strings.Contains(first.SQL, "CREATE TABLE "+table+" (") {
			t.Errorf("migration %s does not create table %s", first.Name, table)
		}
	}

	// The two constraints the deduplication guarantee rests on.
	if !strings.Contains(first.SQL, "UNIQUE (chat_id, fingerprint)") {
		t.Error("the outbox must keep UNIQUE (chat_id, fingerprint)")
	}
	if !strings.Contains(first.SQL, "REFERENCES chats (id) ON DELETE CASCADE") {
		t.Error("subscription tables must cascade from chats")
	}
	if !strings.Contains(first.SQL, "idx_notifications_due") {
		t.Error("the due-outbox index is missing")
	}
}

func TestVersionOf(t *testing.T) {
	tests := []struct {
		name    string
		want    int
		wantErr bool
	}{
		{"0001_init.sql", 1, false},
		{"0042_add_column.sql", 42, false},
		{"9999_x.sql", 9999, false},
		{"init.sql", 0, true},
		{"_init.sql", 0, true},
		{"abc_init.sql", 0, true},
		{"1.sql", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := versionOf(tc.name)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("versionOf(%q) = %d, want an error", tc.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("versionOf(%q): %v", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("versionOf(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}
