package notifier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/store"
)

var epoch = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// fakeSender records every send and answers with a scripted error sequence.
type fakeSender struct {
	mu       sync.Mutex
	params   []*bot.SendMessageParams
	scripted []error
	fallback error
}

func (f *fakeSender) SendMessage(_ context.Context, p *bot.SendMessageParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.params = append(f.params, p)

	n := len(f.params)
	if n <= len(f.scripted) {
		if err := f.scripted[n-1]; err != nil {
			return nil, err
		}
	} else if f.fallback != nil {
		return nil, f.fallback
	}
	return &models.Message{ID: 1000 + n, Date: int(time.Now().Unix())}, nil
}

func (f *fakeSender) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.params)
}

func (f *fakeSender) last() *bot.SendMessageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.params) == 0 {
		return nil
	}
	return f.params[len(f.params)-1]
}

// harness wires a store, a fake clock and a fake sender.
type harness struct {
	store  *store.Store
	clock  *clock.Fake
	sender *fakeSender
	n      *Notifier
}

func newHarness(t *testing.T, scripted ...error) *harness {
	t.Helper()
	cl := clock.NewFake(epoch)
	st, err := store.Open(context.Background(), t.TempDir()+"/bot.db", store.Options{
		Migrate: true, Clock: cl, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sender := &fakeSender{scripted: scripted}
	n := New(st, sender, cl, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{
		Interval:    time.Second,
		MaxAttempts: 3,
		Spacing:     0,
		BackoffBase: time.Minute,
		BackoffMax:  time.Hour,
		Jitter:      func() float64 { return 0.5 },
		BatchSize:   10,
	})
	return &harness{store: st, clock: cl, sender: sender, n: n}
}

// enqueue registers a chat and one pending notification for it.
func (h *harness) enqueue(t *testing.T, chatID int64, fingerprint string, due time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	// Only register when the chat is unknown: re-registering would clear a
	// disabled flag the test is asserting on.
	if _, err := h.store.GetChat(ctx, chatID); errors.Is(err, store.ErrNotFound) {
		if _, err := h.store.RegisterChat(ctx, chatID, "private", "", "Someone"); err != nil {
			t.Fatalf("RegisterChat: %v", err)
		}
	} else if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	ev := model.TaskEvent{
		Fingerprint: fingerprint, TaskRepo: "sisyphus", TaskID: 433219,
		State: "DONE", PreviousState: "BUILDING", Owner: "grenka", Try: 2, Iter: 1,
		Stage: "task-save-repo", Changed: due, Packages: []string{"postfix 3.8.21-alt1"},
	}
	if err := h.store.EnqueueTestNotification(ctx, chatID, ev); err != nil {
		t.Fatalf("EnqueueTestNotification: %v", err)
	}
	if !due.Equal(epoch) {
		if _, err := h.store.DB().ExecContext(ctx,
			`UPDATE notifications SET next_attempt_at = ? WHERE fingerprint = ?`,
			model.FormatTime(due), fingerprint); err != nil {
			t.Fatalf("setting the due time: %v", err)
		}
	}
	var id int64
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT id FROM notifications WHERE fingerprint = ?`, fingerprint).Scan(&id); err != nil {
		t.Fatalf("reading the row id: %v", err)
	}
	return id
}

func (h *harness) row(t *testing.T, id int64) *model.Notification {
	t.Helper()
	row, err := h.store.GetNotification(context.Background(), id)
	if err != nil {
		t.Fatalf("GetNotification(%d): %v", id, err)
	}
	return row
}

func TestDrainDeliversEveryDueRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.enqueue(t, 1, "fp-1", epoch)
	h.enqueue(t, 2, "fp-2", epoch)
	h.enqueue(t, 3, "fp-3", epoch.Add(time.Hour)) // not due yet

	if got := h.n.Drain(ctx); got != 2 {
		t.Errorf("drained = %d, want 2", got)
	}
	if h.sender.calls() != 2 {
		t.Errorf("sends = %d, want 2", h.sender.calls())
	}

	var sent, pending int
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE status = 'sent'`).Scan(&sent); err != nil {
		t.Fatalf("counting sent: %v", err)
	}
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE status = 'pending'`).Scan(&pending); err != nil {
		t.Fatalf("counting pending: %v", err)
	}
	if sent != 2 || pending != 1 {
		t.Errorf("sent = %d, pending = %d; want 2 and 1", sent, pending)
	}
}

func TestDeliveredMessageCarriesTheTaskButtonAndHTML(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.enqueue(t, 42, "fp", epoch)

	if got := h.n.Drain(ctx); got != 1 {
		t.Fatalf("drained = %d", got)
	}
	p := h.sender.last()
	if p == nil {
		t.Fatal("nothing was sent")
	}
	if p.ChatID != int64(42) {
		t.Errorf("chat id = %v", p.ChatID)
	}
	if p.ParseMode != models.ParseModeHTML {
		t.Errorf("parse mode = %q, want HTML", p.ParseMode)
	}
	if !strings.Contains(p.Text, "Task #433219") || !strings.Contains(p.Text, "BUILDING → DONE") {
		t.Errorf("text = %q", p.Text)
	}
	if p.LinkPreviewOptions == nil || p.LinkPreviewOptions.IsDisabled == nil || !*p.LinkPreviewOptions.IsDisabled {
		t.Error("link previews must be disabled")
	}

	markup, ok := p.ReplyMarkup.(models.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("reply markup = %T, want an inline keyboard", p.ReplyMarkup)
	}
	if len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 1 {
		t.Fatalf("keyboard = %+v", markup.InlineKeyboard)
	}
	btn := markup.InlineKeyboard[0][0]
	if btn.URL != "https://git.altlinux.org/tasks/433219/" {
		t.Errorf("button URL = %q", btn.URL)
	}
	if !strings.Contains(btn.Text, "433219") {
		t.Errorf("button text = %q", btn.Text)
	}
}

func TestTransientFailureIsRetriedWithBackoff(t *testing.T) {
	h := newHarness(t, errors.New("dial tcp: connection refused"))
	ctx := context.Background()
	id := h.enqueue(t, 1, "fp", epoch)

	if got := h.n.Drain(ctx); got != 0 {
		t.Errorf("drained = %d, want 0 delivered", got)
	}
	row := h.row(t, id)
	if row.Status != model.OutboxPending {
		t.Errorf("status = %q, want pending", row.Status)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", row.Attempts)
	}
	if !row.NextAttemptAt.After(epoch) {
		t.Errorf("next attempt = %s, want a future time", row.NextAttemptAt)
	}
	if row.LastError == "" {
		t.Error("the failure reason must be recorded")
	}
	if row.SentAt != nil {
		t.Error("sent_at must stay empty")
	}
	// The chat stays enabled: a network blip is not the user's fault.
	chat, err := h.store.GetChat(ctx, 1)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !chat.Enabled {
		t.Error("a transient failure must not disable the chat")
	}
}

func TestRepeatedTransientFailureEventuallyGoesDead(t *testing.T) {
	h := newHarness(t)
	h.sender.fallback = errors.New("still down")
	ctx := context.Background()
	id := h.enqueue(t, 1, "fp", epoch)

	for i := range 10 {
		h.clock.Advance(2 * time.Hour) // make the row due again
		h.n.Drain(ctx)
		row := h.row(t, id)
		if row.Status == model.OutboxDead {
			if i+1 != 3 {
				t.Errorf("the row went dead after %d attempts, want MaxAttempts=3", i+1)
			}
			if row.Attempts != 3 {
				t.Errorf("attempts = %d", row.Attempts)
			}
			if !strings.Contains(row.LastError, "still down") {
				t.Errorf("last error = %q", row.LastError)
			}
			return
		}
	}
	t.Fatalf("the row never went dead: %+v", h.row(t, id))
}

func TestForbiddenIsPermanentAndDisablesTheChat(t *testing.T) {
	h := newHarness(t, fmt.Errorf("%w: bot was blocked by the user", bot.ErrorForbidden))
	ctx := context.Background()
	id := h.enqueue(t, 77, "fp", epoch)

	h.n.Drain(ctx)

	row := h.row(t, id)
	if row.Status != model.OutboxDead {
		t.Errorf("status = %q, want dead on the first forbidden answer", row.Status)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts = %d; exactly one try must be made before giving up", row.Attempts)
	}
	if h.sender.calls() != 1 {
		t.Errorf("sends = %d, want a single attempt", h.sender.calls())
	}
	chat, err := h.store.GetChat(ctx, 77)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.Enabled {
		t.Error("the chat must be disabled after the bot was blocked")
	}
	if !strings.Contains(chat.DisabledReason, "blocked") {
		t.Errorf("reason = %q", chat.DisabledReason)
	}

	// Nothing further is sent for that chat.
	// Anything queued later for that chat is neither delivered nor retried:
	// the claim skips disabled chats.
	second := h.enqueue(t, 77, "fp-2", epoch)
	before := h.sender.calls()
	h.n.Drain(ctx)
	if h.sender.calls() != before {
		t.Error("a disabled chat must not receive anything")
	}
	if got := h.row(t, second).Status; got != model.OutboxPending {
		t.Errorf("a disabled chat's rows must stay pending for a possible /resume, got %q", got)
	}

	// Cancelling explicitly is what the permanent path does for the rows that
	// were already queued.
	cancelled, err := h.store.CancelPendingNotifications(ctx, 77, "gone")
	if err != nil {
		t.Fatalf("CancelPendingNotifications: %v", err)
	}
	if cancelled != 1 {
		t.Errorf("cancelled = %d, want the one pending row", cancelled)
	}
	if got := h.row(t, second).Status; got != model.OutboxDead {
		t.Errorf("status = %q, want dead", got)
	}
}

func TestBadRequestIsPermanent(t *testing.T) {
	h := newHarness(t, fmt.Errorf("%w: chat not found", bot.ErrorBadRequest))
	ctx := context.Background()
	id := h.enqueue(t, 5, "fp", epoch)
	h.n.Drain(ctx)
	if got := h.row(t, id).Status; got != model.OutboxDead {
		t.Errorf("status = %q, want dead", got)
	}
	chat, _ := h.store.GetChat(ctx, 5)
	if chat.Enabled {
		t.Error("an unreachable chat must be disabled")
	}
}

func TestUnauthorizedIsPermanent(t *testing.T) {
	h := newHarness(t, fmt.Errorf("%w: token revoked", bot.ErrorUnauthorized))
	ctx := context.Background()
	id := h.enqueue(t, 6, "fp", epoch)
	h.n.Drain(ctx)
	if got := h.row(t, id).Status; got != model.OutboxDead {
		t.Errorf("status = %q, want dead", got)
	}
}

func TestRateLimitUsesRetryAfterWithoutConsumingAnAttempt(t *testing.T) {
	h := newHarness(t, &bot.TooManyRequestsError{Message: "too many requests", RetryAfter: 120})
	ctx := context.Background()
	id := h.enqueue(t, 8, "fp", epoch)

	h.n.Drain(ctx)

	row := h.row(t, id)
	if row.Status != model.OutboxPending {
		t.Errorf("status = %q, want pending", row.Status)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d; flow control must not consume the retry budget", row.Attempts)
	}
	want := epoch.Add(120 * time.Second)
	if !row.NextAttemptAt.Equal(want) {
		t.Errorf("next attempt = %s, want %s (now + retry_after)", row.NextAttemptAt, want)
	}
	if !strings.Contains(row.LastError, "retry_after 120") {
		t.Errorf("last error = %q", row.LastError)
	}
}

func TestRateLimitWithoutRetryAfterFallsBackToADefault(t *testing.T) {
	h := newHarness(t, &bot.TooManyRequestsError{Message: "slow down", RetryAfter: 0})
	ctx := context.Background()
	id := h.enqueue(t, 9, "fp", epoch)
	h.n.Drain(ctx)

	row := h.row(t, id)
	if row.Status != model.OutboxPending {
		t.Errorf("status = %q", row.Status)
	}
	if !row.NextAttemptAt.After(epoch.Add(4 * time.Second)) {
		t.Errorf("next attempt = %s, want at least a few seconds out", row.NextAttemptAt)
	}
}

func TestSuccessfulRetryAfterAFailure(t *testing.T) {
	h := newHarness(t, errors.New("transient"))
	ctx := context.Background()
	id := h.enqueue(t, 10, "fp", epoch)

	h.n.Drain(ctx)
	if got := h.row(t, id).Status; got != model.OutboxPending {
		t.Fatalf("status after the first failure = %q", got)
	}

	h.sender.scripted = nil // the next call succeeds
	h.clock.Advance(2 * time.Hour)
	if got := h.n.Drain(ctx); got != 1 {
		t.Errorf("drained = %d, want 1", got)
	}
	row := h.row(t, id)
	if row.Status != model.OutboxSent {
		t.Errorf("status = %q, want sent", row.Status)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts = %d, want the one failure recorded", row.Attempts)
	}
	if row.SentAt == nil {
		t.Error("sent_at must be stamped")
	}
}

func TestRecoveryAfterAnUncleanShutdown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.enqueue(t, 11, "fp", epoch)

	// Claim the row and then "crash" by abandoning the notifier.
	rows, err := h.store.ClaimDueNotifications(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim = %d rows, %v", len(rows), err)
	}
	if got := h.row(t, id).Status; got != model.OutboxSending {
		t.Fatalf("status = %q, want sending", got)
	}

	// A new worker recovers it on start-up.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	second := New(h.store, h.sender, h.clock, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{
		MaxAttempts: 3, BackoffBase: time.Minute, Jitter: func() float64 { return 0.5 },
	})
	go func() { _ = second.Run(runCtx) }()
	// Give the worker a moment to run its recovery pass.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.sender.calls() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.sender.calls() != 1 {
		t.Errorf("sends = %d, want the recovered row delivered once", h.sender.calls())
	}
	if got := h.row(t, id).Status; got != model.OutboxSent {
		t.Errorf("status = %q, want sent", got)
	}
}

func TestRunStopsOnCancellation(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.enqueue(t, 12, "fp", epoch)

	done := make(chan error, 1)
	go func() { done <- h.n.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for h.sender.calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("the worker never delivered the queued row")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want a clean stop", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
	if running, _, _ := h.n.NotifierHealth(context.Background()); running {
		t.Error("the worker still reports itself running")
	}
}

func TestRunRefusesToStartTwice(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.n.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	if err := h.n.Run(ctx); err == nil {
		t.Error("a second Run must be refused")
	}
	cancel()
	<-done
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSendNowBypassesTheOutbox(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.RegisterChat(ctx, 20, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}

	markup := models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{{Text: "x", CallbackData: "hp"}},
	}}
	if err := h.n.SendNow(ctx, 20, "<b>hello</b>", markup); err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	p := h.sender.last()
	if p == nil || p.Text != "<b>hello</b>" {
		t.Fatalf("sent = %+v", p)
	}
	if p.ParseMode != models.ParseModeHTML {
		t.Errorf("parse mode = %q", p.ParseMode)
	}
	var n int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("SendNow queued %d rows; it must not touch the outbox", n)
	}

	h.sender.fallback = errors.New("nope")
	if err := h.n.SendNow(ctx, 20, "x", nil); err == nil {
		t.Error("a failed SendNow must report the error")
	}
}

func TestNotifierHealthReportsErrors(t *testing.T) {
	h := newHarness(t, errors.New("boom"))
	ctx := context.Background()

	running, last, err := h.n.NotifierHealth(ctx)
	if running || err != nil || !last.IsZero() {
		t.Errorf("before starting: running=%t last=%s err=%v", running, last, err)
	}

	h.enqueue(t, 30, "fp", epoch)
	h.n.Drain(ctx)

	running, last, err = h.n.NotifierHealth(ctx)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want the last failure surfaced", err)
	}
	if last.IsZero() {
		t.Error("the last delivery attempt must be stamped")
	}
	if running {
		t.Error("Drain alone must not mark the worker as running")
	}
}

func TestPacingSpacesOutSends(t *testing.T) {
	cl := clock.NewFake(epoch)
	st, err := store.Open(context.Background(), t.TempDir()+"/bot.db", store.Options{Migrate: true, Clock: cl})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer st.Close()

	sender := &fakeSender{}
	n := New(st, sender, cl, discardLogger(), nil, Options{
		Spacing:     40 * time.Millisecond,
		MaxAttempts: 3,
		Jitter:      func() float64 { return 0.5 },
	})

	start := time.Now()
	for i := range 3 {
		if err := n.SendNow(context.Background(), int64(i), "x", nil); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 75*time.Millisecond {
		t.Errorf("three paced sends took %s, want at least two spacings", elapsed)
	}
}

func TestBackoffShape(t *testing.T) {
	n := New(nil, nil, clock.NewFake(epoch), nil, nil, Options{
		BackoffBase: 10 * time.Second,
		BackoffMax:  time.Minute,
		Jitter:      func() float64 { return 0 },
	})
	// Equal jitter with the random half pinned to 0: each delay is half the
	// exponential value, which still doubles per attempt and respects the cap.
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 30 * time.Second}, // capped at 1m, half of it
		{9, 30 * time.Second},
		{0, 5 * time.Second}, // clamped to attempt 1
	} {
		if got := n.backoff(tc.attempt); got != tc.want {
			t.Errorf("backoff(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
	n.opts.Jitter = func() float64 { return 1 }
	if got := n.backoff(1); got != 10*time.Second {
		t.Errorf("full jitter at attempt 1 = %s, want 10s", got)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want failureKind
	}{
		{"nil is not a failure", nil, failureTransient},
		{"plain network error", errors.New("EOF"), failureTransient},
		{"forbidden", fmt.Errorf("%w: blocked", bot.ErrorForbidden), failurePermanent},
		{"bad request", fmt.Errorf("%w: chat not found", bot.ErrorBadRequest), failurePermanent},
		{"unauthorized", fmt.Errorf("%w: revoked", bot.ErrorUnauthorized), failurePermanent},
		{"too many requests", &bot.TooManyRequestsError{RetryAfter: 5}, failureRateLimit},
		{"wrapped too many requests", fmt.Errorf("send failed: %w", &bot.TooManyRequestsError{RetryAfter: 5}), failureRateLimit},
		{"conflict", fmt.Errorf("%w: terminated", bot.ErrorConflict), failureTransient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, retryAfter, reason := classify(tc.err)
			if kind != tc.want {
				t.Errorf("kind = %v, want %v", kind, tc.want)
			}
			if kind == failureRateLimit && retryAfter != 5*time.Second {
				t.Errorf("retry after = %s", retryAfter)
			}
			if tc.err != nil && reason == "" {
				t.Error("a reason must be recorded for every failure")
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	n := New(nil, nil, nil, nil, nil, Options{})
	if n.opts.Interval != 5*time.Second || n.opts.MaxAttempts != 8 || n.opts.BatchSize != 20 {
		t.Errorf("defaults = %+v", n.opts)
	}
	if n.opts.BackoffBase != 5*time.Second || n.opts.BackoffMax != 30*time.Minute {
		t.Errorf("backoff defaults = %s / %s", n.opts.BackoffBase, n.opts.BackoffMax)
	}
	if n.opts.Jitter == nil || n.clock == nil || n.log == nil {
		t.Error("a nil clock, logger or jitter source must be replaced")
	}
}
