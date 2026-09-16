package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/model"
)

// outboxFixture creates a chat and one pending notification for it.
func outboxFixture(t *testing.T, st *Store, chatID int64, fingerprint string, due time.Time) int64 {
	t.Helper()
	ctx := context.Background()

	if _, err := st.GetChat(ctx, chatID); err != nil {
		if _, err := st.RegisterChat(ctx, chatID, "private", "", ""); err != nil {
			t.Fatalf("RegisterChat %d: %v", chatID, err)
		}
	}
	ev := model.TaskEvent{
		Fingerprint: fingerprint, TaskRepo: "sisyphus", TaskID: 433219,
		State: "DONE", Owner: "grenka", Try: 2, Iter: 1, Stage: "task-save-repo",
		Changed: epoch, Packages: []string{"postfix 3.8.21-alt1"},
	}
	if err := st.EnqueueTestNotification(ctx, chatID, ev); err != nil {
		t.Fatalf("EnqueueTestNotification: %v", err)
	}
	if !due.Equal(epoch) {
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE notifications SET next_attempt_at = ? WHERE chat_id = ? AND fingerprint = ?`,
			model.FormatTime(due), chatID, fingerprint); err != nil {
			t.Fatalf("setting the due time: %v", err)
		}
	}

	var id int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM notifications WHERE chat_id = ? AND fingerprint = ?`, chatID, fingerprint).Scan(&id); err != nil {
		t.Fatalf("reading the row id: %v", err)
	}
	return id
}

func TestOutboxUniqueConstraintBlocksADuplicateEvent(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	id := outboxFixture(t, st, 10, "fp-1", epoch)

	// The same event for the same chat again: ON CONFLICT DO NOTHING.
	ev := model.TaskEvent{Fingerprint: "fp-1", TaskRepo: "sisyphus", TaskID: 1, State: "DONE", Owner: "grenka"}
	if err := st.EnqueueTestNotification(ctx, 10, ev); err != nil {
		t.Fatalf("a duplicate insert must be ignored, not error: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want the unique constraint to hold", n)
	}

	// A different fingerprint for the same chat is a different event.
	ev.Fingerprint = "fp-2"
	if err := st.EnqueueTestNotification(ctx, 10, ev); err != nil {
		t.Fatalf("EnqueueTestNotification: %v", err)
	}
	// The same fingerprint for a different chat is a different delivery.
	if err := st.EnqueueTestNotification(ctx, 11, model.TaskEvent{
		Fingerprint: "fp-1", TaskRepo: "sisyphus", TaskID: 1, State: "DONE", Owner: "grenka",
	}); err == nil {
		// chat 11 does not exist, so the foreign key must reject it.
		t.Error("inserting for an unknown chat must fail the foreign key")
	}

	if _, err := st.GetNotification(ctx, id); err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
}

func TestClaimDueNotificationsOnlyTakesDueRows(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()

	due := outboxFixture(t, st, 20, "fp-due", epoch)
	outboxFixture(t, st, 21, "fp-future", epoch.Add(time.Hour))

	rows, err := st.ClaimDueNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimDueNotifications: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != due {
		t.Fatalf("claimed = %+v, want only the due row", rows)
	}
	if rows[0].Status != model.OutboxSending {
		t.Errorf("status = %q, want sending", rows[0].Status)
	}
	if rows[0].Event.TaskID != 433219 || rows[0].Event.State != "DONE" {
		t.Errorf("payload did not round-trip: %+v", rows[0].Event)
	}
	if len(rows[0].Event.Packages) != 1 {
		t.Errorf("packages = %v", rows[0].Event.Packages)
	}

	// A row already claimed is not handed out again.
	again, err := st.ClaimDueNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimDueNotifications again: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second claim returned %d rows, want 0", len(again))
	}

	// Advancing past the future row makes it claimable.
	fake.Advance(2 * time.Hour)
	later, err := st.ClaimDueNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimDueNotifications after advancing: %v", err)
	}
	if len(later) != 1 || later[0].ChatID != 21 {
		t.Errorf("claimed = %+v", later)
	}
}

func TestClaimRespectsTheBatchLimit(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	for i := range 5 {
		outboxFixture(t, st, int64(30+i), "fp-"+string(rune('a'+i)), epoch)
	}
	rows, err := st.ClaimDueNotifications(ctx, 2)
	if err != nil {
		t.Fatalf("ClaimDueNotifications: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("claimed = %d, want the batch limit of 2", len(rows))
	}
}

func TestClaimIsSerialisedBetweenWorkers(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	for i := range 10 {
		outboxFixture(t, st, int64(40+i), "fp-conc-"+string(rune('a'+i)), epoch)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = map[int64]int{}
		errs    []error
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := st.ClaimDueNotifications(ctx, 20)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for _, r := range rows {
				claimed[r.ID]++
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("claim errors: %v", errs)
	}
	if len(claimed) != 10 {
		t.Errorf("%d distinct rows were claimed, want 10", len(claimed))
	}
	for id, times := range claimed {
		if times != 1 {
			t.Errorf("row %d was handed out %d times", id, times)
		}
	}
}

func TestMarkNotificationSent(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()
	id := outboxFixture(t, st, 50, "fp-sent", epoch)

	if _, err := st.ClaimDueNotifications(ctx, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	fake.Advance(time.Minute)
	if err := st.MarkNotificationSent(ctx, id); err != nil {
		t.Fatalf("MarkNotificationSent: %v", err)
	}

	row, err := st.GetNotification(ctx, id)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	if row.Status != model.OutboxSent {
		t.Errorf("status = %q, want sent", row.Status)
	}
	if row.SentAt == nil || !row.SentAt.Equal(epoch.Add(time.Minute)) {
		t.Errorf("sent_at = %v, want %s", row.SentAt, epoch.Add(time.Minute))
	}
	if row.LastError != "" {
		t.Errorf("last error should be cleared on success, got %q", row.LastError)
	}

	// A sent row is never claimed again.
	rows, err := st.ClaimDueNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("claimed %d rows after the only one was sent", len(rows))
	}
}

func TestRetryNotificationChargesOnlyWhenAsked(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()
	id := outboxFixture(t, st, 60, "fp-retry", epoch)

	rows, err := st.ClaimDueNotifications(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim = %d rows, %v", len(rows), err)
	}
	if rows[0].Attempts != 0 {
		t.Errorf("claiming charged an attempt: attempts = %d", rows[0].Attempts)
	}

	// Telegram flow control: reschedule without consuming the budget.
	fake.Advance(time.Minute)
	if err := st.RetryNotification(ctx, id, "too many requests: retry_after 30",
		epoch.Add(2*time.Minute), false); err != nil {
		t.Fatalf("RetryNotification: %v", err)
	}
	status, attempts, err := st.NotificationStatus(ctx, id)
	if err != nil {
		t.Fatalf("NotificationStatus: %v", err)
	}
	if status != model.OutboxPending || attempts != 0 {
		t.Errorf("after a rate-limit reschedule: status=%q attempts=%d", status, attempts)
	}

	// A genuine transient failure costs one attempt.
	if err := st.RetryNotification(ctx, id, "connection reset", epoch.Add(5*time.Minute), true); err != nil {
		t.Fatalf("RetryNotification with charge: %v", err)
	}
	status, attempts, err = st.NotificationStatus(ctx, id)
	if err != nil {
		t.Fatalf("NotificationStatus: %v", err)
	}
	if status != model.OutboxPending || attempts != 1 {
		t.Errorf("after a transient failure: status=%q attempts=%d", status, attempts)
	}

	row, err := st.GetNotification(ctx, id)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	if row.LastError != "connection reset" {
		t.Errorf("last error = %q", row.LastError)
	}
	if !row.NextAttemptAt.Equal(epoch.Add(5 * time.Minute)) {
		t.Errorf("next attempt = %s", row.NextAttemptAt)
	}
}

func TestMarkNotificationDead(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	id := outboxFixture(t, st, 70, "fp-dead", epoch)

	long := strings.Repeat("e", 2000)
	if err := st.MarkNotificationDead(ctx, id, long); err != nil {
		t.Fatalf("MarkNotificationDead: %v", err)
	}
	row, err := st.GetNotification(ctx, id)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	if row.Status != model.OutboxDead {
		t.Errorf("status = %q, want dead", row.Status)
	}
	if len(row.LastError) > 512 {
		t.Errorf("last error kept %d bytes", len(row.LastError))
	}
	rows, err := st.ClaimDueNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a dead row was claimed again: %+v", rows)
	}
}

func TestRecoverSendingNotifications(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	outboxFixture(t, st, 80, "fp-a", epoch)
	outboxFixture(t, st, 81, "fp-b", epoch)

	rows, err := st.ClaimDueNotifications(ctx, 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("claimed = %d", len(rows))
	}

	recovered, err := st.RecoverSendingNotifications(ctx)
	if err != nil {
		t.Fatalf("RecoverSendingNotifications: %v", err)
	}
	if recovered != 1 {
		t.Errorf("recovered = %d, want the one in-flight row", recovered)
	}

	// It is charged an attempt, so a crash loop cannot run forever.
	status, attempts, err := st.NotificationStatus(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("NotificationStatus: %v", err)
	}
	if status != model.OutboxPending || attempts != 1 {
		t.Errorf("recovered row = status %q attempts %d", status, attempts)
	}

	// And both rows are claimable again.
	again, err := st.ClaimDueNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(again) != 2 {
		t.Errorf("claimed = %d, want both rows", len(again))
	}
}

func TestOutboxCounts(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()

	empty, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatalf("OutboxCounts: %v", err)
	}
	if empty.Pending != 0 || empty.Sending != 0 || empty.Dead != 0 || empty.OldestAge != 0 {
		t.Errorf("empty outbox = %+v", empty)
	}

	outboxFixture(t, st, 90, "fp-1", epoch)
	outboxFixture(t, st, 91, "fp-2", epoch)
	dead := outboxFixture(t, st, 92, "fp-3", epoch)
	if err := st.MarkNotificationDead(ctx, dead, "gone"); err != nil {
		t.Fatalf("MarkNotificationDead: %v", err)
	}

	fake.Advance(90 * time.Second)
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatalf("OutboxCounts: %v", err)
	}
	if counts.Pending != 2 || counts.Dead != 1 || counts.Sending != 0 {
		t.Errorf("counts = %+v", counts)
	}
	if counts.OldestAge < 89 || counts.OldestAge > 91 {
		t.Errorf("oldest pending age = %d s, want about 90", counts.OldestAge)
	}
}

func TestPruneSentNotifications(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()

	sent := outboxFixture(t, st, 95, "fp-sent", epoch)
	dead := outboxFixture(t, st, 96, "fp-dead", epoch)
	outboxFixture(t, st, 97, "fp-pending", epoch)

	if err := st.MarkNotificationSent(ctx, sent); err != nil {
		t.Fatalf("MarkNotificationSent: %v", err)
	}
	if err := st.MarkNotificationDead(ctx, dead, "x"); err != nil {
		t.Fatalf("MarkNotificationDead: %v", err)
	}

	fake.Advance(20 * 24 * time.Hour)
	n, err := st.PruneSentNotifications(ctx, fake.Now().Add(-14*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneSentNotifications: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned = %d, want the sent and the dead row", n)
	}
	if _, err := st.GetNotification(ctx, sent); err != ErrNotFound {
		t.Errorf("the sent row survived: %v", err)
	}
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatalf("OutboxCounts: %v", err)
	}
	if counts.Pending != 1 {
		t.Errorf("pending = %d, want the untouched row kept", counts.Pending)
	}
}

func TestGetNotificationMissing(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.GetNotification(context.Background(), 12345); err != ErrNotFound {
		t.Errorf("GetNotification = %v, want ErrNotFound", err)
	}
	if _, _, err := st.NotificationStatus(context.Background(), 12345); err != ErrNotFound {
		t.Errorf("NotificationStatus = %v, want ErrNotFound", err)
	}
}

func TestNotificationPayloadSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bot.db"
	ctx := context.Background()

	st, fake := newTestStoreAt(t, path)
	id := outboxFixture(t, st, 99, "fp-persist", epoch)
	_ = st.Close()

	reopened, err := Open(ctx, path, Options{Migrate: true, Clock: fake})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	row, err := reopened.GetNotification(ctx, id)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	if row.Status != model.OutboxPending {
		t.Errorf("status = %q, want pending after a restart", row.Status)
	}
	if row.Event.TaskID != 433219 || row.Event.Owner != "grenka" || row.Event.Stage != "task-save-repo" {
		t.Errorf("event = %+v", row.Event)
	}
	if len(row.Event.Packages) != 1 || row.Event.Packages[0] != "postfix 3.8.21-alt1" {
		t.Errorf("packages = %v", row.Event.Packages)
	}
}
