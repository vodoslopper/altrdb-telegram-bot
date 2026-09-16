package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/notifier"
	"altrdb-telegram-bot/internal/poller"
	"altrdb-telegram-bot/internal/rdb"
	"altrdb-telegram-bot/internal/store"
)

// e2eRDB is a minimal stand-in for the ALT RDB API. It holds the current task
// feed in memory so a test can move a task through its lifecycle and then serve
// exactly the same answer again, which is what a restart looks like from the
// outside.
type e2eRDB struct {
	mu       sync.Mutex
	branches []string
	feed     map[string][]rdb.TaskElement
	maints   []rdb.MaintainerEntry
	requests map[string]int
}

func newE2ERDB() *e2eRDB {
	return &e2eRDB{
		branches: []string{"sisyphus", "p11"},
		feed:     map[string][]rdb.TaskElement{},
		maints: []rdb.MaintainerEntry{
			{PackagerName: "Georgy Gagarin", PackagerNickname: "grenka", CountSourcePkg: 42},
		},
		requests: map[string]int{},
	}
}

func (f *e2eRDB) set(branch string, tasks ...rdb.TaskElement) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feed[branch] = tasks
}

func (f *e2eRDB) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[key]
}

func (f *e2eRDB) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(rdb.PathAllBranches, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.requests["branches"]++
		branches := f.branches
		f.mu.Unlock()
		writeE2EJSON(w, rdb.BranchesResponse{Length: len(branches), Branches: branches})
	})
	mux.HandleFunc(rdb.PathLastTasks, func(w http.ResponseWriter, r *http.Request) {
		branch := r.URL.Query().Get("branch")
		f.mu.Lock()
		f.requests["tasks:"+branch]++
		feed := f.feed[branch]
		f.mu.Unlock()
		writeE2EJSON(w, rdb.TasksListResponse{
			RequestArgs: map[string]any{"branch": branch},
			Length:      len(feed),
			Tasks:       feed,
		})
	})
	mux.HandleFunc(rdb.PathMaintainers, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.requests["maintainers"]++
		maints := f.maints
		f.mu.Unlock()
		writeE2EJSON(w, rdb.MaintainersResponse{Length: len(maints), Maintainers: maints})
	})
	return mux
}

func writeE2EJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// system is the whole application wired against fakes. It can be closed and
// rebuilt on the same database file, which is how the tests model a restart.
type system struct {
	seq    int64
	store  *store.Store
	tg     *Bot
	ntf    *notifier.Notifier
	p      *poller.Poller
	api    *fakeTelegram
	feed   *e2eRDB
	clock  *clock.Fake
	cfg    *config.Config
	ctx    context.Context
	cancel context.CancelFunc
}

func newSystem(t *testing.T, dbPath string, rdbSrv *httptest.Server, feed *e2eRDB, api *fakeTelegram) *system {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	cl := clock.NewFake(epoch)
	st, err := store.Open(ctx, dbPath, store.Options{Migrate: true, Clock: cl, Logger: discardLogger()})
	if err != nil {
		cancel()
		t.Fatalf("opening the store: %v", err)
	}

	cfg := &config.Config{
		TelegramBotToken:       testToken,
		TelegramMode:           config.ModePolling,
		RDBBaseURL:             rdbSrv.URL,
		PollInterval:           time.Minute,
		RDBTasksLimit:          200,
		OverlapWindow:          24 * time.Hour,
		MaxConcurrentBranches:  2,
		BranchRefreshInterval:  time.Hour,
		MaintainerRefreshEvery: time.Hour,
		SnapshotKeepFor:        30 * 24 * time.Hour,
		TerminalStates:         model.DefaultTerminalStates,
		HTTPListenAddr:         "127.0.0.1:0",
		HTTPTimeout:            5 * time.Second,
		LogLevel:               "error",
		Version:                "test",
	}

	s := &system{store: st, feed: feed, api: api, clock: cl, cfg: cfg, ctx: ctx, cancel: cancel}
	s.attachRDB(t, rdbSrv)
	return s
}

// attachRDB (re)builds the client, poller, bot and delivery worker.
func (s *system) attachRDB(t *testing.T, srv *httptest.Server) {
	t.Helper()
	client := rdb.NewClient(rdb.Options{
		BaseURL: srv.URL, HTTPClient: srv.Client(), MaxRetries: 0,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		MinRequestSpacing: 0, Logger: discardLogger(), Clock: s.clock,
		Jitter: func() float64 { return 0.5 },
	})
	s.cfg.RDBBaseURL = srv.URL
	s.p = poller.New(client, s.store, s.cfg, discardLogger(), s.clock, nil)

	tg, err := New(Options{
		Config: s.cfg, Store: s.store, Clock: s.clock, Logger: discardLogger(),
		ServerURL: s.api.serverURL, HTTPClient: &http.Client{Timeout: 30 * time.Second},
		SyncHandlers: true, PageSize: 8, Refresher: s.p, Tasks: client,
	})
	if err != nil {
		t.Fatalf("telegram.New: %v", err)
	}
	s.tg = tg
	s.ntf = notifier.New(s.store, tg.API(), s.clock, discardLogger(), nil, notifier.Options{
		Interval: time.Second, MaxAttempts: 3, BackoffBase: time.Second,
		Jitter: func() float64 { return 0.5 },
	})
	tg.SetNotifier(s.ntf)
}

func (s *system) close(t *testing.T) {
	t.Helper()
	s.cancel()
	if err := s.store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
}

// poll runs one ingestion cycle and then drains the outbox, which is what the
// running service does on its timers.
func (s *system) poll(t *testing.T) {
	t.Helper()
	if err := s.p.Cycle(s.ctx); err != nil {
		t.Logf("poll cycle reported: %v", err)
	}
	s.ntf.Drain(s.ctx)
}

func (s *system) say(text string) {
	s.seq++
	s.tg.HandleUpdate(s.ctx, s.tg.API(), &models.Update{
		ID: s.seq,
		Message: &models.Message{
			ID: testMessageID, Date: int(s.clock.Now().Unix()), Text: text,
			Chat: models.Chat{ID: testChatID, Type: models.ChatTypePrivate, FirstName: "Test", Username: "tester"},
			From: &models.User{ID: testUserID, FirstName: "Test", Username: "tester"},
		},
	})
}

func (s *system) press(data string) {
	s.seq++
	s.tg.HandleUpdate(s.ctx, s.tg.API(), &models.Update{
		ID: s.seq,
		CallbackQuery: &models.CallbackQuery{
			ID:   "cbq-e2e",
			From: models.User{ID: testUserID, Username: "tester"},
			Message: models.MaybeInaccessibleMessage{
				Type: models.MaybeInaccessibleMessageTypeMessage,
				Message: &models.Message{
					ID: testMessageID, Date: int(s.clock.Now().Unix()),
					Chat: models.Chat{ID: testChatID, Type: models.ChatTypePrivate},
					From: &models.User{ID: testBotID, Username: testBotName, IsBot: true},
				},
			},
			ChatInstance: "ci",
			Data:         data,
		},
	})
}

func (s *system) sent() []call { return s.api.byMethod("sendMessage") }

// findCheckbox returns the callback data of a ☐ button with the given label.
func findCheckbox(t *testing.T, api *fakeTelegram, label string) string {
	t.Helper()
	want := emptyBox + " " + label
	for _, c := range api.recorded() {
		for _, b := range c.buttonsFull() {
			if b.Text == want {
				return b.CallbackData
			}
		}
	}
	t.Fatalf("no ☐ %s button was offered; buttons seen: %v", label, buttonTexts(api))
	return ""
}

func buttonTexts(api *fakeTelegram) []string {
	var out []string
	for _, c := range api.recorded() {
		for _, b := range c.buttonsFull() {
			out = append(out, b.Text)
		}
	}
	return out
}

func allTextsOf(api *fakeTelegram) []string {
	var out []string
	for _, c := range api.recorded() {
		if t := c.Plain(); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func e2eTask(id int64, branch, owner, state string, changed time.Time) rdb.TaskElement {
	return rdb.TaskElement{
		TaskID: id, TaskRepo: branch, TaskState: state, TaskOwner: owner,
		TaskTry: 1, TaskIter: 1, TaskChanged: changed.Format(time.RFC3339),
		TaskStage: "task-save-repo",
		Subtasks:  []rdb.Subtask{{SubtaskSrpmName: "postfix", SubtaskSrpmEvr: "3.8.21-alt1"}},
	}
}

// TestEndToEndSubscriptionToSingleDelivery walks the whole product promise:
// a user configures the bot from inside Telegram, the feed is baselined, one
// transition produces exactly one message, and neither a repeated poll nor a
// full process restart produces a second one.
func TestEndToEndSubscriptionToSingleDelivery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")

	feed := newE2ERDB()
	rdbSrv := httptest.NewServer(feed.Handler())
	defer rdbSrv.Close()
	api := newFakeTelegram()
	tgSrv := httptest.NewServer(api.Handler())
	defer tgSrv.Close()
	api.serverURL = tgSrv.URL

	sys := newSystem(t, dbPath, rdbSrv, feed, api)

	// ---- 1. registration and configuration, entirely inside Telegram ----
	sys.say("/start")
	if _, err := sys.store.GetChat(sys.ctx, testChatID); err != nil {
		t.Fatalf("the chat was not registered: %v", err)
	}

	sys.say("/maintainers grenka")
	nicks, err := sys.store.ListMaintainers(sys.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListMaintainers: %v", err)
	}
	if len(nicks) != 1 || nicks[0] != "grenka" {
		t.Fatalf("maintainers = %v", nicks)
	}

	// The states panel must offer the seeded lifecycle states as checkboxes,
	// all of them on one screen: FAILED and DONE are two buttons in the same
	// two-column list.
	api.reset()
	sys.say("/states")
	failedData := findCheckbox(t, api, "FAILED")
	doneData := findCheckbox(t, api, "DONE")
	api.reset()
	sys.press(failedData)
	// A page button from an older, paginated panel must still work.
	sys.press("pg:st:1")
	sys.press(doneData)

	states, err := sys.store.ListChatStates(sys.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListChatStates: %v", err)
	}
	if strings.Join(states, ",") != "FAILED,DONE" {
		t.Fatalf("states = %v, want FAILED and DONE", states)
	}

	// The branches panel must be populated from the RDB catalogue.
	api.reset()
	sys.say("/branches")
	branchData := findCheckbox(t, api, "sisyphus")
	if feed.count("branches") == 0 {
		t.Error("the branch catalogue was never fetched from RDB")
	}
	api.reset()
	sys.press(branchData)

	subs, err := sys.store.LoadSubscription(sys.ctx, testChatID)
	if err != nil {
		t.Fatalf("LoadSubscription: %v", err)
	}
	if !subs.Complete() {
		t.Fatalf("the subscription is still incomplete: %v", subs.Missing())
	}
	watermark := subs.Branches[0].ActiveWatermark
	if watermark.IsZero() {
		t.Fatal("checking a branch must stamp an activation watermark")
	}

	// ---- 2. baseline: the feed already holds finished tasks ----
	feed.set("sisyphus",
		e2eTask(433219, "sisyphus", "grenka", "DONE", epoch.Add(-3*time.Hour)),
		e2eTask(433220, "sisyphus", "rider", "DONE", epoch.Add(-2*time.Hour)),
	)
	api.reset()
	sys.poll(t)

	if got := len(sys.sent()); got != 0 {
		t.Fatalf("the baseline sent %d messages; configuring the bot must not replay history:\n%s",
			got, strings.Join(allTextsOf(api), "\n"))
	}
	n, err := sys.store.CountSnapshots(sys.ctx)
	if err != nil {
		t.Fatalf("CountSnapshots: %v", err)
	}
	if n != 2 {
		t.Errorf("snapshots = %d, want both feed records stored", n)
	}
	// p11 is advertised by RDB but nobody follows it, so it is never polled.
	if got := feed.count("tasks:p11"); got != 0 {
		t.Errorf("p11 was polled %d times", got)
	}

	// ---- 3. a real transition produces exactly one message ----
	sys.clock.Advance(time.Minute)
	feed.set("sisyphus",
		e2eTask(433219, "sisyphus", "grenka", "DONE", epoch.Add(-3*time.Hour)), // unchanged
		e2eTask(433220, "sisyphus", "rider", "DONE", epoch.Add(-2*time.Hour)),  // another maintainer
		// A brand new task, changed after the activation watermark. A record
		// stamped exactly at the watermark is deliberately not notified: with
		// second-resolution timestamps "equal" cannot be told from "earlier".
		e2eTask(433221, "sisyphus", "grenka", "DONE", sys.clock.Now()),
	)
	api.reset()
	sys.poll(t)

	sends := sys.sent()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want exactly one notification:\n%s",
			len(sends), strings.Join(allTextsOf(api), "\n---\n"))
	}
	text := sends[0].Plain()
	for _, want := range []string{"Task #433221", "DONE", "sisyphus", "@grenka", "Try/iteration: 1/1"} {
		if !strings.Contains(text, want) {
			t.Errorf("the notification is missing %q:\n%s", want, text)
		}
	}
	urls := sends[0].URLs()
	if len(urls) != 1 || urls[0] != "https://git.altlinux.org/tasks/433221/" {
		t.Errorf("task link buttons = %v, want the Girar URL for the task", urls)
	}
	if sends[0].ChatID() != testChatID {
		t.Errorf("sent to chat %d", sends[0].ChatID())
	}

	// ---- 4. repeated polls of an unchanged feed add nothing ----
	for range 3 {
		api.reset()
		sys.poll(t)
		if got := len(sys.sent()); got != 0 {
			t.Fatalf("a repeated poll sent %d more messages", got)
		}
	}

	// ---- 5. a full process restart against the same API response ----
	offsetBefore, err := sys.store.TelegramOffset(sys.ctx)
	if err != nil {
		t.Fatalf("TelegramOffset: %v", err)
	}
	if offsetBefore == 0 {
		t.Error("the update offset was never persisted")
	}
	sys.close(t)

	restarted := newSystem(t, dbPath, rdbSrv, feed, api)
	defer restarted.close(t)
	// A restart does not rewind the wall clock.
	restarted.clock.Set(epoch.Add(10 * time.Minute))

	offsetAfter, err := restarted.store.TelegramOffset(restarted.ctx)
	if err != nil {
		t.Fatalf("TelegramOffset after restart: %v", err)
	}
	if offsetAfter != offsetBefore {
		t.Errorf("the update offset changed across the restart: %d -> %d", offsetBefore, offsetAfter)
	}
	subs, err = restarted.store.LoadSubscription(restarted.ctx, testChatID)
	if err != nil {
		t.Fatalf("LoadSubscription after restart: %v", err)
	}
	if !subs.Complete() {
		t.Fatalf("the subscription was lost across the restart: %v", subs.Missing())
	}
	if !subs.Branches[0].ActiveWatermark.Equal(watermark) {
		t.Errorf("the watermark moved across the restart: %s -> %s",
			watermark, subs.Branches[0].ActiveWatermark)
	}

	api.reset()
	restarted.poll(t)
	if got := len(restarted.sent()); got != 0 {
		t.Fatalf("the restart replayed %d notifications", got)
	}

	// A Telegram update replayed from before the restart is ignored too.
	replayed := &models.Update{ID: 1, Message: &models.Message{
		ID: testMessageID, Date: int(restarted.clock.Now().Unix()), Text: "/help",
		Chat: models.Chat{ID: testChatID, Type: models.ChatTypePrivate},
		From: &models.User{ID: testUserID},
	}}
	api.reset()
	restarted.tg.HandleUpdate(restarted.ctx, restarted.tg.API(), replayed)
	if got := len(api.recorded()); got != 0 {
		t.Errorf("a replayed update produced %d API calls", got)
	}

	// ---- 6. after the restart a new transition is still delivered once ----
	restarted.clock.Advance(time.Minute)
	feed.set("sisyphus", e2eTask(433221, "sisyphus", "grenka", "FAILED", restarted.clock.Now()))
	api.reset()
	restarted.poll(t)

	sends = restarted.sent()
	if len(sends) != 1 {
		t.Fatalf("after the restart, sends = %d, want 1", len(sends))
	}
	if !strings.Contains(sends[0].Plain(), "DONE → FAILED") {
		t.Errorf("the transition was not rendered:\n%s", sends[0].Plain())
	}

	var rows int
	if err := restarted.store.DB().QueryRowContext(restarted.ctx,
		`SELECT COUNT(*) FROM notifications`).Scan(&rows); err != nil {
		t.Fatalf("counting outbox rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("outbox rows = %d, want exactly one per delivered event", rows)
	}
}

// TestEndToEndTelegramOutageKeepsQueuedNotifications checks that a Telegram
// outage does not lose anything: the row stays in the outbox until it can go
// out, and then goes out exactly once.
func TestEndToEndTelegramOutageKeepsQueuedNotifications(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	feed := newE2ERDB()
	rdbSrv := httptest.NewServer(feed.Handler())
	defer rdbSrv.Close()
	api := newFakeTelegram()
	tgSrv := httptest.NewServer(api.Handler())
	defer tgSrv.Close()
	api.serverURL = tgSrv.URL

	sys := newSystem(t, dbPath, rdbSrv, feed, api)
	defer sys.close(t)

	sys.say("/start")
	sys.say("/maintainers grenka")
	if err := sys.store.SetAllChatStates(sys.ctx, testChatID, []string{"DONE"}); err != nil {
		t.Fatalf("SetAllChatStates: %v", err)
	}
	if _, err := sys.store.AddChatBranch(sys.ctx, testChatID, "sisyphus"); err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}

	api.reset()
	feed.set("sisyphus", e2eTask(1, "sisyphus", "grenka", "NEW", epoch.Add(-time.Hour)))
	sys.poll(t)
	if got := len(sys.sent()); got != 0 {
		t.Fatalf("the baseline sent %d messages", got)
	}

	// A transition while Telegram is answering with a server error.
	api.failWith("sendMessage", 500, 1, "Internal Server Error")
	api.reset()
	sys.clock.Advance(time.Minute)
	feed.set("sisyphus", e2eTask(1, "sisyphus", "grenka", "DONE", sys.clock.Now()))
	sys.poll(t)

	// One attempt was made, and it must not be recorded as delivered.
	if got := len(sys.sent()); got != 1 {
		t.Errorf("send attempts = %d, want exactly one", got)
	}
	counts, err := sys.store.OutboxCounts(sys.ctx)
	if err != nil {
		t.Fatalf("OutboxCounts: %v", err)
	}
	if counts.Pending != 1 {
		t.Errorf("pending = %d, want the failed notification kept for retry", counts.Pending)
	}
	var sentRows int
	if err := sys.store.DB().QueryRowContext(sys.ctx,
		`SELECT COUNT(*) FROM notifications WHERE status = 'sent'`).Scan(&sentRows); err != nil {
		t.Fatalf("counting sent rows: %v", err)
	}
	if sentRows != 0 {
		t.Errorf("sent rows = %d; a failed send must not be marked delivered", sentRows)
	}

	// Once Telegram recovers and the row is due again, it goes out exactly once.
	sys.clock.Advance(time.Hour)
	api.reset()
	sys.ntf.Drain(sys.ctx)

	sends := sys.sent()
	if len(sends) != 1 {
		t.Fatalf("sends after recovery = %d, want 1", len(sends))
	}
	if !strings.Contains(sends[0].Plain(), "Task #1") {
		t.Errorf("unexpected text:\n%s", sends[0].Plain())
	}
	var deliveredRows int
	if err := sys.store.DB().QueryRowContext(sys.ctx,
		`SELECT COUNT(*) FROM notifications WHERE status = 'sent'`).Scan(&deliveredRows); err != nil {
		t.Fatalf("counting sent rows: %v", err)
	}
	if deliveredRows != 1 {
		t.Errorf("sent rows = %d, want 1", deliveredRows)
	}

	// And a further drain does not resend it.
	api.reset()
	sys.clock.Advance(time.Hour)
	sys.ntf.Drain(sys.ctx)
	if got := len(sys.sent()); got != 0 {
		t.Errorf("a delivered notification was sent again: %d", got)
	}
}

// TestEndToEndRDBOutageKeepsState checks the other side: RDB going away must
// leave the cached catalogues and the snapshots intact, and the UI must still
// work from the cache.
func TestEndToEndRDBOutageKeepsState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	feed := newE2ERDB()
	rdbSrv := httptest.NewServer(feed.Handler())
	defer rdbSrv.Close()
	api := newFakeTelegram()
	tgSrv := httptest.NewServer(api.Handler())
	defer tgSrv.Close()
	api.serverURL = tgSrv.URL

	sys := newSystem(t, dbPath, rdbSrv, feed, api)
	defer sys.close(t)

	sys.say("/start")
	if err := sys.store.SetAllChatStates(sys.ctx, testChatID, []string{"DONE"}); err != nil {
		t.Fatalf("SetAllChatStates: %v", err)
	}
	if _, err := sys.store.AddChatBranch(sys.ctx, testChatID, "sisyphus"); err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}

	feed.set("sisyphus", e2eTask(1, "sisyphus", "grenka", "DONE", epoch.Add(-time.Hour)))
	sys.poll(t)
	before, err := sys.store.CountSnapshots(sys.ctx)
	if err != nil {
		t.Fatalf("CountSnapshots: %v", err)
	}
	branchesBefore, err := sys.store.BranchNames(sys.ctx)
	if err != nil {
		t.Fatalf("BranchNames: %v", err)
	}

	// RDB now fails every request.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"upstream unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	sys.attachRDB(t, failing)

	for range 2 {
		sys.clock.Advance(time.Minute)
		if err := sys.p.Cycle(sys.ctx); err == nil {
			t.Error("a failing feed must be reported by the cycle")
		}
	}

	after, err := sys.store.CountSnapshots(sys.ctx)
	if err != nil {
		t.Fatalf("CountSnapshots: %v", err)
	}
	if after != before {
		t.Errorf("snapshots went from %d to %d during an outage", before, after)
	}
	st, err := sys.store.GetPollState(sys.ctx, "sisyphus")
	if err != nil {
		t.Fatalf("GetPollState: %v", err)
	}
	if st.ConsecutiveFailures < 2 {
		t.Errorf("consecutive failures = %d, want at least 2", st.ConsecutiveFailures)
	}
	if st.LastSuccessAt == nil {
		t.Error("the last successful poll must be remembered through an outage")
	}
	if st.LastError == "" {
		t.Error("the failure reason must be recorded")
	}

	branchesAfter, err := sys.store.BranchNames(sys.ctx)
	if err != nil {
		t.Fatalf("BranchNames: %v", err)
	}
	if strings.Join(branchesBefore, ",") != strings.Join(branchesAfter, ",") {
		t.Errorf("the cached branch list changed during an outage: %v -> %v", branchesBefore, branchesAfter)
	}

	// The branch selector still renders from the cache.
	api.reset()
	sys.say("/branches")
	if !contains(buttonTexts(api), checkedBox+" sisyphus") {
		t.Errorf("the cached branch list disappeared: %v", buttonTexts(api))
	}
}
