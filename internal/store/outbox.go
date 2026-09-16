package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"altrdb-telegram-bot/internal/model"
)

// CancelPendingNotifications gives up on everything still queued for a chat.
// It is called when Telegram says the chat is unreachable: keeping the rows
// would only produce a stream of failures against a dead endpoint.
func (s *Store) CancelPendingNotifications(ctx context.Context, chatID int64, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET status = 'dead', last_error = ?
		 WHERE chat_id = ? AND status IN ('pending', 'sending')`,
		truncate(reason, 512), chatID)
	if err != nil {
		return 0, fmt.Errorf("store: cancel notifications for chat %d: %w", chatID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ClaimDueNotifications atomically moves up to limit due pending rows to
// "sending" and returns them.
//
// The whole claim happens inside one IMMEDIATE transaction, so two delivery
// workers can never be handed the same row.
//
// Claiming does not consume an attempt. Attempts are only charged for real
// transient failures, so a Telegram 429 flow-control answer cannot exhaust the
// retry budget of a perfectly good notification. A crash between claim and
// result is charged one attempt by RecoverSendingNotifications instead, which
// is what keeps a poison message from looping forever.
func (s *Store) ClaimDueNotifications(ctx context.Context, limit int) ([]model.Notification, error) {
	if limit <= 0 {
		limit = 20
	}
	now := s.clock.Now()
	nowStr := model.FormatTime(now)

	var claimed []model.Notification
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// A paused or blocked chat is skipped here rather than failed at send
		// time: its rows stay pending so /resume can pick them up again.
		rows, err := tx.QueryContext(ctx, `
			SELECT n.id, n.chat_id, n.fingerprint, n.task_repo, n.task_id, n.payload, n.status,
			       n.attempts, n.next_attempt_at, n.last_error, n.created_at, n.sent_at
			FROM notifications n
			JOIN chats c ON c.id = n.chat_id
			WHERE n.status = 'pending' AND n.next_attempt_at <= ? AND c.enabled = 1
			ORDER BY n.next_attempt_at, n.id
			LIMIT ?`, nowStr, limit)
		if err != nil {
			return fmt.Errorf("store: select due notifications: %w", err)
		}
		for rows.Next() {
			n, err := scanNotification(rows)
			if err != nil {
				rows.Close()
				return err
			}
			claimed = append(claimed, *n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("store: select due notifications: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}

		ids := make([]any, 0, len(claimed))
		for _, n := range claimed {
			ids = append(ids, n.ID)
		}
		q := fmt.Sprintf(`UPDATE notifications
		                  SET status = 'sending', next_attempt_at = ?
		                  WHERE id IN (%s)`, placeholders(len(ids)))
		if _, err := tx.ExecContext(ctx, q, append([]any{nowStr}, ids...)...); err != nil {
			return fmt.Errorf("store: claim notifications: %w", err)
		}
		for i := range claimed {
			claimed[i].Status = model.OutboxSending
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// MarkNotificationSent records a successful delivery.
func (s *Store) MarkNotificationSent(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET status = 'sent', sent_at = ?, last_error = '' WHERE id = ?`,
		s.now(), id); err != nil {
		return fmt.Errorf("store: mark notification %d sent: %w", id, err)
	}
	return nil
}

// RetryNotification puts a row back to pending with a new due time. charge
// decides whether this costs one of its attempts: true for a genuine transient
// failure, false for Telegram flow control (429 with retry_after), which is not
// the notification's fault.
func (s *Store) RetryNotification(ctx context.Context, id int64, lastErr string, nextAttempt time.Time, charge bool) error {
	q := `UPDATE notifications SET status = 'pending', last_error = ?, next_attempt_at = ?`
	if charge {
		q += `, attempts = attempts + 1`
	}
	q += ` WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q,
		truncate(lastErr, 512), formatDeadline(nextAttempt), id); err != nil {
		return fmt.Errorf("store: retry notification %d: %w", id, err)
	}
	return nil
}

// MarkNotificationDead gives up on a row: too many attempts, or Telegram said
// the chat is gone. The attempt that produced the verdict is counted, so
// attempts always equals the number of deliveries actually tried.
func (s *Store) MarkNotificationDead(ctx context.Context, id int64, lastErr string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET status = 'dead', last_error = ?, attempts = attempts + 1 WHERE id = ?`,
		truncate(lastErr, 512), id); err != nil {
		return fmt.Errorf("store: mark notification %d dead: %w", id, err)
	}
	return nil
}

// RecoverSendingNotifications returns rows stuck in "sending" after an
// unclean exit to the pending queue. Called once at startup, before the
// delivery worker begins.
func (s *Store) RecoverSendingNotifications(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET status = 'pending', next_attempt_at = ?, attempts = attempts + 1
		 WHERE status = 'sending'`,
		s.now())
	if err != nil {
		return 0, fmt.Errorf("store: recover sending notifications: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// OutboxCounts summarises the queue for /healthz and /metrics.
func (s *Store) OutboxCounts(ctx context.Context) (model.OutboxHealth, error) {
	var h model.OutboxHealth
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*), MIN(created_at) FROM notifications GROUP BY status`)
	if err != nil {
		return h, fmt.Errorf("store: outbox counts: %w", err)
	}
	defer rows.Close()

	var oldestPending string
	for rows.Next() {
		var status, created string
		var count int
		if err := rows.Scan(&status, &count, &created); err != nil {
			return h, fmt.Errorf("store: scan outbox count: %w", err)
		}
		switch model.OutboxStatus(status) {
		case model.OutboxPending:
			h.Pending = count
			oldestPending = created
		case model.OutboxSending:
			h.Sending = count
		case model.OutboxDead:
			h.Dead = count
		}
	}
	if err := rows.Err(); err != nil {
		return h, fmt.Errorf("store: outbox counts: %w", err)
	}
	if h.Pending > 0 && oldestPending != "" {
		if t, err := model.ParseTime(oldestPending); err == nil {
			age := s.clock.Now().Sub(t)
			if age > 0 {
				h.OldestAge = int64(age.Seconds())
			}
		}
	}
	return h, nil
}

// PruneSentNotifications deletes delivered rows older than cutoff. Sent rows
// are kept for a while because they are the audit trail that proves an event
// was delivered exactly once.
func (s *Store) PruneSentNotifications(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM notifications WHERE status IN ('sent','dead') AND created_at < ?`,
		model.FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: prune notifications: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// EnqueueTestNotification puts a synthetic event on the outbox. /test uses the
// direct path instead; this exists so the retry machinery can be exercised
// against a real row.
func (s *Store) EnqueueTestNotification(ctx context.Context, chatID int64, ev model.TaskEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("store: encode test event: %w", err)
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO notifications (chat_id, fingerprint, task_repo, task_id, payload,
		                           status, attempts, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, ?)
		ON CONFLICT (chat_id, fingerprint) DO NOTHING`,
		chatID, ev.Fingerprint, ev.TaskRepo, ev.TaskID, string(payload), now, now)
	if err != nil {
		return fmt.Errorf("store: enqueue test notification: %w", err)
	}
	return nil
}

// NotificationExists reports whether a chat already has a row for a
// fingerprint. Used by tests to assert deduplication at the database level.
func (s *Store) NotificationExists(ctx context.Context, chatID int64, fingerprint string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM notifications WHERE chat_id = ? AND fingerprint = ?`, chatID, fingerprint).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: notification exists: %w", err)
	}
	return true, nil
}

// NotificationStatus reads the current status of one outbox row.
func (s *Store) NotificationStatus(ctx context.Context, id int64) (model.OutboxStatus, int, error) {
	var status string
	var attempts int
	err := s.db.QueryRowContext(ctx,
		`SELECT status, attempts FROM notifications WHERE id = ?`, id).Scan(&status, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("store: notification status: %w", err)
	}
	return model.OutboxStatus(status), attempts, nil
}

// GetNotification loads one outbox row.
func (s *Store) GetNotification(ctx context.Context, id int64) (*model.Notification, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, chat_id, fingerprint, task_repo, task_id, payload, status,
		       attempts, next_attempt_at, last_error, created_at, sent_at
		FROM notifications WHERE id = ?`, id)
	n := &model.Notification{}
	var status, next, lastErr, created string
	var sentAt sql.NullString
	var payload string
	err := row.Scan(&n.ID, &n.ChatID, &n.Fingerprint, &n.TaskRepo, &n.TaskID, &payload, &status,
		&n.Attempts, &next, &lastErr, &created, &sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get notification %d: %w", id, err)
	}
	n.Status = model.OutboxStatus(status)
	n.LastError = lastErr
	n.NextAttemptAt, _ = model.ParseTime(next)
	n.CreatedAt, _ = model.ParseTime(created)
	if sentAt.Valid {
		t, _ := model.ParseTime(sentAt.String)
		n.SentAt = &t
	}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &n.Event); err != nil {
			return nil, fmt.Errorf("store: decode payload of notification %d: %w", id, err)
		}
	}
	return n, nil
}

// formatDeadline renders a "not before" instant at the second granularity the
// schema uses, rounding up. Truncating down instead would turn a sub-second
// backoff into "due immediately" and let the delivery worker hammer Telegram in
// a tight loop.
func formatDeadline(t time.Time) string {
	if t.Nanosecond() != 0 {
		t = t.Truncate(time.Second).Add(time.Second)
	}
	return model.FormatTime(t)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanNotification(row scanner) (*model.Notification, error) {
	n := &model.Notification{}
	var status, next, lastErr, created, payload string
	var sentAt sql.NullString
	if err := row.Scan(&n.ID, &n.ChatID, &n.Fingerprint, &n.TaskRepo, &n.TaskID, &payload, &status,
		&n.Attempts, &next, &lastErr, &created, &sentAt); err != nil {
		return nil, fmt.Errorf("store: scan notification: %w", err)
	}
	n.Status = model.OutboxStatus(status)
	n.LastError = lastErr
	n.NextAttemptAt, _ = model.ParseTime(next)
	n.CreatedAt, _ = model.ParseTime(created)
	if sentAt.Valid {
		t, _ := model.ParseTime(sentAt.String)
		n.SentAt = &t
	}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &n.Event); err != nil {
			return nil, fmt.Errorf("store: decode payload of notification %d: %w", n.ID, err)
		}
	}
	return n, nil
}
