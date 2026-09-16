package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"altrdb-telegram-bot/internal/model"
)

// SavePollState upserts the per-branch polling health record. The poller calls
// it after every attempt, successful or not, so /healthz can show exactly where
// ingestion stands without keeping state in memory.
func (s *Store) SavePollState(ctx context.Context, st model.PollState) error {
	if st.Branch == "" {
		return errors.New("store: poll state without branch")
	}
	now := s.now()
	saturated := 0
	if st.WindowSaturated {
		saturated = 1
	}
	baselined := 0
	if st.Baselined {
		baselined = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO poll_state (branch, last_attempt_at, last_success_at, last_error,
		                        consecutive_failures, tasks_seen, oldest_task_changed,
		                        window_saturated, baselined, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (branch) DO UPDATE SET
			last_attempt_at      = excluded.last_attempt_at,
			last_success_at      = excluded.last_success_at,
			last_error           = excluded.last_error,
			consecutive_failures = excluded.consecutive_failures,
			tasks_seen           = excluded.tasks_seen,
			oldest_task_changed  = excluded.oldest_task_changed,
			window_saturated     = excluded.window_saturated,
			baselined            = excluded.baselined,
			updated_at           = excluded.updated_at`,
		st.Branch,
		nullableTime(st.LastAttemptAt),
		nullableTime(st.LastSuccessAt),
		truncate(st.LastError, 512),
		st.ConsecutiveFailures,
		st.TasksSeen,
		nullableTime(st.OldestTaskChanged),
		saturated,
		baselined,
		now)
	if err != nil {
		return fmt.Errorf("store: save poll state %s: %w", st.Branch, err)
	}
	return nil
}

// GetPollState reads one branch's health record.
func (s *Store) GetPollState(ctx context.Context, branch string) (*model.PollState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT branch, last_attempt_at, last_success_at, last_error, consecutive_failures,
		       tasks_seen, oldest_task_changed, window_saturated, baselined, updated_at
		FROM poll_state WHERE branch = ?`, branch)
	st, err := scanPollState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return st, err
}

// ListPollStates returns every branch that has ever been polled.
func (s *Store) ListPollStates(ctx context.Context) ([]model.PollState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT branch, last_attempt_at, last_success_at, last_error, consecutive_failures,
		       tasks_seen, oldest_task_changed, window_saturated, baselined, updated_at
		FROM poll_state ORDER BY branch`)
	if err != nil {
		return nil, fmt.Errorf("store: list poll states: %w", err)
	}
	defer rows.Close()

	var out []model.PollState
	for rows.Next() {
		st, err := scanPollState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

func scanPollState(row scanner) (*model.PollState, error) {
	var st model.PollState
	var lastAttempt, lastSuccess, oldest sql.NullString
	var lastErr, updated string
	var saturated, baselined int
	if err := row.Scan(&st.Branch, &lastAttempt, &lastSuccess, &lastErr, &st.ConsecutiveFailures,
		&st.TasksSeen, &oldest, &saturated, &baselined, &updated); err != nil {
		return nil, fmt.Errorf("store: scan poll state: %w", err)
	}
	if lastAttempt.Valid {
		t, _ := model.ParseTime(lastAttempt.String)
		st.LastAttemptAt = &t
	}
	if lastSuccess.Valid {
		t, _ := model.ParseTime(lastSuccess.String)
		st.LastSuccessAt = &t
	}
	if oldest.Valid {
		t, _ := model.ParseTime(oldest.String)
		st.OldestTaskChanged = &t
	}
	st.LastError = lastErr
	st.WindowSaturated = saturated != 0
	st.Baselined = baselined != 0
	st.UpdatedAt, _ = model.ParseTime(updated)
	return &st, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return model.FormatTime(*t)
}
