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

// RegisterChat creates the chat row on first contact (or refreshes it) and
// clears any previous disable reason: an interactive /start is the operator's
// way back in after unblocking the bot.
func (s *Store) RegisterChat(ctx context.Context, id int64, chatType, username, firstName string) (*model.Chat, error) {
	now := s.clock.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (id, chat_type, username, first_name, enabled, notify_mode,
		                   disabled_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, '', ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			chat_type       = excluded.chat_type,
			username        = excluded.username,
			first_name      = excluded.first_name,
			enabled         = 1,
			disabled_reason = '',
			updated_at      = excluded.updated_at`,
		id, chatType, username, firstName, model.NotifyAll,
		model.FormatTime(now), model.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("store: register chat %d: %w", id, err)
	}
	return s.GetChat(ctx, id)
}

// UpdateChatIdentity refreshes the cached name/username without touching the
// subscription or the enabled flag. Missing chats are created.
func (s *Store) UpdateChatIdentity(ctx context.Context, id int64, chatType, username, firstName string) error {
	now := s.clock.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (id, chat_type, username, first_name, enabled, notify_mode,
		                   disabled_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, '', ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			chat_type  = excluded.chat_type,
			username   = excluded.username,
			first_name = excluded.first_name,
			updated_at = excluded.updated_at`,
		id, chatType, username, firstName, model.NotifyAll,
		model.FormatTime(now), model.FormatTime(now))
	if err != nil {
		return fmt.Errorf("store: touch chat %d: %w", id, err)
	}
	return nil
}

// GetChat loads one chat.
func (s *Store) GetChat(ctx context.Context, id int64) (*model.Chat, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, chat_type, username, first_name, enabled, notify_mode,
		       disabled_reason, created_at, updated_at
		FROM chats WHERE id = ?`, id)

	var c model.Chat
	var enabled int
	var mode, reason, created, updated string
	err := row.Scan(&c.ID, &c.Type, &c.Username, &c.FirstName, &enabled, &mode, &reason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat %d: %w", id, err)
	}
	c.Enabled = enabled != 0
	c.NotifyMode = model.NotifyMode(mode)
	if !c.NotifyMode.Valid() {
		c.NotifyMode = model.NotifyAll
	}
	c.DisabledReason = reason
	c.CreatedAt, _ = model.ParseTime(created)
	c.UpdatedAt, _ = model.ParseTime(updated)
	return &c, nil
}

// SetChatEnabled flips delivery for a chat (/pause, /resume).
func (s *Store) SetChatEnabled(ctx context.Context, id int64, enabled bool, reason string) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE chats SET enabled = ?, disabled_reason = ?, updated_at = ? WHERE id = ?`,
		e, reason, s.now(), id)
	if err != nil {
		return fmt.Errorf("store: set chat %d enabled=%t: %w", id, enabled, err)
	}
	return nil
}

// DisableChat turns a chat off and records why. Used when Telegram reports the
// bot was blocked or the chat no longer exists.
func (s *Store) DisableChat(ctx context.Context, id int64, reason string) error {
	return s.SetChatEnabled(ctx, id, false, truncate(reason, 512))
}

// SetNotifyMode switches between "every transition" and "terminal states only".
func (s *Store) SetNotifyMode(ctx context.Context, id int64, mode model.NotifyMode) error {
	if !mode.Valid() {
		return fmt.Errorf("store: invalid notify mode %q", mode)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE chats SET notify_mode = ?, updated_at = ? WHERE id = ?`, mode, s.now(), id)
	if err != nil {
		return fmt.Errorf("store: set notify mode for %d: %w", id, err)
	}
	return nil
}

// LoadSubscription returns everything a chat has selected.
func (s *Store) LoadSubscription(ctx context.Context, chatID int64) (*model.Subscription, error) {
	sub := &model.Subscription{}

	mrows, err := s.db.QueryContext(ctx,
		`SELECT nickname FROM chat_maintainers WHERE chat_id = ? ORDER BY nickname`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: load maintainers: %w", err)
	}
	for mrows.Next() {
		var n string
		if err := mrows.Scan(&n); err != nil {
			mrows.Close()
			return nil, fmt.Errorf("store: scan maintainer: %w", err)
		}
		sub.Maintainers = append(sub.Maintainers, n)
	}
	err = mrows.Err()
	mrows.Close()
	if err != nil {
		return nil, fmt.Errorf("store: load maintainers: %w", err)
	}

	srows, err := s.db.QueryContext(ctx, `SELECT state FROM chat_states WHERE chat_id = ?`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: load states: %w", err)
	}
	for srows.Next() {
		var st string
		if err := srows.Scan(&st); err != nil {
			srows.Close()
			return nil, fmt.Errorf("store: scan state: %w", err)
		}
		sub.States = append(sub.States, st)
	}
	err = srows.Err()
	srows.Close()
	if err != nil {
		return nil, fmt.Errorf("store: load states: %w", err)
	}
	model.SortStates(sub.States)

	brows, err := s.db.QueryContext(ctx,
		`SELECT branch, active_watermark, created_at FROM chat_branches WHERE chat_id = ? ORDER BY branch`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: load branches: %w", err)
	}
	defer brows.Close()
	for brows.Next() {
		var b model.BranchSubscription
		var wm, created string
		if err := brows.Scan(&b.Branch, &wm, &created); err != nil {
			return nil, fmt.Errorf("store: scan branch: %w", err)
		}
		b.ActiveWatermark, _ = model.ParseTime(wm)
		b.CreatedAt, _ = model.ParseTime(created)
		sub.Branches = append(sub.Branches, b)
	}
	if err := brows.Err(); err != nil {
		return nil, fmt.Errorf("store: load branches: %w", err)
	}
	return sub, nil
}

// AddMaintainers inserts nicknames, skipping ones already present, and reports
// which were new. Input is expected to be normalised already.
func (s *Store) AddMaintainers(ctx context.Context, chatID int64, nicknames []string) ([]string, error) {
	if len(nicknames) == 0 {
		return nil, nil
	}
	now := s.now()
	var added []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chat_maintainers WHERE chat_id = ?`, chatID).Scan(&count); err != nil {
			return fmt.Errorf("store: count maintainers: %w", err)
		}
		if count+len(nicknames) > model.MaxNicknamesPerChat {
			return fmt.Errorf("store: a chat may follow at most %d maintainers (already %d)",
				model.MaxNicknamesPerChat, count)
		}
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO chat_maintainers (chat_id, nickname, created_at) VALUES (?, ?, ?)
			ON CONFLICT (chat_id, nickname) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("store: prepare add maintainer: %w", err)
		}
		defer stmt.Close()
		for _, n := range nicknames {
			res, err := stmt.ExecContext(ctx, chatID, n, now)
			if err != nil {
				return fmt.Errorf("store: add maintainer %s: %w", n, err)
			}
			if affected, _ := res.RowsAffected(); affected > 0 {
				added = append(added, n)
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE chats SET updated_at = ? WHERE id = ?`, now, chatID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// RemoveMaintainer deletes one nickname. It reports whether a row went away.
func (s *Store) RemoveMaintainer(ctx context.Context, chatID int64, nickname string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM chat_maintainers WHERE chat_id = ? AND nickname = ?`, chatID, nickname)
	if err != nil {
		return false, fmt.Errorf("store: remove maintainer %s: %w", nickname, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RemoveMaintainerByIndex deletes the n-th nickname of the sorted list. The
// caller passes a list hash alongside so a stale button is detected first.
func (s *Store) RemoveMaintainerByIndex(ctx context.Context, chatID int64, index int) (string, error) {
	list, err := s.ListMaintainers(ctx, chatID)
	if err != nil {
		return "", err
	}
	if index < 0 || index >= len(list) {
		return "", fmt.Errorf("%w: maintainer index %d", ErrNotFound, index)
	}
	if _, err := s.RemoveMaintainer(ctx, chatID, list[index]); err != nil {
		return "", err
	}
	return list[index], nil
}

// ClearMaintainers removes every nickname of a chat.
func (s *Store) ClearMaintainers(ctx context.Context, chatID int64) (int, error) {
	return s.clearChild(ctx, `DELETE FROM chat_maintainers WHERE chat_id = ?`, chatID)
}

// ListMaintainers returns the sorted nickname list.
func (s *Store) ListMaintainers(ctx context.Context, chatID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT nickname FROM chat_maintainers WHERE chat_id = ? ORDER BY nickname`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: list maintainers: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: scan maintainer: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListChatStates returns the checked states in display order.
func (s *Store) ListChatStates(ctx context.Context, chatID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state FROM chat_states WHERE chat_id = ?`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: list chat states: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return nil, fmt.Errorf("store: scan chat state: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list chat states: %w", err)
	}
	model.SortStates(out)
	return out, nil
}

// ToggleChatState flips one state and reports the new value.
func (s *Store) ToggleChatState(ctx context.Context, chatID int64, state string) (bool, error) {
	if state == "" {
		return false, errors.New("store: empty state")
	}
	var on bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM chat_states WHERE chat_id = ? AND state = ?`, chatID, state).Scan(&exists); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: probe chat state: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chat_states (chat_id, state, created_at) VALUES (?, ?, ?)`,
				chatID, state, s.now()); err != nil {
				return fmt.Errorf("store: check state: %w", err)
			}
			on = true
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM chat_states WHERE chat_id = ? AND state = ?`, chatID, state); err != nil {
			return fmt.Errorf("store: uncheck state: %w", err)
		}
		on = false
		return nil
	})
	return on, err
}

// SetAllChatStates replaces the state selection, used by Select all / Clear all.
func (s *Store) SetAllChatStates(ctx context.Context, chatID int64, states []string) error {
	now := s.now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_states WHERE chat_id = ?`, chatID); err != nil {
			return fmt.Errorf("store: clear chat states: %w", err)
		}
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO chat_states (chat_id, state, created_at) VALUES (?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("store: prepare state insert: %w", err)
		}
		defer stmt.Close()
		for _, st := range states {
			if st == "" {
				continue
			}
			if _, err := stmt.ExecContext(ctx, chatID, st, now); err != nil {
				return fmt.Errorf("store: insert state %s: %w", st, err)
			}
		}
		return nil
	})
}

// ListChatBranches returns the checked branches with their watermarks.
func (s *Store) ListChatBranches(ctx context.Context, chatID int64) ([]model.BranchSubscription, error) {
	sub, err := s.LoadSubscription(ctx, chatID)
	if err != nil {
		return nil, err
	}
	return sub.Branches, nil
}

// ToggleChatBranch flips one branch. Turning a branch on stamps a fresh
// activation watermark so the tasks already sitting in the feed are not
// replayed; turning it off and on again deliberately re-stamps.
func (s *Store) ToggleChatBranch(ctx context.Context, chatID int64, branch string) (bool, time.Time, error) {
	if branch == "" {
		return false, time.Time{}, errors.New("store: empty branch")
	}
	now := s.clock.Now()
	var on bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var wm string
		err := tx.QueryRowContext(ctx,
			`SELECT active_watermark FROM chat_branches WHERE chat_id = ? AND branch = ?`,
			chatID, branch).Scan(&wm)
		switch {
		case err == nil:
			if _, derr := tx.ExecContext(ctx,
				`DELETE FROM chat_branches WHERE chat_id = ? AND branch = ?`, chatID, branch); derr != nil {
				return fmt.Errorf("store: uncheck branch: %w", derr)
			}
			on = false
			return nil
		case errors.Is(err, sql.ErrNoRows):
			if _, ierr := tx.ExecContext(ctx,
				`INSERT INTO chat_branches (chat_id, branch, active_watermark, created_at) VALUES (?, ?, ?, ?)`,
				chatID, branch, model.FormatTime(now), model.FormatTime(now)); ierr != nil {
				return fmt.Errorf("store: check branch: %w", ierr)
			}
			on = true
			return nil
		default:
			return fmt.Errorf("store: probe chat branch: %w", err)
		}
	})
	if err != nil {
		return false, time.Time{}, err
	}
	if on {
		return true, now, nil
	}
	return false, time.Time{}, nil
}

// AddChatBranch checks a branch without disturbing an existing watermark.
func (s *Store) AddChatBranch(ctx context.Context, chatID int64, branch string) (time.Time, error) {
	now := s.clock.Now()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_branches (chat_id, branch, active_watermark, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (chat_id, branch) DO NOTHING`,
		chatID, branch, model.FormatTime(now), model.FormatTime(now))
	if err != nil {
		return time.Time{}, fmt.Errorf("store: add branch %s: %w", branch, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already checked: keep the watermark that is already there.
		var wm string
		if err := s.db.QueryRowContext(ctx,
			`SELECT active_watermark FROM chat_branches WHERE chat_id = ? AND branch = ?`,
			chatID, branch).Scan(&wm); err != nil {
			return time.Time{}, fmt.Errorf("store: read watermark: %w", err)
		}
		t, _ := model.ParseTime(wm)
		return t, nil
	}
	return now, nil
}

// SetAllChatBranches implements Select all / Clear all for branches. Already
// checked branches keep their watermark; newly checked ones get "now".
func (s *Store) SetAllChatBranches(ctx context.Context, chatID int64, branches []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if len(branches) == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM chat_branches WHERE chat_id = ?`, chatID); err != nil {
				return fmt.Errorf("store: clear branches: %w", err)
			}
			return nil
		}
		for _, b := range branches {
			if b == "" {
				continue
			}
			now := model.FormatTime(s.clock.Now())
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO chat_branches (chat_id, branch, active_watermark, created_at)
				VALUES (?, ?, ?, ?) ON CONFLICT (chat_id, branch) DO NOTHING`,
				chatID, b, now, now); err != nil {
				return fmt.Errorf("store: check branch %s: %w", b, err)
			}
		}
		return nil
	})
}

// ClearChatBranches unchecks every branch.
func (s *Store) ClearChatBranches(ctx context.Context, chatID int64) (int, error) {
	return s.clearChild(ctx, `DELETE FROM chat_branches WHERE chat_id = ?`, chatID)
}

// ActiveBranches lists the distinct branches that at least one enabled chat
// follows. The poller polls exactly this set: one fetch per branch serves every
// subscriber, no matter how many users picked it.
func (s *Store) ActiveBranches(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT cb.branch
		FROM chat_branches cb
		JOIN chats c ON c.id = cb.chat_id
		WHERE c.enabled = 1
		ORDER BY cb.branch`)
	if err != nil {
		return nil, fmt.Errorf("store: active branches: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("store: scan branch: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CountChats summarises the chat table for /healthz.
func (s *Store) CountChats(ctx context.Context) (total, enabled, complete int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats`).Scan(&total)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: count chats: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats WHERE enabled = 1`).Scan(&enabled)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: count enabled chats: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM chats c
		WHERE c.enabled = 1
		  AND EXISTS (SELECT 1 FROM chat_maintainers m WHERE m.chat_id = c.id)
		  AND EXISTS (SELECT 1 FROM chat_states s WHERE s.chat_id = c.id)
		  AND EXISTS (SELECT 1 FROM chat_branches b WHERE b.chat_id = c.id)`).Scan(&complete)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: count complete chats: %w", err)
	}
	return total, enabled, complete, nil
}

// ---------------------------------------------------------------------------
// conversation state
// ---------------------------------------------------------------------------

// GetConversation returns the pending multi-step state, or nil.
func (s *Store) GetConversation(ctx context.Context, chatID int64) (*model.Conversation, error) {
	var step, payload, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT step, payload, updated_at FROM conversation_state WHERE chat_id = ?`, chatID).
		Scan(&step, &payload, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get conversation %d: %w", chatID, err)
	}
	c := &model.Conversation{ChatID: chatID, Step: model.ConversationStep(step), Payload: payload}
	c.UpdatedAt, _ = model.ParseTime(updated)
	return c, nil
}

// SetConversation records the pending step, creating the chat row if a
// callback arrived before /start somehow.
func (s *Store) SetConversation(ctx context.Context, chatID int64, step model.ConversationStep, payload string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_state (chat_id, step, payload, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (chat_id) DO UPDATE SET step = excluded.step, payload = excluded.payload,
			                                    updated_at = excluded.updated_at`,
			chatID, string(step), payload, s.now()); err != nil {
			return fmt.Errorf("store: set conversation %d: %w", chatID, err)
		}
		return nil
	})
}

// ClearConversation ends the multi-step flow.
func (s *Store) ClearConversation(ctx context.Context, chatID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM conversation_state WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("store: clear conversation %d: %w", chatID, err)
	}
	return nil
}

func (s *Store) clearChild(ctx context.Context, query string, chatID int64) (int, error) {
	res, err := s.db.ExecContext(ctx, query, chatID)
	if err != nil {
		return 0, fmt.Errorf("store: clear rows for chat %d: %w", chatID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}
