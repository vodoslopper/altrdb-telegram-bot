package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/notifier"
	"altrdb-telegram-bot/internal/store"
)

// Test constants for the fake Telegram server.
const (
	testToken     = "123456:TEST-TOKEN"
	testBotID     = int64(123456)
	testBotName   = "altrdb_notify_bot"
	testChatID    = int64(4242)
	testUserID    = int64(4242)
	testMessageID = 777
)

var epoch = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// call is one Bot API request the fake server received.
type call struct {
	Method string
	Form   url.Values
}

// ChatID returns the chat the call was addressed to.
func (c call) ChatID() int64 {
	v := c.Form.Get("chat_id")
	var id int64
	fmt.Sscanf(v, "%d", &id)
	return id
}

// Text is the message text of a send or edit.
func (c call) Text() string { return c.Form.Get("text") }

// Plain is the message text with all markup removed, for readable assertions.
func (c call) Plain() string { return stripMarkup(c.Text()) }

// keyboard parses the call's inline keyboard.
func (c call) keyboard() models.InlineKeyboardMarkup {
	var markup models.InlineKeyboardMarkup
	raw := c.Form.Get("reply_markup")
	if raw == "" {
		return markup
	}
	_ = json.Unmarshal([]byte(raw), &markup)
	return markup
}

// buttonsFull returns every inline button, labels and callback data together.
func (c call) buttonsFull() []models.InlineKeyboardButton {
	var out []models.InlineKeyboardButton
	for _, row := range c.keyboard().InlineKeyboard {
		out = append(out, row...)
	}
	return out
}

// Buttons lists every inline button label of the call's keyboard.
func (c call) Buttons() []string {
	raw := c.Form.Get("reply_markup")
	if raw == "" {
		return nil
	}
	var markup models.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		return nil
	}
	var out []string
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			out = append(out, b.Text)
		}
	}
	return out
}

// URLs lists the URL values of the call's keyboard buttons.
func (c call) URLs() []string {
	var out []string
	for _, row := range c.keyboard().InlineKeyboard {
		for _, b := range row {
			if b.URL != "" {
				out = append(out, b.URL)
			}
		}
	}
	return out
}

// CallbackData lists the callback_data values of the call's keyboard.
func (c call) CallbackData() []string {
	raw := c.Form.Get("reply_markup")
	if raw == "" {
		return nil
	}
	var markup models.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		return nil
	}
	var out []string
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData != "" {
				out = append(out, b.CallbackData)
			}
		}
	}
	return out
}

func stripMarkup(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&amp;", "&")
	return out
}

// failure scripts an error answer for a Bot API method.
type failure struct {
	errorCode   int
	description string
	retryAfter  int
	times       int // 0 means "every call"
}

// fakeTelegram is an in-process stand-in for the Telegram Bot API. It records
// every call and answers with well-formed results so the real library client
// is exercised end to end without any network.
type fakeTelegram struct {
	mu        sync.Mutex
	calls     []call
	failures  map[string][]failure
	msgSeq    int
	serverURL string
}

func newFakeTelegram() *fakeTelegram {
	return &fakeTelegram{failures: map[string][]failure{}}
}

// failWith scripts an error answer for the next `times` calls of a method.
func (f *fakeTelegram) failWith(method string, errorCode, times int, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = append(f.failures[method], failure{
		errorCode: errorCode, description: description, times: times,
	})
}

func (f *fakeTelegram) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call{}, f.calls...)
}

func (f *fakeTelegram) byMethod(method string) []call {
	var out []call
	for _, c := range f.recorded() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeTelegram) lastByMethod(method string) (call, bool) {
	calls := f.byMethod(method)
	if len(calls) == 0 {
		return call{}, false
	}
	return calls[len(calls)-1], true
}

func (f *fakeTelegram) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// takeFailure pops one scripted failure for a method, if any.
func (f *fakeTelegram) takeFailure(method string) (failure, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	queue := f.failures[method]
	if len(queue) == 0 {
		return failure{}, false
	}
	next := queue[0]
	if next.times > 0 {
		next.times--
	}
	if next.times == 0 {
		f.failures[method] = queue[1:]
	} else {
		f.failures[method][0] = next
	}
	return next, true
}

func (f *fakeTelegram) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]

		if err := r.ParseMultipartForm(1 << 20); err != nil {
			// Some methods arrive without a body.
			_ = r.ParseForm()
		}

		f.mu.Lock()
		f.calls = append(f.calls, call{Method: method, Form: r.PostForm})
		f.msgSeq++
		seq := f.msgSeq
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail, ok := f.takeFailure(method); ok {
			body := map[string]any{
				"ok":          false,
				"error_code":  fail.errorCode,
				"description": fail.description,
			}
			if fail.retryAfter > 0 {
				body["parameters"] = map[string]any{"retry_after": fail.retryAfter}
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}

		var result any = true
		switch method {
		case "getMe":
			result = models.User{ID: testBotID, Username: testBotName, FirstName: "ALT RDB Bot", IsBot: true}
		case "getUpdates":
			result = []any{}
		case "sendMessage", "editMessageText":
			chatID := int64(testChatID)
			fmt.Sscanf(r.PostForm.Get("chat_id"), "%d", &chatID)
			result = models.Message{
				ID:   1000 + seq,
				Date: int(time.Now().Unix()),
				Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate},
				From: &models.User{ID: testBotID, Username: testBotName, IsBot: true},
				Text: r.PostForm.Get("text"),
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	})
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	seq   int64
	t     *testing.T
	cfg   *config.Config
	store *store.Store
	clock *clock.Fake
	api   *fakeTelegram
	tg    *Bot
	ntf   *notifier.Notifier
	ctx   context.Context
}

// newHarness wires a bot against the fake Telegram server and a fresh database.
func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	api := newFakeTelegram()
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	cl := clock.NewFake(epoch)
	st, err := store.Open(context.Background(), t.TempDir()+"/bot.db", store.Options{
		Migrate: true, Clock: cl, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		TelegramBotToken: testToken,
		TelegramMode:     config.ModePolling,
		RDBBaseURL:       "https://rdb.invalid/api",
		TerminalStates:   []string{"DONE", "FAILED"},
		HTTPListenAddr:   "127.0.0.1:0",
		HTTPTimeout:      2 * time.Second,
		LogLevel:         "error",
		Version:          "test",
	}
	if mutate != nil {
		mutate(cfg)
	}

	tg, err := New(Options{
		Config:       cfg,
		Store:        st,
		Clock:        cl,
		Logger:       discardLogger(),
		ServerURL:    srv.URL,
		HTTPClient:   srv.Client(),
		SyncHandlers: true,
		PageSize:     4, // small on purpose, so pagination is exercised
	})
	if err != nil {
		t.Fatalf("telegram.New: %v", err)
	}
	ntf := notifier.New(st, tg.API(), cl, discardLogger(), nil, notifier.Options{
		Interval: time.Second, MaxAttempts: 3, BackoffBase: time.Second,
		Jitter: func() float64 { return 0.5 },
	})
	tg.SetNotifier(ntf)

	api.reset() // drop the getMe handshake from the recorded calls
	return &harness{t: t, cfg: cfg, store: st, clock: cl, api: api, tg: tg, ntf: ntf, ctx: context.Background()}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// message builds a private-chat message update.
func (h *harness) message(text string) *models.Update {
	return h.messageFrom(testChatID, testUserID, text)
}

func (h *harness) messageFrom(chatID, userID int64, text string) *models.Update {
	return &models.Update{
		ID: h.nextUpdateID(),
		Message: &models.Message{
			ID:   testMessageID,
			Date: int(h.clock.Now().Unix()),
			Text: text,
			Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate, FirstName: "Test", Username: "tester"},
			From: &models.User{ID: userID, FirstName: "Test", Username: "tester"},
		},
	}
}

// callback builds a button press on one of the bot's own messages.
func (h *harness) callback(data string) *models.Update {
	return h.callbackIn(testChatID, testUserID, testMessageID, data)
}

func (h *harness) callbackIn(chatID, userID int64, messageID int, data string) *models.Update {
	mim := models.MaybeInaccessibleMessage{
		Type: models.MaybeInaccessibleMessageTypeMessage,
		Message: &models.Message{
			ID:   messageID,
			Date: int(h.clock.Now().Unix()),
			Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate},
			From: &models.User{ID: testBotID, Username: testBotName, IsBot: true},
		},
	}
	return &models.Update{
		ID: h.nextUpdateID(),
		CallbackQuery: &models.CallbackQuery{
			ID:           fmt.Sprintf("cbq-%d", messageID),
			From:         models.User{ID: userID, Username: "tester"},
			Message:      mim,
			ChatInstance: "ci",
			Data:         data,
		},
	}
}

func (h *harness) nextUpdateID() int64 {
	h.seq++
	return h.seq
}

// send feeds a message update to the bot.
func (h *harness) send(text string) {
	h.t.Helper()
	h.tg.HandleUpdate(h.ctx, h.tg.API(), h.message(text))
}

// press feeds a callback update to the bot.
func (h *harness) press(data string) {
	h.t.Helper()
	h.tg.HandleUpdate(h.ctx, h.tg.API(), h.callback(data))
}

// pressUpdate feeds an explicitly built callback update.
func (h *harness) pressUpdate(u *models.Update) {
	h.t.Helper()
	h.tg.HandleUpdate(h.ctx, h.tg.API(), u)
}

// lastText returns the plain text of the most recent send or edit.
func (h *harness) lastText(method string) string {
	h.t.Helper()
	c, ok := h.api.lastByMethod(method)
	if !ok {
		h.t.Fatalf("no %s call was made", method)
	}
	return c.Plain()
}

// subscribe fills a chat's selection directly in the database, bypassing the
// UI, for tests that are about something else.
func (h *harness) subscribe(chatID int64, maintainers, states, branches []string, watermark time.Time) {
	h.t.Helper()
	ctx := h.ctx
	if _, err := h.store.RegisterChat(ctx, chatID, "private", "tester", "Test"); err != nil {
		h.t.Fatalf("RegisterChat: %v", err)
	}
	if _, err := h.store.AddMaintainers(ctx, chatID, maintainers); err != nil {
		h.t.Fatalf("AddMaintainers: %v", err)
	}
	if err := h.store.SetAllChatStates(ctx, chatID, states); err != nil {
		h.t.Fatalf("SetAllChatStates: %v", err)
	}
	for _, b := range branches {
		if _, err := h.store.DB().ExecContext(ctx,
			`INSERT INTO chat_branches (chat_id, branch, active_watermark, created_at) VALUES (?, ?, ?, ?)`,
			chatID, b, model.FormatTime(watermark), model.FormatTime(watermark)); err != nil {
			h.t.Fatalf("inserting chat_branches: %v", err)
		}
	}
}

// stateID resolves a state name to the id the keyboard buttons carry.
func (h *harness) stateID(name string) int64 {
	h.t.Helper()
	id, err := h.store.StateID(h.ctx, name)
	if err != nil {
		h.t.Fatalf("StateID(%s): %v", name, err)
	}
	return id
}

// ensureBranchCatalogue seeds the branch list the way the poller would.
func (h *harness) ensureBranchCatalogue(branches ...string) {
	h.t.Helper()
	if err := h.store.ReplaceBranches(h.ctx, branches); err != nil {
		h.t.Fatalf("ReplaceBranches: %v", err)
	}
}
