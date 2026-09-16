package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/model"

	"io"
	"log/slog"
)

// epoch is the fixed "now" every store test starts from.
var epoch = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// newTestStore opens a throwaway database in t.TempDir(), migrated and with a
// controllable clock.
func newTestStore(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	return newTestStoreAt(t, filepath.Join(t.TempDir(), "bot.db"))
}

func newTestStoreAt(t *testing.T, path string) (*Store, *clock.Fake) {
	t.Helper()
	fake := clock.NewFake(epoch)
	st, err := Open(context.Background(), path, Options{
		Migrate: true,
		Clock:   fake,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, fake
}

func TestMigrateFromEmptyDatabase(t *testing.T) {
	st, _ := newTestStore(t)

	ver, err := st.SchemaVersionFromDB(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersionFromDB: %v", err)
	}
	if ver < 1 {
		t.Errorf("schema version = %d, want at least 1", ver)
	}
	if st.SchemaVersion() != ver {
		t.Errorf("cached version %d differs from the database %d", st.SchemaVersion(), ver)
	}

	// Every table the application relies on must exist after migration.
	tables := []string{
		"chats", "chat_maintainers", "chat_states", "chat_branches", "conversation_state",
		"rdb_branches", "rdb_states", "maintainers_cache", "task_snapshots", "notifications",
		"poll_state", "app_lock", "telegram_offset", "meta", "schema_migrations",
	}
	for _, table := range tables {
		var name string
		err := st.DB().QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after migration: %v", table, err)
		}
	}

	// The seeded lifecycle states must be selectable straight away.
	states, err := st.StateNames(context.Background())
	if err != nil {
		t.Fatalf("StateNames: %v", err)
	}
	if len(states) != len(model.KnownStates) {
		t.Errorf("seeded states = %v, want %v", states, model.KnownStates)
	}
	if states[0] != "NEW" || states[len(states)-1] != "DONE" {
		t.Errorf("seeded states are not in lifecycle order: %v", states)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.db")

	st, fake := newTestStoreAt(t, path)
	ver1, err := st.SchemaVersionFromDB(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersionFromDB: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	_ = st.Close()

	// Reopening must not re-apply anything and must not duplicate the seed.
	st2, err := Open(context.Background(), path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	ver2, err := st2.SchemaVersionFromDB(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersionFromDB: %v", err)
	}
	if ver1 != ver2 {
		t.Errorf("version moved from %d to %d on a re-migrate", ver1, ver2)
	}
	var n int
	if err := st2.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM rdb_states`).Scan(&n); err != nil {
		t.Fatalf("counting states: %v", err)
	}
	if n != len(model.KnownStates) {
		t.Errorf("rdb_states has %d rows after a second migration, want %d", n, len(model.KnownStates))
	}
}

func TestOpenCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "bot.db")
	st, err := Open(context.Background(), path, Options{Migrate: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file was not created: %v", err)
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), "", Options{}); err == nil {
		t.Fatal("an empty path must be rejected")
	}
}

func TestDSNEnablesTheRequiredPragmas(t *testing.T) {
	dsn := DSN("/var/lib/x/bot.db", 3*time.Second)
	for _, want := range []string{
		"journal_mode(WAL)", "foreign_keys(1)", "busy_timeout(3000)",
		"synchronous(NORMAL)", "_txlock=immediate",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN %q is missing %q", dsn, want)
		}
	}
}

func TestWALAndForeignKeysAreActuallyOn(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	var mode string
	if err := st.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := st.DB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var busy int
	if err := st.DB().QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d ms, want 5000", busy)
	}
}

func TestForeignKeyCascadeDeletesSubscriptions(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := st.RegisterChat(ctx, 7, "private", "someone", "Some One"); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if _, err := st.AddMaintainers(ctx, 7, []string{"grenka"}); err != nil {
		t.Fatalf("AddMaintainers: %v", err)
	}
	if _, err := st.ToggleChatState(ctx, 7, "DONE"); err != nil {
		t.Fatalf("ToggleChatState: %v", err)
	}
	if _, _, err := st.ToggleChatBranch(ctx, 7, "sisyphus"); err != nil {
		t.Fatalf("ToggleChatBranch: %v", err)
	}
	if err := st.SetConversation(ctx, 7, model.StepAwaitingMaintainers, ""); err != nil {
		t.Fatalf("SetConversation: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `DELETE FROM chats WHERE id = 7`); err != nil {
		t.Fatalf("deleting the chat: %v", err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM chat_maintainers`,
		`SELECT COUNT(*) FROM chat_states`,
		`SELECT COUNT(*) FROM chat_branches`,
		`SELECT COUNT(*) FROM conversation_state`,
	} {
		var n int
		if err := st.DB().QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Errorf("%s = %d after the parent chat was deleted, want 0", q, n)
		}
	}
}

func TestMetaRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	got, err := st.GetMeta(ctx, "missing")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "" {
		t.Errorf("a missing key = %q, want empty", got)
	}
	if err := st.SetMeta(ctx, "k", "v1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := st.SetMeta(ctx, "k", "v2"); err != nil {
		t.Fatalf("SetMeta update: %v", err)
	}
	got, err = st.GetMeta(ctx, "k")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "v2" {
		t.Errorf("value = %q, want v2", got)
	}
}

func TestTelegramOffsetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.db")
	ctx := context.Background()

	st, fake := newTestStoreAt(t, path)
	if got, err := st.TelegramOffset(ctx); err != nil || got != 0 {
		t.Fatalf("initial offset = %d, %v; want 0", got, err)
	}
	if err := st.SaveTelegramOffset(ctx, 100); err != nil {
		t.Fatalf("SaveTelegramOffset: %v", err)
	}
	// A lower id must never move the high-water mark backwards.
	if err := st.SaveTelegramOffset(ctx, 42); err != nil {
		t.Fatalf("SaveTelegramOffset: %v", err)
	}
	if got, _ := st.TelegramOffset(ctx); got != 100 {
		t.Errorf("offset = %d, want 100", got)
	}
	_ = st.Close()

	st2, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if got, err := st2.TelegramOffset(ctx); err != nil || got != 100 {
		t.Errorf("offset after restart = %d, %v; want 100", got, err)
	}
}

// ---------------------------------------------------------------------------
// lease
// ---------------------------------------------------------------------------

func TestLeaseRejectsASecondHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.db")
	ctx := context.Background()
	// Both handles share one clock: the lease TTL is measured against it.
	fake := clock.NewFake(epoch)

	first, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer first.Close()
	if err := first.AcquireLease(ctx, "host:1:db", 1, 90*time.Second); err != nil {
		t.Fatalf("first AcquireLease: %v", err)
	}

	second, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("opening a second handle: %v", err)
	}
	defer second.Close()

	err = second.AcquireLease(ctx, "host:2:db", 2, 90*time.Second)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second AcquireLease = %v, want ErrLeaseHeld", err)
	}

	// The holder can renew its own lease.
	if err := first.RenewLease(ctx, "host:1:db"); err != nil {
		t.Errorf("RenewLease by the holder: %v", err)
	}
	if err := second.RenewLease(ctx, "host:2:db"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("RenewLease by a non-holder = %v, want ErrLeaseLost", err)
	}

	holder, heartbeat, err := first.LeaseHolder(ctx)
	if err != nil {
		t.Fatalf("LeaseHolder: %v", err)
	}
	if holder != "host:1:db" || heartbeat.IsZero() {
		t.Errorf("holder = %q, heartbeat = %s", holder, heartbeat)
	}

	if err := first.ReleaseLease(ctx, "host:1:db"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if err := second.AcquireLease(ctx, "host:2:db", 2, 90*time.Second); err != nil {
		t.Errorf("acquiring after a release: %v", err)
	}
}

func TestLeaseIsStolenAfterTheTTLExpires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.db")
	ctx := context.Background()
	fake := clock.NewFake(epoch)

	st, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := st.AcquireLease(ctx, "crashed-process", 1, 90*time.Second); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	fake.Advance(91 * time.Second)

	other, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer other.Close()

	if err := other.AcquireLease(ctx, "new-process", 2, 90*time.Second); err != nil {
		t.Fatalf("a lease whose heartbeat expired must be stealable: %v", err)
	}
	holder, _, err := other.LeaseHolder(ctx)
	if err != nil {
		t.Fatalf("LeaseHolder: %v", err)
	}
	if holder != "new-process" {
		t.Errorf("holder = %q, want new-process", holder)
	}
}

func TestKeepLeaseStopsWhenTheLeaseIsLost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.db")
	ctx := context.Background()
	fake := clock.NewFake(epoch)

	first, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer first.Close()
	if err := first.AcquireLease(ctx, "a", 1, time.Second); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	second, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer second.Close()

	// Steal it out from under the first process.
	if _, err := second.DB().ExecContext(ctx, `UPDATE app_lock SET holder = 'b' WHERE id = 1`); err != nil {
		t.Fatalf("stealing the lease: %v", err)
	}

	lost := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		first.KeepLease(ctx, "a", 5*time.Millisecond, func(err error) { lost <- err })
		close(done)
	}()

	select {
	case err := <-lost:
		if !errors.Is(err, ErrLeaseLost) {
			t.Errorf("onLost error = %v, want ErrLeaseLost", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("KeepLease did not notice the lost lease")
	}
	<-done
}
