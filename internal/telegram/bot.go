// Package telegram implements the bot's user interface: commands, checkbox
// style inline keyboards, callback routing and the update loop.
//
// Everything a handler needs lives in the database, and every handler answers
// its callback query before doing slow work, so the Telegram client spinner
// always stops.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/health"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/notifier"
	"altrdb-telegram-bot/internal/rdb"
	"altrdb-telegram-bot/internal/store"
)

// Directory is the part of the poller the UI calls when a user asks for a
// fresh branch list or packager directory.
type Directory interface {
	RefreshBranchesNow(ctx context.Context) error
	RefreshMaintainers(ctx context.Context, branch string) error
}

// TaskLookup fetches one task on demand. Implemented by the RDB client and used
// only by /task, never by the polling path.
type TaskLookup interface {
	TaskInfo(ctx context.Context, taskID int64) (*rdb.TaskInfoResponse, error)
}

// Options wires the bot.
type Options struct {
	Config    *config.Config
	Store     *store.Store
	Clock     clock.Clock
	Logger    *slog.Logger
	Metrics   *health.Metrics
	Refresher Directory
	Tasks     TaskLookup

	// ServerURL overrides https://api.telegram.org. Tests point it at an
	// httptest server; production leaves it empty.
	ServerURL string
	// HTTPClient is handed to the library for Bot API calls.
	HTTPClient *http.Client
	// Workers is the number of update-processing goroutines.
	Workers int
	// SyncHandlers makes ProcessUpdate run handlers inline. Tests need this;
	// production wants the library's async behaviour.
	SyncHandlers bool
	// PageSize is how many checkbox rows one keyboard page holds. The states
	// screen ignores it: it is always one two-column page (see statesPerPage).
	PageSize int
}

// Bot is the Telegram side of the application.
type Bot struct {
	api      *bot.Bot
	store    *store.Store
	clock    clock.Clock
	log      *slog.Logger
	cfg      *config.Config
	metrics  *health.Metrics
	refresh  Directory
	tasks    TaskLookup
	ntf      *notifier.Notifier
	terminal model.TerminalStates
	pageSize int

	botID       int64
	botUsername string
	refusals    *refuseLog

	mu           sync.RWMutex
	running      bool
	lastUpdateID int64
	lastUpdateAt time.Time
	lastError    string

	updates   atomic.Uint64
	callbacks atomic.Uint64
	commands  atomic.Uint64
	rejected  atomic.Uint64
}

// New builds the bot. It performs the library's getMe handshake, so the token
// and the network (or the test server) must be reachable.
func New(opts Options) (*Bot, error) {
	if opts.Config == nil {
		return nil, errors.New("telegram: config is required")
	}
	if opts.Store == nil {
		return nil, errors.New("telegram: store is required")
	}
	if opts.Clock == nil {
		opts.Clock = clock.New()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.PageSize <= 0 {
		opts.PageSize = 8
	}

	bt := &Bot{
		store:    opts.Store,
		clock:    opts.Clock,
		log:      opts.Logger.With("component", "telegram"),
		cfg:      opts.Config,
		metrics:  opts.Metrics,
		refresh:  opts.Refresher,
		tasks:    opts.Tasks,
		terminal: model.NewTerminalStates(opts.Config.TerminalStates),
		pageSize: opts.PageSize,
		refusals: newRefuseLog(),
	}

	libOpts := []bot.Option{
		bot.WithDefaultHandler(bt.HandleUpdate),
		bot.WithWorkers(opts.Workers),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateCallbackQuery,
			models.AllowedUpdateMyChatMember,
		}),
		bot.WithErrorsHandler(func(err error) {
			bt.log.Error("telegram error", "error", err.Error())
			bt.setLastError(err.Error())
		}),
	}
	if opts.ServerURL != "" {
		libOpts = append(libOpts, bot.WithServerURL(opts.ServerURL))
	}
	if opts.HTTPClient != nil {
		libOpts = append(libOpts, bot.WithHTTPClient(50*time.Second, opts.HTTPClient))
	}
	if opts.SyncHandlers {
		libOpts = append(libOpts, bot.WithNotAsyncHandlers())
	}

	// Resume from the persisted offset so a restart does not replay the
	// backlog and re-toggle checkboxes the user already set.
	if offset, err := opts.Store.TelegramOffset(context.Background()); err != nil {
		bt.log.Warn("reading telegram offset failed, starting from zero", "error", err.Error())
	} else if offset > 0 {
		libOpts = append(libOpts, bot.WithInitialOffset(offset))
		bt.lastUpdateID = offset
		bt.log.Info("resuming telegram updates", "offset", offset)
	}

	api, err := bot.New(opts.Config.TelegramBotToken, libOpts...)
	if err != nil {
		return nil, fmt.Errorf("telegram: init bot: %w", err)
	}
	bt.api = api
	bt.botID = api.ID()

	me, err := api.GetMe(context.Background())
	if err != nil {
		bt.log.Warn("getMe failed, continuing with the token-derived id", "error", err.Error())
	} else {
		bt.botID = me.ID
		bt.botUsername = strings.ToLower(me.Username)
	}

	return bt, nil
}

// API exposes the underlying library client, so main can hand the same
// connection to the delivery worker.
func (bt *Bot) API() *bot.Bot { return bt.api }

// SetNotifier attaches the delivery worker used by /test.
func (bt *Bot) SetNotifier(n *notifier.Notifier) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	bt.ntf = n
}

func (bt *Bot) notifier() *notifier.Notifier {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	return bt.ntf
}

// BotUsername is the @name used to recognise "/command@thisbot".
func (bt *Bot) BotUsername() string { return bt.botUsername }

// lookupTimeout bounds an on-demand RDB call made from a Telegram handler. It
// is deliberately shorter than a poll: a user waiting for /task should get an
// answer or an error quickly, and the handler goroutine must not pile up.
func (bt *Bot) lookupTimeout() time.Duration {
	if bt.cfg != nil && bt.cfg.HTTPTimeout > 0 {
		return min(bt.cfg.HTTPTimeout, 20*time.Second)
	}
	return 20 * time.Second
}

// Run starts the update loop in the configured mode and blocks until ctx ends.
func (bt *Bot) Run(ctx context.Context) error {
	bt.mu.Lock()
	bt.running = true
	bt.mu.Unlock()
	defer func() {
		bt.mu.Lock()
		bt.running = false
		bt.mu.Unlock()
		if bt.metrics != nil {
			bt.metrics.TelegramLoopRunning.Set(0)
		}
	}()
	if bt.metrics != nil {
		bt.metrics.TelegramLoopRunning.Set(1)
	}

	switch bt.cfg.TelegramMode {
	case config.ModeWebhook:
		// The HTTP listener is owned by main; here we only run the worker
		// pool that consumes updates pushed in by WebhookHandler.
		bt.log.Info("telegram webhook mode: waiting for updates", "path", bt.cfg.WebhookPath())
		done := make(chan struct{})
		go func() {
			bt.api.StartWebhook(ctx)
			close(done)
		}()
		select {
		case <-ctx.Done():
			<-done
			return nil
		case <-done:
			return nil
		}
	default:
		bt.log.Info("telegram long polling started", "bot", bt.botUsername)
		done := make(chan struct{})
		go func() {
			bt.api.Start(ctx)
			close(done)
		}()
		<-done
		return nil
	}
}

// WebhookHandler returns the HTTP handler for webhook mode. It is mounted on
// the health listener so no second port is opened.
func (bt *Bot) WebhookHandler() http.Handler {
	h := bt.api.WebhookHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Reject an oversized body before the library reads it. A Telegram
		// update is a few kilobytes at most.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	})
}

// HandleUpdate is the single entry point registered with the library. Routing
// is done here rather than through pattern handlers because access control and
// offset bookkeeping must happen for every update, matched or not.
func (bt *Bot) HandleUpdate(ctx context.Context, _ *bot.Bot, u *models.Update) {
	if u == nil {
		return
	}
	bt.updates.Add(1)

	if u.ID > 0 {
		bt.mu.Lock()
		stale := u.ID <= bt.lastUpdateID
		bt.mu.Unlock()
		if stale {
			bt.log.Debug("ignoring already processed update", "update_id", u.ID)
			return
		}
		bt.mu.Lock()
		bt.lastUpdateID = u.ID
		bt.lastUpdateAt = bt.clock.Now()
		bt.mu.Unlock()
		if err := bt.store.SaveTelegramOffset(ctx, u.ID); err != nil {
			// Not fatal: the library also tracks the offset in memory, and
			// Telegram only redelivers unconfirmed updates.
			bt.log.Warn("persisting telegram offset failed", "update_id", u.ID, "error", err.Error())
		}
	}

	// Handlers must never panic into the library's worker goroutine.
	defer func() {
		if p := recover(); p != nil {
			bt.log.Error("panic in update handler", "update_id", u.ID, "panic", fmt.Sprint(p))
			bt.setLastError(fmt.Sprintf("panic: %v", p))
		}
	}()

	switch {
	case u.CallbackQuery != nil:
		bt.callbacks.Add(1)
		bt.handleCallback(ctx, u.CallbackQuery)
	case u.Message != nil:
		bt.handleMessage(ctx, u.Message)
	case u.MyChatMember != nil:
		bt.handleMyChatMember(ctx, u.MyChatMember)
	default:
		bt.log.Debug("ignoring update kind", "update_id", u.ID)
	}
}

// TelegramHealth implements health.Telegram.
func (bt *Bot) TelegramHealth(ctx context.Context) model.TelegramHealth {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	h := model.TelegramHealth{
		Mode:         string(bt.cfg.TelegramMode),
		Running:      bt.running,
		LastUpdateID: bt.lastUpdateID,
	}
	if !bt.lastUpdateAt.IsZero() {
		h.LastUpdateAt = model.FormatTime(bt.lastUpdateAt)
	}
	h.LastError = bt.lastError
	return h
}

// Counters reports update statistics for tests and metrics.
func (bt *Bot) Counters() (updates, callbacks, commands, rejected uint64) {
	return bt.updates.Load(), bt.callbacks.Load(), bt.commands.Load(), bt.rejected.Load()
}

func (bt *Bot) setLastError(s string) {
	if len(s) > 256 {
		s = s[:256]
	}
	bt.mu.Lock()
	bt.lastError = s
	bt.mu.Unlock()
}

// ---------------------------------------------------------------------------
// send helpers: every one of them logs instead of failing the handler, because
// there is nothing useful to do with a chat we cannot write to.
// ---------------------------------------------------------------------------

func (bt *Bot) send(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) {
	if strings.TrimSpace(text) == "" {
		return
	}
	text = notifier.ClampText(text)
	_, err := bt.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: notifier.PreviewOff(),
		ReplyMarkup:        markup,
	})
	if err != nil {
		bt.log.Warn("sendMessage failed", "chat_id", chatID, "error", err.Error())
		bt.setLastError(err.Error())
		bt.noteUnreachable(ctx, chatID, err)
	}
}

// edit replaces the text and keyboard of one of our messages. "message is not
// modified" is not an error: the user tapped a control that changes nothing.
func (bt *Bot) edit(ctx context.Context, chatID int64, messageID int, text string, markup models.ReplyMarkup) error {
	text = notifier.ClampText(text)
	_, err := bt.api.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:             chatID,
		MessageID:          messageID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: notifier.PreviewOff(),
		ReplyMarkup:        markup,
	})
	if err == nil {
		return nil
	}
	if isNotModified(err) {
		return nil
	}
	bt.log.Warn("editMessageText failed", "chat_id", chatID, "message_id", messageID, "error", err.Error())
	bt.setLastError(err.Error())
	bt.noteUnreachable(ctx, chatID, err)
	return err
}

// answer acknowledges a callback query. Telegram keeps the client spinner
// running until this arrives, so it is called on every path, including errors.
func (bt *Bot) answer(ctx context.Context, callbackID, text string, alert bool) {
	if callbackID == "" {
		return
	}
	_, err := bt.api.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: callbackID,
		Text:            text,
		ShowAlert:       alert,
	})
	if err != nil {
		bt.log.Debug("answerCallbackQuery failed", "error", err.Error())
	}
}

// isCommandForAnotherBot reports text that looks like a slash command but is
// addressed to a different bot by name.
func isCommandForAnotherBot(text, botUsername string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") || botUsername == "" {
		return false
	}
	head, _, _ := strings.Cut(text, " ")
	_, bot, found := strings.Cut(strings.TrimPrefix(head, "/"), "@")
	return found && !strings.EqualFold(bot, botUsername)
}

// isNotModified detects Telegram's "nothing changed" answer to an edit.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "message is not modified")
}

// noteUnreachable disables a chat after Telegram says the bot was blocked or
// the chat vanished, so the delivery worker stops hammering a dead endpoint.
func (bt *Bot) noteUnreachable(ctx context.Context, chatID int64, err error) {
	if !errors.Is(err, bot.ErrorForbidden) && !errors.Is(err, bot.ErrorUnauthorized) {
		return
	}
	if derr := bt.store.DisableChat(ctx, chatID, err.Error()); derr != nil {
		bt.log.Error("disabling unreachable chat failed", "chat_id", chatID, "error", derr.Error())
		return
	}
	if n, cerr := bt.store.CancelPendingNotifications(ctx, chatID, err.Error()); cerr != nil {
		bt.log.Error("cancelling queued notifications failed", "chat_id", chatID, "error", cerr.Error())
	} else if n > 0 {
		bt.log.Info("cancelled queued notifications for an unreachable chat", "chat_id", chatID, "count", n)
	}
	bt.log.Warn("chat disabled after a permanent Telegram error", "chat_id", chatID, "error", err.Error())
}
