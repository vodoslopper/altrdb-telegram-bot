package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"altrdb-telegram-bot/internal/model"
)

// ---------------------------------------------------------------------------
// branch catalogue
// ---------------------------------------------------------------------------

// ReplaceBranches upserts the branch list reported by RDB. Branches that are no
// longer reported are kept: a chat may still be subscribed to one, and dropping
// the row would silently change the meaning of that subscription.
func (s *Store) ReplaceBranches(ctx context.Context, branches []string) error {
	if len(branches) == 0 {
		return nil
	}
	now := s.now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO rdb_branches (branch, sort_order, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (branch) DO UPDATE SET
				sort_order   = excluded.sort_order,
				last_seen_at = excluded.last_seen_at`)
		if err != nil {
			return fmt.Errorf("store: prepare branch upsert: %w", err)
		}
		defer stmt.Close()
		for i, b := range branches {
			b = strings.TrimSpace(b)
			if b == "" {
				continue
			}
			if _, err := stmt.ExecContext(ctx, b, i, now, now); err != nil {
				return fmt.Errorf("store: upsert branch %s: %w", b, err)
			}
		}
		return nil
	})
}

// ListBranches returns the cached branch list in the API's own order.
func (s *Store) ListBranches(ctx context.Context) ([]model.Branch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, branch, sort_order, first_seen_at, last_seen_at
		 FROM rdb_branches ORDER BY sort_order, branch`)
	if err != nil {
		return nil, fmt.Errorf("store: list branches: %w", err)
	}
	defer rows.Close()

	var out []model.Branch
	for rows.Next() {
		var b model.Branch
		var first, last string
		if err := rows.Scan(&b.ID, &b.Name, &b.SortOrder, &first, &last); err != nil {
			return nil, fmt.Errorf("store: scan branch: %w", err)
		}
		b.FirstSeen, _ = model.ParseTime(first)
		b.LastSeen, _ = model.ParseTime(last)
		out = append(out, b)
	}
	return out, rows.Err()
}

// BranchNames returns just the cached branch names.
func (s *Store) BranchNames(ctx context.Context) ([]string, error) {
	bs, err := s.ListBranches(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out, nil
}

// BranchID resolves a branch name to its surrogate id, used to keep callback
// data short and free of raw values.
func (s *Store) BranchID(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM rdb_branches WHERE branch = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: branch id %s: %w", name, err)
	}
	return id, nil
}

// BranchName resolves a surrogate id back to a branch name.
func (s *Store) BranchName(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT branch FROM rdb_branches WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: branch name %d: %w", id, err)
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// task state catalogue
// ---------------------------------------------------------------------------

// ObserveStates records states seen in a live response. New ones become
// selectable immediately; that is how the bot learns about states added after
// it was built.
func (s *Store) ObserveStates(ctx context.Context, states []string) error {
	clean := make([]string, 0, len(states))
	for _, st := range states {
		if st = strings.TrimSpace(st); st != "" {
			clean = append(clean, st)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	now := s.now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO rdb_states (state, known, sort_rank, first_seen_at, last_seen_at)
			VALUES (?, 0, 1000, ?, ?)
			ON CONFLICT (state) DO UPDATE SET last_seen_at = excluded.last_seen_at`)
		if err != nil {
			return fmt.Errorf("store: prepare state upsert: %w", err)
		}
		defer stmt.Close()
		for _, st := range clean {
			if _, err := stmt.ExecContext(ctx, st, now, now); err != nil {
				return fmt.Errorf("store: observe state %s: %w", st, err)
			}
		}
		return nil
	})
}

// ListStates returns the full catalogue: seeded lifecycle states first in
// lifecycle order, learned states alphabetically after them.
func (s *Store) ListStates(ctx context.Context) ([]model.State, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, state, known, sort_rank, first_seen_at, last_seen_at
		 FROM rdb_states ORDER BY sort_rank, state`)
	if err != nil {
		return nil, fmt.Errorf("store: list states: %w", err)
	}
	defer rows.Close()

	var out []model.State
	for rows.Next() {
		var st model.State
		var known int
		var first, last string
		if err := rows.Scan(&st.ID, &st.Name, &known, &st.SortRank, &first, &last); err != nil {
			return nil, fmt.Errorf("store: scan state: %w", err)
		}
		st.Known = known != 0
		st.FirstSeen, _ = model.ParseTime(first)
		st.LastSeen, _ = model.ParseTime(last)
		out = append(out, st)
	}
	return out, rows.Err()
}

// StateNames returns just the ordered state names.
func (s *Store) StateNames(ctx context.Context) ([]string, error) {
	states, err := s.ListStates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(states))
	for _, st := range states {
		out = append(out, st.Name)
	}
	return out, nil
}

// StateID resolves a state name to its surrogate id.
func (s *Store) StateID(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM rdb_states WHERE state = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: state id %s: %w", name, err)
	}
	return id, nil
}

// StateName resolves a surrogate id back to a state name.
func (s *Store) StateName(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM rdb_states WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: state name %d: %w", id, err)
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// maintainer directory cache
// ---------------------------------------------------------------------------

// ReplaceMaintainersCache stores one branch's directory. The cache is what
// lets the bot validate a nickname and offer suggestions without calling RDB
// on every keystroke.
func (s *Store) ReplaceMaintainersCache(ctx context.Context, branch string, maintainers []model.Maintainer) error {
	if branch == "" {
		return errors.New("store: empty branch")
	}
	now := s.now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM maintainers_cache WHERE branch = ?`, branch); err != nil {
			return fmt.Errorf("store: clear maintainer cache %s: %w", branch, err)
		}
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO maintainers_cache (branch, nickname, packager_name, count_source_pkg, fetched_at)
			VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("store: prepare maintainer insert: %w", err)
		}
		defer stmt.Close()
		for _, m := range maintainers {
			if m.Nickname == "" {
				continue
			}
			if _, err := stmt.ExecContext(ctx, branch, m.Nickname, m.PackagerName, m.CountSourcePkg, now); err != nil {
				return fmt.Errorf("store: insert maintainer %s: %w", m.Nickname, err)
			}
		}
		return nil
	})
}

// MaintainerCacheAge reports when a branch's directory was last fetched. The
// second result is false when the branch has never been cached: MAX over an
// empty set is NULL, which must not be mistaken for a parse failure.
func (s *Store) MaintainerCacheAge(ctx context.Context, branch string) (time.Time, bool, error) {
	var fetched sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(fetched_at) FROM maintainers_cache WHERE branch = ?`, branch).Scan(&fetched)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: maintainer cache age: %w", err)
	}
	if !fetched.Valid || fetched.String == "" {
		return time.Time{}, false, nil
	}
	t, err := model.ParseTime(fetched.String)
	if err != nil {
		return time.Time{}, false, nil
	}
	return t, true, nil
}

// FindMaintainer checks a nickname against the cached directories of the given
// branches (or of every branch when the list is empty). It reports the display
// name and which branches know the nickname.
func (s *Store) FindMaintainer(ctx context.Context, nickname string, branches []string) (found bool, name string, matched []string, err error) {
	q := `SELECT packager_name, branch FROM maintainers_cache WHERE nickname = ?`
	args := []any{nickname}
	if len(branches) > 0 {
		q += fmt.Sprintf(` AND branch IN (%s)`, placeholders(len(branches)))
		for _, b := range branches {
			args = append(args, b)
		}
	}
	q += ` ORDER BY branch`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return false, "", nil, fmt.Errorf("store: find maintainer %s: %w", nickname, err)
	}
	defer rows.Close()
	for rows.Next() {
		var packager, branch string
		if err := rows.Scan(&packager, &branch); err != nil {
			return false, "", nil, fmt.Errorf("store: scan maintainer: %w", err)
		}
		if name == "" {
			name = packager
		}
		matched = append(matched, branch)
		found = true
	}
	return found, name, matched, rows.Err()
}

// SuggestMaintainers returns cached nicknames starting with prefix, used for
// "did you mean" buttons after a rejected or unknown nickname.
func (s *Store) SuggestMaintainers(ctx context.Context, prefix string, limit int) ([]model.Maintainer, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT nickname, MAX(packager_name) AS packager_name, SUM(count_source_pkg) AS pkgs
		FROM maintainers_cache
		WHERE nickname LIKE ? ESCAPE '\'
		GROUP BY nickname
		ORDER BY pkgs DESC, nickname
		LIMIT ?`, escapeLike(prefix)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("store: suggest maintainers: %w", err)
	}
	defer rows.Close()

	var out []model.Maintainer
	for rows.Next() {
		var m model.Maintainer
		if err := rows.Scan(&m.Nickname, &m.PackagerName, &m.CountSourcePkg); err != nil {
			return nil, fmt.Errorf("store: scan suggestion: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// placeholders renders "?,?,?" for n values.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// escapeLike neutralises the LIKE wildcards in user input.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
