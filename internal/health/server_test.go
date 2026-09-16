package health

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/store"
)

type stubPoller struct {
	health model.PollerHealth
}

func (s stubPoller) PollerHealth(context.Context) model.PollerHealth { return s.health }

type stubTelegram struct {
	health model.TelegramHealth
}

func (s stubTelegram) TelegramHealth(context.Context) model.TelegramHealth { return s.health }

type stubNotifier struct {
	running bool
	last    time.Time
	err     error
}

func (s stubNotifier) NotifierHealth(context.Context) (bool, time.Time, error) {
	return s.running, s.last, s.err
}

type stubLease struct {
	holder string
	err    error
}

func (s stubLease) LeaseInfo(context.Context) (string, time.Time, error) {
	return s.holder, time.Now().UTC(), s.err
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/bot.db", store.Options{Migrate: true})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testConfig() *config.Config {
	return &config.Config{
		TelegramBotToken: "1:token",
		HTTPListenAddr:   "127.0.0.1:0",
		Version:          "1.2.3",
		Commit:           "abc1234",
		LogLevel:         "error",
	}
}

func do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLivezAlwaysAnswers(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{Config: testConfig(), Store: st})

	rec := do(t, srv.Handler(), "/livez")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even while other components are down", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestHealthzIsCompleteAndHealthy(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 1, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	now := time.Now().UTC()
	if err := st.SavePollState(ctx, model.PollState{
		Branch: "sisyphus", LastSuccessAt: &now, TasksSeen: 42, Baselined: true,
	}); err != nil {
		t.Fatalf("SavePollState: %v", err)
	}

	srv := NewServer(Options{
		Config:  testConfig(),
		Store:   st,
		Metrics: NewMetrics("1.2.3", "abc1234"),
		Poller: stubPoller{model.PollerHealth{
			Running: true, IntervalSeconds: 60, TasksLimit: 200,
			Branches: []model.PollState{{Branch: "sisyphus", TasksSeen: 42, Baselined: true}},
		}},
		Telegram: stubTelegram{model.TelegramHealth{Mode: "polling", Running: true, LastUpdateID: 99}},
		Notifier: stubNotifier{running: true, last: now},
		Lease:    stubLease{holder: "host:1:bot.db"},
	})

	rec := do(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var h model.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("the body is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if h.Status != "ok" {
		t.Errorf("status = %q", h.Status)
	}
	if h.Version != "1.2.3" || h.Commit != "abc1234" {
		t.Errorf("build identity = %s/%s", h.Version, h.Commit)
	}
	if !h.Database.OK || h.Database.Driver != "sqlite" || h.Database.SchemaVersion < 1 {
		t.Errorf("database = %+v", h.Database)
	}
	if !h.Telegram.Running || h.Telegram.Mode != "polling" || h.Telegram.LastUpdateID != 99 {
		t.Errorf("telegram = %+v", h.Telegram)
	}
	if !h.Poller.Running || len(h.Poller.Branches) != 1 || h.Poller.Branches[0].TasksSeen != 42 {
		t.Errorf("poller = %+v", h.Poller)
	}
	if h.Chats.Total != 1 || h.Chats.Enabled != 1 {
		t.Errorf("chats = %+v", h.Chats)
	}
	if h.Extra["lease_holder"] != "host:1:bot.db" {
		t.Errorf("lease = %v", h.Extra["lease_holder"])
	}
	if h.Extra["notifier_running"] != true {
		t.Errorf("notifier = %v", h.Extra["notifier_running"])
	}
	// The token must never appear anywhere in the document.
	if strings.Contains(rec.Body.String(), "token") {
		t.Errorf("the health document leaked a secret:\n%s", rec.Body.String())
	}
}

func TestReadyzIsUnavailableWhenTelegramIsDown(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Poller:   stubPoller{model.PollerHealth{Running: true}},
		Telegram: stubTelegram{model.TelegramHealth{Mode: "polling", Running: false}},
	})

	rec := do(t, srv.Handler(), "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the update loop is not running", rec.Code)
	}
	var h model.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if h.Status != "degraded" {
		t.Errorf("status = %q", h.Status)
	}
}

func TestReadyzIsUnavailableWhenTheDatabaseIsGone(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Telegram: stubTelegram{model.TelegramHealth{Running: true}},
	})
	if err := st.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	rec := do(t, srv.Handler(), "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with a closed database", rec.Code)
	}
	var h model.Health
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if h.Database.OK {
		t.Error("the database must be reported as down")
	}
	if h.Database.Error == "" {
		t.Error("the failure reason must be included")
	}
}

func TestHealthzReportsAStoppedPollerWithoutFailing(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Telegram: stubTelegram{model.TelegramHealth{Running: true}},
		Poller:   stubPoller{model.PollerHealth{Running: false}},
	})

	rec := do(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; a stopped poller is not a readiness failure", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "poller_note") {
		t.Errorf("the stopped poller should be mentioned:\n%s", rec.Body.String())
	}
}

func TestHealthzWarnsAboutASaturatedWindow(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Telegram: stubTelegram{model.TelegramHealth{Running: true}},
		Poller: stubPoller{model.PollerHealth{
			Running:         true,
			WindowSaturated: true,
			Branches:        []model.PollState{{Branch: "sisyphus", WindowSaturated: true}},
		}},
	})

	rec := do(t, srv.Handler(), "/healthz")
	if !strings.Contains(rec.Body.String(), "window_warning") {
		t.Errorf("a saturated poll window must be surfaced:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "RDB_TASKS_LIMIT") {
		t.Errorf("the warning should name the knob to turn:\n%s", rec.Body.String())
	}
}

func TestHealthzReportsAnOutboxBacklog(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 1, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if err := st.EnqueueTestNotification(ctx, 1, model.TaskEvent{
		Fingerprint: "fp", TaskRepo: "sisyphus", TaskID: 1, State: "DONE", Owner: "grenka",
	}); err != nil {
		t.Fatalf("EnqueueTestNotification: %v", err)
	}

	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Telegram: stubTelegram{model.TelegramHealth{Running: true}},
	})
	rec := do(t, srv.Handler(), "/healthz")

	var h model.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if h.Outbox.Pending != 1 {
		t.Errorf("outbox = %+v, want one pending row", h.Outbox)
	}
}

func TestHealthzReportsComponentErrors(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Telegram: stubTelegram{model.TelegramHealth{Running: true}},
		Notifier: stubNotifier{err: io.ErrUnexpectedEOF},
		Lease:    stubLease{err: io.ErrClosedPipe},
	})

	rec := do(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when a component reports an error", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "notifier_error") || !strings.Contains(body, "lease_error") {
		t.Errorf("component errors are missing:\n%s", body)
	}
}

func TestHealthzWithoutAStore(t *testing.T) {
	srv := NewServer(Options{Config: testConfig()})
	rec := do(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "store not configured") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestMetricsEndpoint(t *testing.T) {
	st := newTestStore(t)
	m := NewMetrics("1.2.3", "abc1234")
	m.PollsTotal.WithLabelValues("sisyphus", "ok").Inc()
	m.OutboxPending.Set(3)
	m.TelegramLoopRunning.Set(1)

	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Metrics:  m,
		Telegram: stubTelegram{model.TelegramHealth{Running: true}},
	})

	rec := do(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`altrdb_polls_total{branch="sisyphus",result="ok"} 1`,
		"altrdb_outbox_pending 3",
		"altrdb_telegram_loop_running 1",
		`altrdb_build_info{commit="abc1234",version="1.2.3"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics are missing %q", want)
		}
	}
	// The default Go process collectors must not leak into our registry.
	if strings.Contains(body, "go_goroutines") {
		t.Error("the private registry picked up the default process collector")
	}
}

func TestMetricsEndpointIsOptional(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{Config: testConfig(), Store: st})
	rec := do(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no registry was configured", rec.Code)
	}
}

func TestMountedHandlersAreServed(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{Config: testConfig(), Store: st})
	srv.Mount("/telegram/webhook", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "hooked")
	}))

	rec := do(t, srv.Handler(), "/telegram/webhook")
	if rec.Code != http.StatusAccepted || rec.Body.String() != "hooked" {
		t.Errorf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestRootRedirectsToHealthz(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{
		Config:   testConfig(),
		Store:    st,
		Telegram: stubTelegram{model.TelegramHealth{Running: true}},
	})
	rec := do(t, srv.Handler(), "/")
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/healthz" {
		t.Errorf("Location = %q", loc)
	}
}

func TestMethodNotAllowedOnHealthEndpoints(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(Options{Config: testConfig(), Store: st})
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", rec.Code)
	}
}

func TestRunStopsOnCancellation(t *testing.T) {
	st := newTestStore(t)
	cfg := testConfig()
	cfg.HTTPListenAddr = "127.0.0.1:0"
	srv := NewServer(Options{Config: cfg, Store: st, Logger: nil})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want a clean stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunReportsABusyPort(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer first.Close()

	addr := strings.TrimPrefix(first.URL, "http://")
	cfg := testConfig()
	cfg.HTTPListenAddr = addr
	srv := NewServer(Options{Config: cfg, Store: newTestStore(t)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Run(ctx); err == nil {
		t.Error("binding an address that is already taken must fail")
	}
}

// TestRunServesARealListener covers the path a recorder cannot: binding a
// socket, answering over TCP, and shutting down when the context ends.
func TestRunServesARealListener(t *testing.T) {
	st := newTestStore(t)
	m := NewMetrics("test", "none")

	// A real queued row: /healthz reads the database, not the gauge.
	if _, err := st.RegisterChat(context.Background(), 1, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if err := st.EnqueueTestNotification(context.Background(), 1, model.TaskEvent{
		Fingerprint: "fp-listener", TaskRepo: "sisyphus", TaskID: 1, State: "DONE", Owner: "grenka",
	}); err != nil {
		t.Fatalf("EnqueueTestNotification: %v", err)
	}

	addr := freeAddr(t)
	cfg := testConfig()
	cfg.HTTPListenAddr = addr

	srv := NewServer(Options{
		Config:   cfg,
		Store:    st,
		Metrics:  m,
		Poller:   stubPoller{model.PollerHealth{Running: true, IntervalSeconds: 60}},
		Telegram: stubTelegram{model.TelegramHealth{Mode: "polling", Running: true, LastUpdateID: 7}},
		Notifier: stubNotifier{running: true},
		Lease:    stubLease{holder: "test-host:1:bot.db"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not stop after cancellation")
		}
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + addr

	// Wait for the listener instead of sleeping a fixed amount.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(base + "/livez")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/livez", http.StatusOK},
		{"/readyz", http.StatusOK},
		{"/healthz", http.StatusOK},
		{"/metrics", http.StatusOK},
	} {
		resp, err := client.Get(base + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s = %d, want %d (%s)", tc.path, resp.StatusCode, tc.want, body)
		}
	}

	// The status document over a real socket, including the JSON field names
	// operators script against.
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	var h model.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decoding /healthz: %v", err)
	}
	if h.Status != "ok" || !h.Database.OK || !h.Telegram.Running || !h.Poller.Running {
		t.Errorf("health = %+v", h)
	}
	if h.Outbox.Pending != 1 {
		t.Errorf("outbox pending = %d, want the one queued row", h.Outbox.Pending)
	}
	if h.Extra["lease_holder"] != "test-host:1:bot.db" {
		t.Errorf("lease holder = %v", h.Extra["lease_holder"])
	}

	// Metrics are exposed with the altrdb_ prefix and no default collectors.
	mresp, err := client.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	mbody, _ := io.ReadAll(io.LimitReader(mresp.Body, 4<<20))
	mresp.Body.Close()
	if !strings.Contains(string(mbody), "altrdb_build_info") {
		t.Error("the exposition is missing altrdb_build_info")
	}
	// Serving /healthz refreshes the gauges, so the backlog shows up there too.
	if !strings.Contains(string(mbody), "altrdb_outbox_pending 1") {
		t.Errorf("altrdb_outbox_pending was not refreshed from the database:\n%s",
			filterMetricLines(string(mbody), "altrdb_outbox"))
	}
	if strings.Contains(string(mbody), "go_goroutines") {
		t.Error("the private registry leaked the default process collectors")
	}
}

// filterMetricLines keeps only the exposition lines matching a prefix, so a
// failure message stays readable.
func filterMetricLines(body, prefix string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// freeAddr reserves a loopback port and returns it, so the test does not hardcode
// a port that might already be taken.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}
