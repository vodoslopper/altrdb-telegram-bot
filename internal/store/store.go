// Package store is the persistence layer: SQLite by default, behind narrow
// interfaces so a PostgreSQL driver could replace it without touching callers.
//
// Every timestamp is stored as RFC 3339 UTC text and every comparison between
// two of them happens in Go, never in SQL. That keeps the schema portable and
// avoids the classic trap where "2026-01-01T00:00:00.5Z" sorts before
// "2026-01-01T00:00:00Z" as plain text.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/migrations"
)

// DriverName is the database/sql driver this build uses.
const DriverName = "sqlite"

// Errors returned by the store.
var (
	// ErrLeaseHeld means another live process owns the database.
	ErrLeaseHeld = errors.New("database lease is held by another process")
	// ErrLeaseLost means this process no longer owns the database.
	ErrLeaseLost = errors.New("database lease was lost")
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("not found")
)

// Store owns the database handle, the clock and the logger.
type Store struct {
	db    *sql.DB
	clock clock.Clock
	log   *slog.Logger
	path  string

	version int
}

// Options tunes how the database is opened.
type Options struct {
	// MaxOpenConns caps the connection pool. SQLite serialises writers
	// anyway; more than a handful of connections buys nothing.
	MaxOpenConns int
	// BusyTimeout is how long a writer waits for the write lock.
	BusyTimeout time.Duration
	// Clock overrides the time source, for tests.
	Clock clock.Clock
	// Logger receives migration and lease messages.
	Logger *slog.Logger
	// Migrate runs pending migrations during Open when true.
	Migrate bool
}

// DSN builds the SQLite connection string for path.
//
// WAL keeps readers off the writer's back, foreign_keys enforces the CASCADEs
// the schema relies on, busy_timeout turns lock contention into a wait instead
// of an error, synchronous=NORMAL is the safe-and-fast WAL setting, and
// _txlock=immediate makes every database/sql transaction take the write lock
// up front. Without that last one two concurrent read-then-write transactions
// can deadlock into an immediate SQLITE_BUSY that busy_timeout will not retry.
func DSN(path string, busyTimeout time.Duration) string {
	if busyTimeout <= 0 {
		busyTimeout = 5 * time.Second
	}
	q := url.Values{}
	for _, p := range []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()),
		"journal_mode(WAL)",
		"foreign_keys(1)",
		"synchronous(NORMAL)",
		"cache_size(-16000)",
		"temp_store(FILE)",
	} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	q.Set("_time_format", "sqlite")
	// Parens are legal unescaped in a query value and the driver decodes them
	// either way; keeping them readable makes the DSN usable in logs and docs.
	encoded := strings.NewReplacer("%28", "(", "%29", ")").Replace(q.Encode())
	return "file:" + path + "?" + encoded
}

// Open opens (and creates if needed) the database at path and applies any
// pending migrations when opts.Migrate is set.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if opts.Clock == nil {
		opts.Clock = clock.New()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxOpenConns <= 0 {
		opts.MaxOpenConns = 4
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 5 * time.Second
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	db, err := sql.Open(DriverName, DSN(path, opts.BusyTimeout))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(opts.MaxOpenConns)
	db.SetMaxIdleConns(opts.MaxOpenConns)
	db.SetConnMaxIdleTime(10 * time.Minute)

	s := &Store{db: db, clock: opts.Clock, log: opts.Logger, path: path}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if err := s.ensureMigrationsTable(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if opts.Migrate {
		if err := s.Migrate(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	ver, err := s.SchemaVersionFromDB(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.version = ver
	return s, nil
}

// DB exposes the raw handle. Prefer the typed methods; this exists for the
// health probe and for tests.
func (s *Store) DB() *sql.DB { return s.db }

// Path is the database file this store opened.
func (s *Store) Path() string { return s.path }

// Clock is the time source the store stamps rows with.
func (s *Store) Clock() clock.Clock { return s.clock }

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion() int { return s.version }

// Close releases the connection pool.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping verifies the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) ensureMigrationsTable(ctx context.Context) error {
	const q = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	return nil
}

// AppliedVersions lists the migration versions already in the database.
func (s *Store) AppliedVersions(ctx context.Context) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[int]string{}
	for rows.Next() {
		var v int
		var name string
		if err := rows.Scan(&v, &name); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		out[v] = name
	}
	return out, rows.Err()
}

// SchemaVersion reads the highest applied migration version from disk.
func (s *Store) SchemaVersionFromDB(ctx context.Context) (int, error) {
	applied, err := s.AppliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	max := 0
	for v := range applied {
		if v > max {
			max = v
		}
	}
	return max, nil
}

// Migrate applies every embedded migration that has not run yet, one
// transaction per file, and then seeds the known task states.
func (s *Store) Migrate(ctx context.Context) error {
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	applied, err := s.AppliedVersions(ctx)
	if err != nil {
		return err
	}

	for _, m := range all {
		if _, done := applied[m.Version]; done {
			continue
		}
		s.log.Info("applying migration", "version", m.Version, "name", m.Name)

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin migration %s: %w", m.Name, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: apply migration %s: %w", m.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, model.FormatTime(s.clock.Now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: record migration %s: %w", m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", m.Name, err)
		}
	}

	ver, err := s.SchemaVersionFromDB(ctx)
	if err != nil {
		return err
	}
	s.version = ver

	if err := s.SeedKnownStates(ctx); err != nil {
		return err
	}
	return nil
}

// SeedKnownStates inserts the built-in lifecycle states and upgrades any that
// were first learned from the API. Idempotent.
func (s *Store) SeedKnownStates(ctx context.Context) error {
	now := model.FormatTime(s.clock.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin seed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO rdb_states (state, known, sort_rank, first_seen_at, last_seen_at)
		VALUES (?, 1, ?, ?, ?)
		ON CONFLICT (state) DO UPDATE SET
			known = 1,
			sort_rank = excluded.sort_rank`)
	if err != nil {
		return fmt.Errorf("store: prepare seed: %w", err)
	}
	defer stmt.Close()

	for _, state := range model.KnownStates {
		if _, err := stmt.ExecContext(ctx, state, model.StateRank(state), now, now); err != nil {
			return fmt.Errorf("store: seed state %s: %w", state, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit seed: %w", err)
	}
	return nil
}

// tx runs fn inside a transaction, rolling back on any error or panic.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		_ = tx.Rollback() // no-op after Commit
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// now is the store-wide timestamp helper.
func (s *Store) now() string { return model.FormatTime(s.clock.Now()) }

// ---------------------------------------------------------------------------
// single-instance lease
// ---------------------------------------------------------------------------

// AcquireLease claims the database for holder, stealing it only from a holder
// whose heartbeat is older than ttl.
func (s *Store) AcquireLease(ctx context.Context, holder string, pid int, ttl time.Duration) error {
	now := s.clock.Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		var existingHolder string
		var heartbeat string
		err := tx.QueryRowContext(ctx,
			`SELECT holder, heartbeat_at FROM app_lock WHERE id = 1`).Scan(&existingHolder, &heartbeat)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err := tx.ExecContext(ctx,
				`INSERT INTO app_lock (id, holder, pid, acquired_at, heartbeat_at) VALUES (1, ?, ?, ?, ?)`,
				holder, pid, model.FormatTime(now), model.FormatTime(now))
			if err != nil {
				return fmt.Errorf("store: claim lease: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("store: read lease: %w", err)
		}

		if existingHolder == holder {
			return s.touchLease(ctx, tx, holder, pid, now)
		}
		hb, perr := model.ParseTime(heartbeat)
		if perr == nil && now.Sub(hb) < ttl {
			return fmt.Errorf("%w: %s (heartbeat %s)", ErrLeaseHeld, existingHolder, heartbeat)
		}
		s.log.Warn("stealing stale database lease",
			"previous_holder", existingHolder, "last_heartbeat", heartbeat, "ttl", ttl.String())
		_, err = tx.ExecContext(ctx,
			`UPDATE app_lock SET holder = ?, pid = ?, acquired_at = ?, heartbeat_at = ? WHERE id = 1`,
			holder, pid, model.FormatTime(now), model.FormatTime(now))
		if err != nil {
			return fmt.Errorf("store: steal lease: %w", err)
		}
		return nil
	})
}

func (s *Store) touchLease(ctx context.Context, tx *sql.Tx, holder string, pid int, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE app_lock SET heartbeat_at = ?, pid = ? WHERE id = 1 AND holder = ?`,
		model.FormatTime(now), pid, holder)
	if err != nil {
		return fmt.Errorf("store: heartbeat: %w", err)
	}
	return nil
}

// RenewLease refreshes the heartbeat. It returns ErrLeaseLost when the row
// belongs to somebody else, which must be treated as fatal.
func (s *Store) RenewLease(ctx context.Context, holder string) error {
	now := s.clock.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE app_lock SET heartbeat_at = ? WHERE id = 1 AND holder = ?`,
		model.FormatTime(now), holder)
	if err != nil {
		return fmt.Errorf("store: renew lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: renew lease: %w", err)
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// LeaseHolder reports who currently owns the database.
func (s *Store) LeaseHolder(ctx context.Context) (string, time.Time, error) {
	var holder, heartbeat string
	err := s.db.QueryRowContext(ctx, `SELECT holder, heartbeat_at FROM app_lock WHERE id = 1`).
		Scan(&holder, &heartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store: read lease: %w", err)
	}
	t, _ := model.ParseTime(heartbeat)
	return holder, t, nil
}

// ReleaseLease drops the lease if we still hold it.
func (s *Store) ReleaseLease(ctx context.Context, holder string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_lock WHERE id = 1 AND holder = ?`, holder)
	if err != nil {
		return fmt.Errorf("store: release lease: %w", err)
	}
	return nil
}

// KeepLease renews the lease until ctx is done. onLost is called exactly once
// if the lease disappears; the caller is expected to shut the process down.
func (s *Store) KeepLease(ctx context.Context, holder string, every time.Duration, onLost func(error)) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.RenewLease(ctx, holder); err != nil {
				if onLost != nil {
					onLost(err)
				}
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// meta / telegram offset
// ---------------------------------------------------------------------------

// SetMeta stores a key/value pair.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, s.now())
	if err != nil {
		return fmt.Errorf("store: set meta %s: %w", key, err)
	}
	return nil
}

// GetMeta reads a key, returning "" and no error when it is absent.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get meta %s: %w", key, err)
	}
	return v, nil
}

// SaveTelegramOffset records the highest update id already dispatched.
func (s *Store) SaveTelegramOffset(ctx context.Context, updateID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO telegram_offset (id, last_update_id, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   last_update_id = CASE WHEN excluded.last_update_id > telegram_offset.last_update_id
		                         THEN excluded.last_update_id ELSE telegram_offset.last_update_id END,
		   updated_at = excluded.updated_at`,
		updateID, s.now())
	if err != nil {
		return fmt.Errorf("store: save telegram offset: %w", err)
	}
	return nil
}

// TelegramOffset returns the persisted high-water mark, 0 when unset.
func (s *Store) TelegramOffset(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT last_update_id FROM telegram_offset WHERE id = 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read telegram offset: %w", err)
	}
	return id, nil
}
