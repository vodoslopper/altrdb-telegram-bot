package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"altrdb-telegram-bot/internal/model"
)

const defaultRecentTaskMaxAge = 30 * 24 * time.Hour

// ApplyResult summarises one poll's effect on the database.
type ApplyResult struct {
	// Tasks is how many records the poll carried.
	Tasks int
	// Changed is how many fingerprints differed from the stored snapshot,
	// including tasks seen for the first time.
	Changed int
	// Baseline is true when the pass only recorded snapshots.
	Baseline bool
	// Notifications is how many outbox rows were inserted. The unique
	// constraint makes this 0 for anything already queued.
	Notifications int
	// Duplicates is how many inserts the unique constraint rejected.
	Duplicates int
	// Events are the task events that were queued this pass.
	Events []model.TaskEvent
}

// ApplyTasks records one poll result and enqueues notifications, in a single
// transaction.
//
// The transaction is the whole point: a snapshot is only ever saved together
// with the notifications it implies, so a crash cannot leave the database
// saying "already seen" for a task nobody was told about. The reverse is also
// impossible, because the outbox rows carry UNIQUE (chat_id, fingerprint).
//
// baseline suppresses notification creation. It is used for the first pass over
// a branch that has no stored snapshots, so a fresh install or a wiped snapshot
// table does not blast every chat with the current contents of the feed.
func (s *Store) ApplyTasks(ctx context.Context, tasks []model.Task, baseline bool, terminal model.TerminalStates) (ApplyResult, error) {
	res := ApplyResult{Tasks: len(tasks), Baseline: baseline}
	if len(tasks) == 0 {
		return res, nil
	}

	now := s.clock.Now()
	nowStr := model.FormatTime(now)

	states := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.State != "" {
			states = append(states, t.State)
		}
	}

	err := s.tx(ctx, func(tx *sql.Tx) error {
		if len(states) > 0 {
			if err := observeStatesTx(ctx, tx, states, nowStr); err != nil {
				return err
			}
		}

		existing, err := loadSnapshotsTx(ctx, tx, tasks)
		if err != nil {
			return err
		}

		upsert, err := tx.PrepareContext(ctx, `
			INSERT INTO task_snapshots (task_repo, task_id, fingerprint, task_state, task_owner,
			                            task_try, task_iter, task_testonly, task_stage, task_message,
			                            task_changed, packages, first_seen_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (task_repo, task_id) DO UPDATE SET
				fingerprint   = excluded.fingerprint,
				task_state    = excluded.task_state,
				task_owner    = excluded.task_owner,
				task_try      = excluded.task_try,
				task_iter     = excluded.task_iter,
				task_testonly = excluded.task_testonly,
				task_stage    = excluded.task_stage,
				task_message  = excluded.task_message,
				task_changed  = excluded.task_changed,
				packages      = excluded.packages,
				updated_at    = excluded.updated_at`)
		if err != nil {
			return fmt.Errorf("store: prepare snapshot upsert: %w", err)
		}
		defer upsert.Close()

		for _, t := range tasks {
			fp := model.Fingerprint(t)
			prev, seen := existing[t.TaskID]
			pkgs, err := json.Marshal(t.Packages)
			if err != nil {
				pkgs = []byte("[]")
			}
			firstSeen := nowStr
			if seen {
				firstSeen = model.FormatTime(prev.FirstSeenAt)
			}
			if _, err := upsert.ExecContext(ctx,
				t.TaskRepo, t.TaskID, fp, t.State, t.Owner, t.Try, t.Iter, t.Testonly,
				t.Stage, t.Message, model.FormatTime(t.Changed), string(pkgs), firstSeen, nowStr); err != nil {
				return fmt.Errorf("store: upsert snapshot %s/%d: %w", t.TaskRepo, t.TaskID, err)
			}

			if seen && prev.Fingerprint == fp {
				continue // nothing meaningful moved
			}
			res.Changed++
			if baseline {
				continue
			}

			event := model.TaskEvent{
				Fingerprint:   fp,
				TaskRepo:      t.TaskRepo,
				TaskID:        t.TaskID,
				State:         t.State,
				Owner:         t.Owner,
				Try:           t.Try,
				Iter:          t.Iter,
				Testonly:      t.Testonly,
				Stage:         t.Stage,
				Message:       t.Message,
				Changed:       t.Changed,
				Packages:      t.Packages,
				FirstObserved: !seen,
			}
			if seen {
				event.PreviousState = prev.State
			}

			inserted, dups, err := fanOutTx(ctx, tx, t, event, terminal, now)
			if err != nil {
				return err
			}
			if inserted > 0 {
				res.Notifications += inserted
				res.Events = append(res.Events, event)
			}
			res.Duplicates += dups
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// fanOutTx finds every chat whose subscription matches the task right now and
// queues one outbox row per chat.
func fanOutTx(ctx context.Context, tx *sql.Tx, t model.Task, event model.TaskEvent, terminal model.TerminalStates, now time.Time) (inserted, duplicates int, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.notify_mode, cb.active_watermark
		FROM chats c
		JOIN chat_branches cb ON cb.chat_id = c.id AND cb.branch = ?
		WHERE c.enabled = 1
		  AND EXISTS (SELECT 1 FROM chat_states cs WHERE cs.chat_id = c.id AND cs.state = ?)
		  AND EXISTS (SELECT 1 FROM chat_maintainers cm WHERE cm.chat_id = c.id AND cm.nickname = ?)`,
		t.TaskRepo, t.State, strings.ToLower(t.Owner))
	if err != nil {
		return 0, 0, fmt.Errorf("store: match chats for task %d: %w", t.TaskID, err)
	}
	type candidate struct {
		chatID    int64
		watermark time.Time
		mode      model.NotifyMode
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var mode, wm string
		if err := rows.Scan(&c.chatID, &mode, &wm); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scan chat match: %w", err)
		}
		c.mode = model.NotifyMode(mode)
		c.watermark, _ = model.ParseTime(wm)
		candidates = append(candidates, c)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return 0, 0, fmt.Errorf("store: match chats for task %d: %w", t.TaskID, rowsErr)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return 0, 0, fmt.Errorf("store: encode event payload: %w", err)
	}
	nowStr := model.FormatTime(now)

	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO notifications (chat_id, fingerprint, task_repo, task_id, payload,
		                           status, attempts, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, ?)
		ON CONFLICT (chat_id, fingerprint) DO NOTHING`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: prepare notification insert: %w", err)
	}
	defer ins.Close()

	for _, c := range candidates {
		// Activation watermark: never replay what was already in the feed when
		// the chat ticked this branch.
		if !t.Changed.After(c.watermark) {
			continue
		}
		if c.mode == model.NotifyTerminal && !terminal.Contains(strings.ToUpper(t.State)) {
			continue
		}
		out, err := ins.ExecContext(ctx, c.chatID, event.Fingerprint, t.TaskRepo, t.TaskID,
			string(payload), nowStr, nowStr)
		if err != nil {
			return inserted, duplicates, fmt.Errorf("store: queue notification for chat %d: %w", c.chatID, err)
		}
		if n, _ := out.RowsAffected(); n > 0 {
			inserted += int(n)
		} else {
			duplicates++
		}
	}
	return inserted, duplicates, nil
}

func loadSnapshotsTx(ctx context.Context, tx *sql.Tx, tasks []model.Task) (map[int64]model.Snapshot, error) {
	repo := tasks[0].TaskRepo
	ids := make([]int64, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.TaskID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make(map[int64]model.Snapshot, len(ids))
	// Chunk the IN list; SQLite's default variable limit is 999 in older
	// builds and the feed can carry a few hundred tasks.
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]
		q := fmt.Sprintf(`
			SELECT task_repo, task_id, fingerprint, task_state, task_owner, task_try, task_iter,
			       task_testonly, task_stage, task_message, task_changed, packages,
			       first_seen_at, updated_at
			FROM task_snapshots WHERE task_repo = ? AND task_id IN (%s)`, placeholders(len(batch)))
		args := make([]any, 0, len(batch)+1)
		args = append(args, repo)
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("store: load snapshots: %w", err)
		}
		for rows.Next() {
			var snap model.Snapshot
			var changed, pkgs, first, updated string
			if err := rows.Scan(&snap.TaskRepo, &snap.TaskID, &snap.Fingerprint, &snap.State,
				&snap.Owner, &snap.Try, &snap.Iter, &snap.Testonly, &snap.Stage, &snap.Message,
				&changed, &pkgs, &first, &updated); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan snapshot: %w", err)
			}
			snap.Changed, _ = model.ParseTime(changed)
			snap.FirstSeenAt, _ = model.ParseTime(first)
			snap.UpdatedAt, _ = model.ParseTime(updated)
			if pkgs != "" {
				_ = json.Unmarshal([]byte(pkgs), &snap.Packages)
			}
			out[snap.TaskID] = snap
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("store: load snapshots: %w", err)
		}
	}
	return out, nil
}

func observeStatesTx(ctx context.Context, tx *sql.Tx, states []string, now string) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO rdb_states (state, known, sort_rank, first_seen_at, last_seen_at)
		VALUES (?, 0, 1000, ?, ?)
		ON CONFLICT (state) DO UPDATE SET last_seen_at = excluded.last_seen_at`)
	if err != nil {
		return fmt.Errorf("store: prepare state observe: %w", err)
	}
	defer stmt.Close()
	for _, st := range states {
		if _, err := stmt.ExecContext(ctx, st, now, now); err != nil {
			return fmt.Errorf("store: observe state %s: %w", st, err)
		}
	}
	return nil
}

// NeedsBaseline reports whether a branch has no stored snapshots yet. The first
// successful poll of such a branch must not notify: everything in it is history
// from this database's point of view.
func (s *Store) NeedsBaseline(ctx context.Context, branch string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_snapshots WHERE task_repo = ?`, branch).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: count snapshots %s: %w", branch, err)
	}
	return n == 0, nil
}

// GetSnapshot reads the stored snapshot of one task.
func (s *Store) GetSnapshot(ctx context.Context, repo string, taskID int64) (*model.Snapshot, error) {
	var snap model.Snapshot
	var changed, pkgs, first, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT task_repo, task_id, fingerprint, task_state, task_owner, task_try, task_iter,
		       task_testonly, task_stage, task_message, task_changed, packages,
		       first_seen_at, updated_at
		FROM task_snapshots WHERE task_repo = ? AND task_id = ?`, repo, taskID).
		Scan(&snap.TaskRepo, &snap.TaskID, &snap.Fingerprint, &snap.State, &snap.Owner,
			&snap.Try, &snap.Iter, &snap.Testonly, &snap.Stage, &snap.Message,
			&changed, &pkgs, &first, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get snapshot %s/%d: %w", repo, taskID, err)
	}
	snap.Changed, _ = model.ParseTime(changed)
	snap.FirstSeenAt, _ = model.ParseTime(first)
	snap.UpdatedAt, _ = model.ParseTime(updated)
	if pkgs != "" {
		_ = json.Unmarshal([]byte(pkgs), &snap.Packages)
	}
	return &snap, nil
}

// CountSnapshots returns how many tasks are being tracked.
func (s *Store) CountSnapshots(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_snapshots`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count snapshots: %w", err)
	}
	return n, nil
}

// PruneSnapshots drops tasks that have not changed since cutoff. They are long
// gone from the capped feed, so keeping them only grows the file.
func (s *Store) PruneSnapshots(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM task_snapshots WHERE task_changed < ?`,
		model.FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: prune snapshots: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RecentMatching returns the currently matching tasks changed within maxAge
// for /recent. It reads the local snapshot table, so it never touches RDB and
// never moves a watermark.
func (s *Store) RecentMatching(ctx context.Context, chatID int64, limit int, maxAge time.Duration) ([]model.TaskEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	if maxAge <= 0 {
		maxAge = defaultRecentTaskMaxAge
	}
	sub, err := s.LoadSubscription(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if !sub.Complete() {
		return nil, nil
	}

	q := `SELECT task_repo, task_id, fingerprint, task_state, task_owner, task_try, task_iter,
	             task_testonly, task_stage, task_message, task_changed, packages
	      FROM task_snapshots
	      WHERE task_owner IN (` + placeholders(len(sub.Maintainers)) + `)
	        AND task_state IN (` + placeholders(len(sub.States)) + `)
	        AND task_repo IN (` + placeholders(len(sub.Branches)) + `)
	        AND task_changed >= ?
	      ORDER BY task_changed DESC
	      LIMIT ?`

	args := make([]any, 0, len(sub.Maintainers)+len(sub.States)+len(sub.Branches)+2)
	for _, m := range sub.Maintainers {
		args = append(args, m)
	}
	for _, st := range sub.States {
		args = append(args, st)
	}
	for _, b := range sub.Branches {
		args = append(args, b.Branch)
	}
	args = append(args, model.FormatTime(s.clock.Now().Add(-maxAge)))
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: recent matching: %w", err)
	}
	defer rows.Close()

	var out []model.TaskEvent
	for rows.Next() {
		var repo, fp, state, owner, stage, message, changed, pkgs string
		var id int64
		var try, iter, testonly int
		if err := rows.Scan(&repo, &id, &fp, &state, &owner, &try, &iter, &testonly,
			&stage, &message, &changed, &pkgs); err != nil {
			return nil, fmt.Errorf("store: scan recent task: %w", err)
		}
		ev := model.TaskEvent{
			Fingerprint: fp, TaskRepo: repo, TaskID: id, State: state, Owner: owner,
			Try: try, Iter: iter, Testonly: testonly, Stage: stage, Message: message,
		}
		ev.Changed, _ = model.ParseTime(changed)
		if pkgs != "" {
			_ = json.Unmarshal([]byte(pkgs), &ev.Packages)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
