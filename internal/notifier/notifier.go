package notifier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/health"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/store"
)

// linkPreviewOff disables the link card for the task URL. Telegram would
// otherwise fetch and render a preview of git.altlinux.org for every
// notification, which is noise and extra load on their side.
var linkPreviewOff = &models.LinkPreviewOptions{IsDisabled: boolPtr(true)}

func boolPtr(v bool) *bool { return &v }

// PreviewOff returns the shared link-preview suppression options. The UI layer
// uses the same value so commands and notifications look alike.
func PreviewOff() *models.LinkPreviewOptions { return linkPreviewOff }

// Sender is the slice of the Telegram Bot API the delivery worker needs.
// *bot.Bot satisfies it, and so does a recording fake in tests.
type Sender interface {
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
}

// Options tunes the worker.
type Options struct {
	Interval    time.Duration // how often the queue is drained
	MaxAttempts int           // transient failures before a row goes dead
	Spacing     time.Duration // minimum gap between two sends
	BackoffBase time.Duration
	BackoffMax  time.Duration
	Jitter      func() float64
	BatchSize   int
}

// Notifier drains the transactional outbox.
type Notifier struct {
	store   *store.Store
	sender  Sender
	clock   clock.Clock
	log     *slog.Logger
	metrics *health.Metrics
	opts    Options

	mu           sync.RWMutex
	running      bool
	started      bool
	lastDelivery time.Time
	lastError    string

	sendMu   sync.Mutex
	lastSend time.Time
}

// New builds a delivery worker.
func New(st *store.Store, sender Sender, cl clock.Clock, log *slog.Logger, m *health.Metrics, opts Options) *Notifier {
	if cl == nil {
		cl = clock.New()
	}
	if log == nil {
		log = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 8
	}
	if opts.Spacing < 0 {
		opts.Spacing = 0
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = 5 * time.Second
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 30 * time.Minute
	}
	if opts.Jitter == nil {
		opts.Jitter = rand.Float64
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 20
	}
	return &Notifier{
		store:   st,
		sender:  sender,
		clock:   cl,
		log:     log.With("component", "notifier"),
		metrics: m,
		opts:    opts,
	}
}

// Run drains the outbox until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) error {
	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return errors.New("notifier: already started")
	}
	n.started = true
	n.running = true
	n.mu.Unlock()

	defer func() {
		n.mu.Lock()
		n.running = false
		n.mu.Unlock()
	}()

	// A previous unclean exit can leave rows stuck in "sending". They were
	// claimed but never resolved, so they go back to pending and are charged
	// one attempt each.
	if recovered, err := n.store.RecoverSendingNotifications(ctx); err != nil {
		n.log.Error("recovering in-flight notifications failed", "error", err.Error())
	} else if recovered > 0 {
		n.log.Warn("recovered notifications left in-flight by a previous run", "count", recovered)
	}

	n.log.Info("delivery worker started",
		"interval", n.opts.Interval.String(), "max_attempts", n.opts.MaxAttempts)

	// Drain once immediately so a notification queued while the process was
	// down does not wait a full interval.
	n.drain(ctx)

	ticker := time.NewTicker(n.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			n.log.Info("delivery worker stopping", "reason", ctx.Err().Error())
			return nil
		case <-ticker.C:
			n.drain(ctx)
		}
	}
}

// drain empties everything currently due, in bounded batches.
func (n *Notifier) drain(ctx context.Context) int {
	delivered := 0
	for batch := 0; batch < 10; batch++ {
		if ctx.Err() != nil {
			return delivered
		}
		rows, err := n.store.ClaimDueNotifications(ctx, n.opts.BatchSize)
		if err != nil {
			n.setLastError(err.Error())
			n.log.Error("claiming notifications failed", "error", err.Error())
			return delivered
		}
		if len(rows) == 0 {
			return delivered
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				// Put the claim back so it is not lost on shutdown.
				if err := n.store.RetryNotification(ctx, row.ID, "shutdown", n.clock.Now(), false); err != nil {
					n.log.Error("requeueing notification failed", "id", row.ID, "error", err.Error())
				}
				return delivered
			}
			if n.Deliver(ctx, row) {
				delivered++
			}
		}
	}
	return delivered
}

// Drain is the exported form of one drain pass, used by tests and by the
// "send it now" path after a poll cycle. It returns how many messages went out.
func (n *Notifier) Drain(ctx context.Context) int { return n.drain(ctx) }

// Deliver sends one outbox row and records the outcome. It reports whether the
// message was actually accepted by Telegram.
func (n *Notifier) Deliver(ctx context.Context, row model.Notification) bool {
	text := Render(row.Event)
	button := models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: fmt.Sprintf("Open task #%d", row.Event.TaskID), URL: TaskURL(row.Event.TaskID)},
		}},
	}

	n.pace(ctx)

	_, err := n.sender.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             row.ChatID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: linkPreviewOff,
		ReplyMarkup:        button,
	})

	n.mu.Lock()
	n.lastDelivery = n.clock.Now()
	n.mu.Unlock()

	if err == nil {
		if serr := n.store.MarkNotificationSent(ctx, row.ID); serr != nil {
			n.log.Error("marking notification sent failed", "id", row.ID, "error", serr.Error())
		}
		n.observe("sent")
		n.log.Info("notification delivered",
			"id", row.ID, "chat_id", row.ChatID, "task_id", row.Event.TaskID,
			"state", row.Event.State, "attempt", row.Attempts+1)
		return true
	}

	n.handleFailure(ctx, row, err)
	return false
}

// SendNow delivers a rendered message immediately, bypassing the outbox. Used
// by /test, where the point is to show the operator what a notification looks
// like right now.
func (n *Notifier) SendNow(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) error {
	n.pace(ctx)
	_, err := n.sender.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: linkPreviewOff,
		ReplyMarkup:        markup,
	})
	if err != nil {
		n.observe("error")
		return err
	}
	n.observe("sent")
	return nil
}

// handleFailure classifies a send error and updates the row and, when the chat
// is gone, the chat itself.
func (n *Notifier) handleFailure(ctx context.Context, row model.Notification, err error) {
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		// Shutting down mid-send: leave the row for the next start.
		_ = n.store.RetryNotification(context.WithoutCancel(ctx), row.ID,
			"interrupted by shutdown", n.clock.Now(), false)
		return
	}

	kind, retryAfter, reason := classify(err)
	attempts := row.Attempts + 1

	switch kind {
	case failureRateLimit:
		n.observe("rate_limited")
		next := n.clock.Now().Add(retryAfter)
		if serr := n.store.RetryNotification(ctx, row.ID, reason, next, false); serr != nil {
			n.log.Error("requeueing rate-limited notification failed", "id", row.ID, "error", serr.Error())
		}
		n.log.Warn("telegram rate limit, backing off",
			"id", row.ID, "retry_after", retryAfter.String(), "next_attempt", model.FormatTime(next))
		n.setLastError(reason)

	case failurePermanent:
		n.observe("permanent")
		if serr := n.store.MarkNotificationDead(ctx, row.ID, reason); serr != nil {
			n.log.Error("marking notification dead failed", "id", row.ID, "error", serr.Error())
		}
		if serr := n.store.DisableChat(ctx, row.ChatID, reason); serr != nil {
			n.log.Error("disabling chat failed", "chat_id", row.ChatID, "error", serr.Error())
		}
		if cancelled, cerr := n.store.CancelPendingNotifications(ctx, row.ChatID, reason); cerr != nil {
			n.log.Error("cancelling queued notifications failed", "chat_id", row.ChatID, "error", cerr.Error())
		} else if cancelled > 0 {
			n.log.Info("cancelled queued notifications for an unreachable chat",
				"chat_id", row.ChatID, "count", cancelled)
		}
		if n.metrics != nil {
			n.metrics.OutboxDead.Inc()
		}
		n.log.Error("chat is unreachable, delivery disabled",
			"chat_id", row.ChatID, "notification_id", row.ID, "reason", reason)
		n.setLastError(reason)

	default: // failureTransient
		n.observe("transient")
		if attempts >= n.opts.MaxAttempts {
			if serr := n.store.MarkNotificationDead(ctx, row.ID, reason); serr != nil {
				n.log.Error("marking notification dead failed", "id", row.ID, "error", serr.Error())
			}
			if n.metrics != nil {
				n.metrics.OutboxDead.Inc()
			}
			n.log.Error("giving up on notification after repeated failures",
				"id", row.ID, "chat_id", row.ChatID, "attempts", attempts, "reason", reason)
			n.setLastError(reason)
			return
		}
		next := n.clock.Now().Add(n.backoff(attempts))
		if serr := n.store.RetryNotification(ctx, row.ID, reason, next, true); serr != nil {
			n.log.Error("requeueing notification failed", "id", row.ID, "error", serr.Error())
		}
		n.log.Warn("notification delivery failed, will retry",
			"id", row.ID, "chat_id", row.ChatID, "attempt", attempts,
			"max_attempts", n.opts.MaxAttempts, "next_attempt", model.FormatTime(next), "reason", reason)
		n.setLastError(reason)
	}
}

// failureKind is how the worker reacts to a send error.
type failureKind int

const (
	failureTransient failureKind = iota
	failureRateLimit
	failurePermanent
)

// classify maps a Telegram error onto a reaction. The library surfaces the Bot
// API's own error classes as sentinel errors, so this stays honest about what
// Telegram actually said.
func classify(err error) (failureKind, time.Duration, string) {
	if err == nil {
		return failureTransient, 0, ""
	}

	var tooMany *bot.TooManyRequestsError
	if errors.As(err, &tooMany) {
		d := time.Duration(tooMany.RetryAfter) * time.Second
		if d <= 0 {
			d = 5 * time.Second
		}
		return failureRateLimit, d, err.Error()
	}

	switch {
	case errors.Is(err, bot.ErrorForbidden):
		// 403: bot was blocked, kicked, or the chat is gone. Retrying can
		// never succeed and Telegram starts penalising the token.
		return failurePermanent, 0, err.Error()
	case errors.Is(err, bot.ErrorBadRequest):
		// 400: our payload was rejected (chat not found, text too long,
		// entities could not be parsed). Same payload would fail again.
		return failurePermanent, 0, err.Error()
	case errors.Is(err, bot.ErrorUnauthorized):
		// 401: the token is wrong or revoked. Nothing about this row can fix it.
		return failurePermanent, 0, err.Error()
	default:
		return failureTransient, 0, err.Error()
	}
}

// backoff is exponential with equal jitter, capped.
func (n *Notifier) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exp := float64(n.opts.BackoffBase) * math.Pow(2, float64(attempt-1))
	if exp > float64(n.opts.BackoffMax) {
		exp = float64(n.opts.BackoffMax)
	}
	half := exp / 2
	return time.Duration(half + half*n.opts.Jitter())
}

// pace keeps sends spaced out. Telegram allows roughly 30 messages per second
// globally; a burst of a few hundred notifications after an RDB outage should
// not come anywhere near that.
func (n *Notifier) pace(ctx context.Context) {
	if n.opts.Spacing <= 0 {
		return
	}
	n.sendMu.Lock()
	next := n.lastSend.Add(n.opts.Spacing)
	now := time.Now()
	if now.Before(next) {
		wait := next.Sub(now)
		n.lastSend = next
		n.sendMu.Unlock()
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		return
	}
	n.lastSend = now
	n.sendMu.Unlock()
}

// NotifierHealth implements health.Notifier.
func (n *Notifier) NotifierHealth(ctx context.Context) (bool, time.Time, error) {
	n.mu.RLock()
	running := n.running
	last := n.lastDelivery
	lastErr := n.lastError
	n.mu.RUnlock()

	var err error
	if lastErr != "" {
		err = errors.New(lastErr)
	}
	return running, last, err
}

func (n *Notifier) observe(result string) {
	if n.metrics == nil {
		return
	}
	n.metrics.TelegramSendsTotal.WithLabelValues(result).Inc()
}

func (n *Notifier) setLastError(s string) {
	if len(s) > 256 {
		s = s[:256]
	}
	n.mu.Lock()
	n.lastError = s
	n.mu.Unlock()
}
