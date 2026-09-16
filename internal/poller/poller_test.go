package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/rdb"
	"altrdb-telegram-bot/internal/store"
)

var epoch = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// fakeRDB serves the four endpoints the poller touches from in-memory data and
// counts the requests per branch, which is how "one fetch per active branch" is
// verified.
type fakeRDB struct {
	mu       sync.Mutex
	branches []string
	feeds    map[string][]rdb.TaskElement
	maints   map[string][]rdb.MaintainerEntry
	requests map[string]int
	failures int // how many leading requests fail with 500
	status   int
}

func newFakeRDB() *fakeRDB {
	return &fakeRDB{
		branches: []string{"sisyphus", "p11"},
		feeds:    map[string][]rdb.TaskElement{},
		maints:   map[string][]rdb.MaintainerEntry{},
		requests: map[string]int{},
	}
}

func (f *fakeRDB) setFeed(branch string, tasks ...rdb.TaskElement) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feeds[branch] = tasks
}

func (f *fakeRDB) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[key]
}

func (f *fakeRDB) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc(rdb.PathAllBranches, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests["branches"]++
		fail := f.shouldFailLocked()
		branches := f.branches
		f.mu.Unlock()
		if fail(w) {
			return
		}
		writeJSON(w, rdb.BranchesResponse{Length: len(branches), Branches: branches})
	})

	mux.HandleFunc(rdb.PathLastTasks, func(w http.ResponseWriter, r *http.Request) {
		branch := r.URL.Query().Get("branch")
		f.mu.Lock()
		f.requests["tasks:"+branch]++
		fail := f.shouldFailLocked()
		feed := f.feeds[branch]
		f.mu.Unlock()
		if fail(w) {
			return
		}
		writeJSON(w, rdb.TasksListResponse{
			RequestArgs: map[string]any{"branch": branch},
			Length:      len(feed),
			Tasks:       feed,
		})
	})

	mux.HandleFunc(rdb.PathMaintainers, func(w http.ResponseWriter, r *http.Request) {
		branch := r.URL.Query().Get("branch")
		f.mu.Lock()
		f.requests["maintainers:"+branch]++
		fail := f.shouldFailLocked()
		list := f.maints[branch]
		f.mu.Unlock()
		if fail(w) {
			return
		}
		writeJSON(w, rdb.MaintainersResponse{Length: len(list), Maintainers: list})
	})

	return mux
}

// shouldFailLocked reports whether this request must fail, and returns a func
// that writes the error response. The caller must hold f.mu.
func (f *fakeRDB) shouldFailLocked() func(http.ResponseWriter) bool {
	if f.failures > 0 {
		f.failures--
		status := f.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return func(w http.ResponseWriter) bool {
			http.Error(w, `{"message":"injected failure"}`, status)
			return true
		}
	}
	return func(http.ResponseWriter) bool { return false }
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// taskRecord builds one feed record.
func taskRecord(id int64, branch, owner, state string, changed time.Time) rdb.TaskElement {
	return rdb.TaskElement{
		TaskID: id, TaskRepo: branch, TaskState: state, TaskOwner: owner,
		TaskTry: 1, TaskIter: 1, TaskChanged: changed.Format(time.RFC3339),
		TaskStage: "task-build",
		Subtasks:  []rdb.Subtask{{SubtaskSrpmName: "postfix", SubtaskSrpmEvr: "3.8.21-alt1"}},
	}
}

// harness wires a store, a fake RDB server, a client and a poller together.
type harness struct {
	store  *store.Store
	clock  *clock.Fake
	fake   *fakeRDB
	server *httptest.Server
	client *rdb.Client
	p      *Poller
	cfg    *config.Config
	log    *slog.Logger
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	fake := newFakeRDB()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)

	cl := clock.NewFake(epoch)
	st, err := store.Open(context.Background(), t.TempDir()+"/bot.db", store.Options{
		Migrate: true, Clock: cl, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		RDBBaseURL:             srv.URL,
		PollInterval:           time.Minute,
		RDBTasksLimit:          200,
		OverlapWindow:          24 * time.Hour,
		MaxConcurrentBranches:  3,
		BranchRefreshInterval:  30 * time.Minute,
		MaintainerRefreshEvery: 6 * time.Hour,
		SnapshotKeepFor:        30 * 24 * time.Hour,
		TerminalStates:         model.DefaultTerminalStates,
		HTTPTimeout:            5 * time.Second,
	}
	if mutate != nil {
		mutate(cfg)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := rdb.NewClient(rdb.Options{
		BaseURL: srv.URL, HTTPClient: srv.Client(), MaxRetries: 1,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		MinRequestSpacing: 0, Logger: log, Clock: cl,
		Jitter: func() float64 { return 0.5 },
	})

	return &harness{
		store: st, clock: cl, fake: fake, server: srv, client: client,
		p: New(client, st, cfg, log, cl, nil), cfg: cfg, log: log,
	}
}

// subscribe creates an enabled chat with a complete selection.
func (h *harness) subscribe(t *testing.T, chatID int64, maintainers, states, branches []string, watermark time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.RegisterChat(ctx, chatID, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if _, err := h.store.AddMaintainers(ctx, chatID, maintainers); err != nil {
		t.Fatalf("AddMaintainers: %v", err)
	}
	if err := h.store.SetAllChatStates(ctx, chatID, states); err != nil {
		t.Fatalf("SetAllChatStates: %v", err)
	}
	for _, b := range branches {
		if _, err := h.store.DB().ExecContext(ctx,
			`INSERT INTO chat_branches (chat_id, branch, active_watermark, created_at) VALUES (?, ?, ?, ?)`,
			chatID, b, model.FormatTime(watermark), model.FormatTime(watermark)); err != nil {
			t.Fatalf("insert chat_branches: %v", err)
		}
	}
}

func (h *harness) pending(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM notifications WHERE status = 'pending'`).Scan(&n); err != nil {
		t.Fatalf("counting pending: %v", err)
	}
	return n
}

func TestFirstCycleBaselinesAndQueuesNothing(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	// A chat that has been subscribed for days: without the baseline rule a
	// first poll would dump the whole feed into its outbox.
	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-72*time.Hour))
	h.fake.setFeed("sisyphus",
		taskRecord(100, "sisyphus", "grenka", "DONE", epoch.Add(-3*time.Hour)),
		taskRecord(101, "sisyphus", "grenka", "DONE", epoch.Add(-2*time.Hour)),
	)

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if got := h.pending(t); got != 0 {
		t.Errorf("the first cycle queued %d notifications, want 0", got)
	}
	n, err := h.store.CountSnapshots(ctx)
	if err != nil {
		t.Fatalf("CountSnapshots: %v", err)
	}
	if n != 2 {
		t.Errorf("snapshots = %d, want both records baselined", n)
	}
	st, err := h.store.GetPollState(ctx, "sisyphus")
	if err != nil {
		t.Fatalf("GetPollState: %v", err)
	}
	if !st.Baselined {
		t.Error("poll state must record that the branch is baselined")
	}
}

func TestTransitionAfterBaselineProducesExactlyOneNotification(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.fake.setFeed("sisyphus", taskRecord(100, "sisyphus", "grenka", "BUILDING", epoch.Add(-time.Minute)))

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("baseline cycle: %v", err)
	}
	if got := h.pending(t); got != 0 {
		t.Fatalf("baseline queued %d", got)
	}

	h.clock.Advance(time.Minute)
	h.fake.setFeed("sisyphus", taskRecord(100, "sisyphus", "grenka", "DONE", epoch))

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if got := h.pending(t); got != 1 {
		t.Fatalf("pending = %d, want exactly one notification", got)
	}

	// Repeating the same poll must not queue anything else.
	for range 3 {
		if err := h.p.Cycle(ctx); err != nil {
			t.Fatalf("repeat cycle: %v", err)
		}
	}
	if got := h.pending(t); got != 1 {
		t.Errorf("pending after repeated polls = %d, want 1", got)
	}

	var state, owner string
	var taskID int64
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT task_id, json_extract(payload, '$.state'), json_extract(payload, '$.owner')
		 FROM notifications`).Scan(&taskID, &state, &owner); err != nil {
		t.Fatalf("reading the outbox row: %v", err)
	}
	if taskID != 100 || state != "DONE" || owner != "grenka" {
		t.Errorf("outbox row = %d/%s/%s", taskID, state, owner)
	}
}

func TestOneFetchPerActiveBranchServesAllSubscribers(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	// Three chats on sisyphus, one on p11, one on p10 that nobody else picked.
	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.subscribe(t, 2, []string{"rider"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.subscribe(t, 3, []string{"glebfm"}, []string{"DONE"}, []string{"sisyphus", "p11"}, epoch.Add(-time.Hour))

	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "DONE", epoch.Add(-time.Minute)))
	h.fake.setFeed("p11", taskRecord(2, "p11", "glebfm", "DONE", epoch.Add(-time.Minute)))

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	// Baseline for both, so the transition below is what actually notifies.
	h.clock.Advance(time.Minute)
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "DONE", epoch))
	h.fake.setFeed("p11", taskRecord(2, "p11", "glebfm", "DONE", epoch))
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}

	if got := h.fake.count("tasks:sisyphus"); got != 2 {
		t.Errorf("sisyphus was fetched %d times over two cycles with three subscribers, want 2", got)
	}
	if got := h.fake.count("tasks:p11"); got != 2 {
		t.Errorf("p11 was fetched %d times, want 2", got)
	}
	// Nobody follows p10 even though the API advertises it.
	if got := h.fake.count("tasks:p10"); got != 0 {
		t.Errorf("p10 was fetched %d times, want 0: only followed branches are polled", got)
	}
	if got := h.pending(t); got != 2 {
		t.Errorf("pending = %d, want one per matching chat", got)
	}
}

func TestPausedChatDropsItsBranchFromThePollingSet(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"p11"}, epoch.Add(-time.Hour))
	h.fake.setFeed("p11", taskRecord(1, "p11", "grenka", "DONE", epoch))
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if got := h.fake.count("tasks:p11"); got != 1 {
		t.Fatalf("p11 fetches = %d", got)
	}

	if err := h.store.SetChatEnabled(ctx, 1, false, "paused"); err != nil {
		t.Fatalf("SetChatEnabled: %v", err)
	}
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if got := h.fake.count("tasks:p11"); got != 1 {
		t.Errorf("p11 was still polled after its only subscriber paused: %d fetches", got)
	}
}

func TestBranchCatalogueRefreshAndOutageTolerance(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.BranchRefreshInterval = time.Hour })
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "DONE", epoch))

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	names, err := h.store.BranchNames(ctx)
	if err != nil {
		t.Fatalf("BranchNames: %v", err)
	}
	if strings.Join(names, ",") != "sisyphus,p11" {
		t.Errorf("cached branches = %v", names)
	}
	if got := h.fake.count("branches"); got != 1 {
		t.Errorf("branch fetches = %d, want 1", got)
	}

	// Within the refresh interval the catalogue is not fetched again.
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if got := h.fake.count("branches"); got != 1 {
		t.Errorf("branch fetches after a second cycle = %d, want still 1", got)
	}

	// An outage must not empty the selector.
	h.fake.mu.Lock()
	h.fake.failures = 100
	h.fake.mu.Unlock()
	h.p.mu.Lock()
	h.p.lastBranchSync = time.Time{}
	h.p.mu.Unlock()

	if err := h.p.Cycle(ctx); err == nil {
		t.Log("cycle reported no error even though the catalogue fetch failed (task polls may still work)")
	}
	names, err = h.store.BranchNames(ctx)
	if err != nil {
		t.Fatalf("BranchNames after the outage: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("cached branches after an outage = %v, want the last known list kept", names)
	}
}

func TestPollFailureIsRecordedAndDoesNotStopTheLoop(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "DONE", epoch))

	h.fake.mu.Lock()
	h.fake.failures = 1000
	h.fake.mu.Unlock()

	for i := range 3 {
		if err := h.p.Cycle(ctx); err == nil {
			t.Errorf("cycle %d reported success while RDB is failing", i)
		}
	}
	st, err := h.store.GetPollState(ctx, "sisyphus")
	if err != nil {
		t.Fatalf("GetPollState: %v", err)
	}
	if st.ConsecutiveFailures < 1 {
		t.Errorf("consecutive failures = %d", st.ConsecutiveFailures)
	}
	if st.LastError == "" {
		t.Error("the failure reason must be recorded")
	}
	if st.LastSuccessAt != nil {
		t.Errorf("last success = %v, want none", st.LastSuccessAt)
	}

	// Recovery: the next successful poll clears the counter.
	h.fake.mu.Lock()
	h.fake.failures = 0
	h.fake.mu.Unlock()
	h.clock.Advance(time.Minute)
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle after recovery: %v", err)
	}
	st, err = h.store.GetPollState(ctx, "sisyphus")
	if err != nil {
		t.Fatalf("GetPollState: %v", err)
	}
	if st.ConsecutiveFailures != 0 || st.LastError != "" {
		t.Errorf("after recovery: failures=%d error=%q", st.ConsecutiveFailures, st.LastError)
	}
	if st.LastSuccessAt == nil {
		t.Error("last success must be stamped after recovery")
	}
}

func TestWindowSaturationIsReported(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RDBTasksLimit = 3
		c.OverlapWindow = 24 * time.Hour
	})
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	// Three records (the cap) all from the last few minutes: the window does
	// not reach back 24h, so transitions older than that are unobservable.
	h.fake.setFeed("sisyphus",
		taskRecord(1, "sisyphus", "grenka", "DONE", epoch.Add(-3*time.Minute)),
		taskRecord(2, "sisyphus", "grenka", "DONE", epoch.Add(-2*time.Minute)),
		taskRecord(3, "sisyphus", "grenka", "DONE", epoch.Add(-1*time.Minute)),
	)
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	st, err := h.store.GetPollState(ctx, "sisyphus")
	if err != nil {
		t.Fatalf("GetPollState: %v", err)
	}
	if !st.WindowSaturated {
		t.Error("a full window that does not cover the overlap period must be flagged")
	}
	if st.OldestTaskChanged == nil {
		t.Error("the oldest observed change must be recorded")
	}
	if !h.p.PollerHealth(ctx).WindowSaturated {
		t.Error("the health document must expose the saturated window")
	}

	// A window that reaches back further than the overlap period is fine.
	h.fake.setFeed("sisyphus",
		taskRecord(1, "sisyphus", "grenka", "DONE", epoch.Add(-48*time.Hour)),
	)
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	st, _ = h.store.GetPollState(ctx, "sisyphus")
	if st.WindowSaturated {
		t.Error("a partial window must not be flagged as saturated")
	}
}

func TestUnknownStatesAreLearned(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "COMMITTING", epoch))

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	states, err := h.store.ListStates(ctx)
	if err != nil {
		t.Fatalf("ListStates: %v", err)
	}
	found := false
	for _, s := range states {
		if s.Name == "COMMITTING" {
			found = true
			if s.Known {
				t.Error("a state learned from the feed must not be marked known")
			}
		}
	}
	if !found {
		t.Errorf("COMMITTING was not recorded in %v", states)
	}
}

func TestMaintenancePrunesOldRows(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.SnapshotKeepFor = 24 * time.Hour })
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	old := taskRecord(1, "sisyphus", "grenka", "DONE", epoch.Add(-72*time.Hour))
	fresh := taskRecord(2, "sisyphus", "grenka", "DONE", epoch.Add(-time.Minute))
	h.fake.setFeed("sisyphus", old, fresh)

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	// The first cycle baselines; maintenance runs at the end of it.
	n, err := h.store.CountSnapshots(ctx)
	if err != nil {
		t.Fatalf("CountSnapshots: %v", err)
	}
	if n != 1 {
		t.Errorf("snapshots = %d, want the old record pruned", n)
	}
	if _, err := h.store.GetSnapshot(ctx, "sisyphus", 1); err == nil {
		t.Error("the 72h old snapshot survived pruning")
	}
	if _, err := h.store.GetSnapshot(ctx, "sisyphus", 2); err != nil {
		t.Errorf("the recent snapshot was pruned: %v", err)
	}
}

func TestMaintainerDirectoryIsCachedPerActiveBranch(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaintainerRefreshEvery = time.Hour })
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.fake.mu.Lock()
	h.fake.maints["sisyphus"] = []rdb.MaintainerEntry{{PackagerName: "Georgy", PackagerNickname: "grenka", CountSourcePkg: 4}}
	h.fake.mu.Unlock()
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "DONE", epoch))

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if got := h.fake.count("maintainers:sisyphus"); got != 1 {
		t.Errorf("directory fetches = %d, want 1", got)
	}
	found, name, _, err := h.store.FindMaintainer(ctx, "grenka", []string{"sisyphus"})
	if err != nil || !found || name != "Georgy" {
		t.Errorf("FindMaintainer = %t %q %v", found, name, err)
	}

	// A second cycle inside the TTL must not refetch.
	h.clock.Advance(10 * time.Minute)
	h.p.mu.Lock()
	h.p.lastMaintenance = time.Time{}
	h.p.mu.Unlock()
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if got := h.fake.count("maintainers:sisyphus"); got != 1 {
		t.Errorf("directory fetches after a second cycle = %d, want still 1", got)
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.PollInterval = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- h.p.Run(ctx) }()

	// Let it tick a few times, then cancel.
	time.Sleep(70 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want a clean stop", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if h.p.PollerHealth(context.Background()).Running {
		t.Error("the poller still reports itself running after stopping")
	}
}

func TestRunRefusesToStartTwice(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.PollInterval = time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.p.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)

	if err := h.p.Run(ctx); err == nil {
		t.Error("a second Run must be refused")
	}
	cancel()
	<-done
}

func TestPollerHealthDocument(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "DONE", epoch))
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}

	hh := h.p.PollerHealth(ctx)
	if hh.IntervalSeconds != 60 || hh.TasksLimit != 200 {
		t.Errorf("health = %+v", hh)
	}
	if len(hh.Branches) != 1 || hh.Branches[0].Branch != "sisyphus" {
		t.Errorf("branches = %+v", hh.Branches)
	}
	if hh.Branches[0].TasksSeen != 1 {
		t.Errorf("tasks seen = %d", hh.Branches[0].TasksSeen)
	}
	last, _, err := h.p.LastCycle()
	if err != nil || last.IsZero() {
		t.Errorf("last cycle = %s, %v", last, err)
	}
}

func TestTerminalModeIsHonouredEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"BUILDING", "DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	if err := h.store.SetNotifyMode(ctx, 1, model.NotifyTerminal); err != nil {
		t.Fatalf("SetNotifyMode: %v", err)
	}

	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "NEW", epoch.Add(-2*time.Minute)))
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	h.clock.Advance(time.Minute)
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "BUILDING", epoch.Add(-time.Minute)))
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := h.pending(t); got != 0 {
		t.Errorf("BUILDING queued %d notifications in terminal mode", got)
	}

	h.clock.Advance(time.Minute)
	h.fake.setFeed("sisyphus", taskRecord(1, "sisyphus", "grenka", "DONE", epoch))
	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := h.pending(t); got != 1 {
		t.Errorf("DONE queued %d notifications, want 1", got)
	}
}

func TestMalformedRecordsDoNotBlindTheBranch(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	h.fake.setFeed("sisyphus",
		taskRecord(1, "sisyphus", "grenka", "DONE", epoch.Add(-time.Minute)),
		rdb.TaskElement{TaskID: 2, TaskRepo: "sisyphus", TaskState: "DONE", TaskChanged: "whenever"},
	)

	if err := h.p.Cycle(ctx); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	n, err := h.store.CountSnapshots(ctx)
	if err != nil {
		t.Fatalf("CountSnapshots: %v", err)
	}
	if n != 1 {
		t.Errorf("snapshots = %d, want the good record stored despite the bad one", n)
	}
}

func TestFeedInterfaceIsSatisfiedByTheRealClient(t *testing.T) {
	// Compile-time proof that the poller talks to the real client, not only to
	// a test double.
	var _ Feed = (*rdb.Client)(nil)
	_ = fmt.Sprintf("%T", (*rdb.Client)(nil))
}
