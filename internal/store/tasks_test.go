package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/model"
)

var terminal = model.NewTerminalStates(model.DefaultTerminalStates)

// task builds a feed record.
func task(id int64, repo, owner, state string, changed time.Time) model.Task {
	return model.Task{
		TaskID:   id,
		TaskRepo: repo,
		State:    state,
		Owner:    owner,
		Try:      1,
		Iter:     1,
		Stage:    "task-build",
		Changed:  changed,
		Packages: []string{"postfix 3.8.21-alt1"},
	}
}

// subscribe creates an enabled chat with a complete selection and an explicit
// activation watermark for every branch.
func subscribe(t *testing.T, st *Store, chatID int64, maintainers, states, branches []string, watermark time.Time) {
	t.Helper()
	ctx := context.Background()

	if _, err := st.RegisterChat(ctx, chatID, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat %d: %v", chatID, err)
	}
	if _, err := st.AddMaintainers(ctx, chatID, maintainers); err != nil {
		t.Fatalf("AddMaintainers %d: %v", chatID, err)
	}
	if err := st.SetAllChatStates(ctx, chatID, states); err != nil {
		t.Fatalf("SetAllChatStates %d: %v", chatID, err)
	}
	for _, b := range branches {
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO chat_branches (chat_id, branch, active_watermark, created_at) VALUES (?, ?, ?, ?)`,
			chatID, b, model.FormatTime(watermark), model.FormatTime(watermark)); err != nil {
			t.Fatalf("insert chat_branches %d/%s: %v", chatID, b, err)
		}
	}
}

func pendingCount(t *testing.T, st *Store, chatID int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM notifications WHERE chat_id = ?`, chatID).Scan(&n); err != nil {
		t.Fatalf("counting notifications: %v", err)
	}
	return n
}

func TestNeedsBaseline(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	needed, err := st.NeedsBaseline(ctx, "sisyphus")
	if err != nil {
		t.Fatalf("NeedsBaseline: %v", err)
	}
	if !needed {
		t.Error("a branch with no snapshots must need a baseline")
	}

	if _, err := st.ApplyTasks(ctx, []model.Task{task(1, "sisyphus", "grenka", "DONE", epoch)}, true, terminal); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if needed, _ := st.NeedsBaseline(ctx, "sisyphus"); needed {
		t.Error("a branch that has snapshots must not need a baseline")
	}
	// Another branch is independent.
	if needed, _ := st.NeedsBaseline(ctx, "p11"); !needed {
		t.Error("p11 has no snapshots and must need a baseline")
	}
}

func TestApplyTasksBaselineQueuesNothing(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	// A fully subscribed chat whose watermark is long in the past: the only
	// thing standing between it and a history blast is the baseline pass.
	subscribe(t, st, 100, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-72*time.Hour))

	feed := []model.Task{
		task(1, "sisyphus", "grenka", "DONE", epoch.Add(-time.Hour)),
		task(2, "sisyphus", "grenka", "DONE", epoch.Add(-2*time.Hour)),
	}
	res, err := st.ApplyTasks(ctx, feed, true, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if !res.Baseline {
		t.Error("the result must report that this was a baseline pass")
	}
	if res.Tasks != 2 || res.Changed != 2 {
		t.Errorf("tasks = %d, changed = %d", res.Tasks, res.Changed)
	}
	if res.Notifications != 0 {
		t.Errorf("a baseline queued %d notifications, want 0", res.Notifications)
	}
	if got := pendingCount(t, st, 100); got != 0 {
		t.Errorf("outbox rows = %d, want 0", got)
	}
	if n, _ := st.CountSnapshots(ctx); n != 2 {
		t.Errorf("snapshots = %d, want both records stored", n)
	}
}

func TestApplyTasksQueuesNothingWhenNothingChanged(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 101, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	feed := []model.Task{task(1, "sisyphus", "grenka", "DONE", epoch)}
	if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	for i := range 5 {
		res, err := st.ApplyTasks(ctx, feed, false, terminal)
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		if res.Changed != 0 || res.Notifications != 0 {
			t.Errorf("poll %d changed=%d notifications=%d, want 0/0", i, res.Changed, res.Notifications)
		}
	}
	if got := pendingCount(t, st, 101); got != 0 {
		t.Errorf("outbox rows after five identical polls = %d, want 0", got)
	}
}

func TestApplyTasksDetectsATransition(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 102, []string{"grenka"}, []string{"DONE", "FAILED"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	first := []model.Task{task(7, "sisyphus", "grenka", "BUILDING", epoch)}
	if _, err := st.ApplyTasks(ctx, first, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	second := []model.Task{task(7, "sisyphus", "grenka", "DONE", epoch.Add(time.Minute))}
	res, err := st.ApplyTasks(ctx, second, false, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if res.Changed != 1 || res.Notifications != 1 {
		t.Fatalf("changed = %d, notifications = %d; want 1/1", res.Changed, res.Notifications)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events = %+v", res.Events)
	}
	ev := res.Events[0]
	if ev.State != "DONE" || ev.PreviousState != "BUILDING" || ev.TaskID != 7 {
		t.Errorf("event = %+v", ev)
	}
	if ev.FirstObserved {
		t.Error("a known task changing state is not a first observation")
	}
	if ev.Fingerprint == "" || ev.Fingerprint != model.Fingerprint(second[0]) {
		t.Errorf("fingerprint = %q", ev.Fingerprint)
	}

	snap, err := st.GetSnapshot(ctx, "sisyphus", 7)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.State != "DONE" || snap.Fingerprint != ev.Fingerprint {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.FirstSeenAt.IsZero() || snap.UpdatedAt.Before(snap.FirstSeenAt) {
		t.Errorf("timestamps = first %s, updated %s", snap.FirstSeenAt, snap.UpdatedAt)
	}
}

func TestFirstObservationAfterBaselineIsNotified(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 103, []string{"grenka"}, []string{"NEW"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	if _, err := st.ApplyTasks(ctx, []model.Task{task(1, "sisyphus", "grenka", "DONE", epoch)}, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// A brand new task appears in the feed.
	fresh := []model.Task{
		task(1, "sisyphus", "grenka", "DONE", epoch),
		task(2, "sisyphus", "grenka", "NEW", epoch.Add(30*time.Minute)),
	}
	res, err := st.ApplyTasks(ctx, fresh, false, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if res.Notifications != 1 {
		t.Fatalf("notifications = %d, want exactly the new task", res.Notifications)
	}
	if res.Events[0].TaskID != 2 || !res.Events[0].FirstObserved {
		t.Errorf("event = %+v", res.Events[0])
	}
	if res.Events[0].PreviousState != "" {
		t.Errorf("a first observation has no previous state, got %q", res.Events[0].PreviousState)
	}
}

func TestMatchingNeedsOwnerStateAndBranch(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name        string
		maintainers []string
		states      []string
		branches    []string
		want        int
	}{
		{"everything matches", []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, 1},
		{"owner differs", []string{"rider"}, []string{"DONE"}, []string{"sisyphus"}, 0},
		{"state not selected", []string{"grenka"}, []string{"FAILED"}, []string{"sisyphus"}, 0},
		{"branch not selected", []string{"grenka"}, []string{"DONE"}, []string{"p11"}, 0},
		{"no maintainers selected", nil, []string{"DONE"}, []string{"sisyphus"}, 0},
		{"no states selected", []string{"grenka"}, nil, []string{"sisyphus"}, 0},
		{"no branches selected", []string{"grenka"}, []string{"DONE"}, nil, 0},
		{"several maintainers, one matches", []string{"rider", "grenka", "glebfm"}, []string{"DONE"}, []string{"sisyphus"}, 1},
		{"several states, one matches", []string{"grenka"}, []string{"NEW", "DONE", "FAILED"}, []string{"sisyphus"}, 1},
		{"several branches, one matches", []string{"grenka"}, []string{"DONE"}, []string{"p10", "sisyphus", "p11"}, 1},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chatID := int64(200 + i)
			subscribe(t, st, chatID, tc.maintainers, tc.states, tc.branches, epoch.Add(-time.Hour))

			feed := []model.Task{task(500, "sisyphus", "grenka", "DONE", epoch)}
			if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			changed := task(500, "sisyphus", "grenka", "DONE", epoch.Add(time.Minute))
			res, err := st.ApplyTasks(ctx, []model.Task{changed}, false, terminal)
			if err != nil {
				t.Fatalf("ApplyTasks: %v", err)
			}
			if got := pendingCount(t, st, chatID); got != tc.want {
				t.Errorf("notifications = %d, want %d (events=%+v)", got, tc.want, res.Events)
			}
		})
	}
}

func TestDisabledChatReceivesNothing(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 300, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	if err := st.SetChatEnabled(ctx, 300, false, "paused by the user"); err != nil {
		t.Fatalf("SetChatEnabled: %v", err)
	}

	if _, err := st.ApplyTasks(ctx, []model.Task{task(9, "sisyphus", "grenka", "NEW", epoch)}, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	res, err := st.ApplyTasks(ctx, []model.Task{task(9, "sisyphus", "grenka", "DONE", epoch.Add(time.Minute))}, false, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if res.Notifications != 0 {
		t.Errorf("a paused chat was queued %d notifications", res.Notifications)
	}
	if got := pendingCount(t, st, 300); got != 0 {
		t.Errorf("outbox rows = %d, want 0", got)
	}
}

func TestWatermarkSuppressesPreExistingTasks(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()

	// The feed already contains an old task before the chat subscribes.
	old := task(11, "sisyphus", "grenka", "DONE", epoch.Add(-5*time.Hour))
	if _, err := st.ApplyTasks(ctx, []model.Task{old}, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// Now the chat ticks the branch: the watermark is "now".
	subscribe(t, st, 301, []string{"grenka"}, []string{"DONE"}, nil, time.Time{})
	wm, err := st.AddChatBranch(ctx, 301, "sisyphus")
	if err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}
	if !wm.Equal(epoch) {
		t.Errorf("watermark = %s, want %s", wm, epoch)
	}

	// The old task reappears with a new fingerprint (its stage moved) but its
	// task_changed is still older than the watermark: no notification.
	relived := old
	relived.Stage = "task-save-repo"
	relived.Changed = epoch.Add(-4 * time.Hour)
	res, err := st.ApplyTasks(ctx, []model.Task{relived}, false, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if res.Changed != 1 {
		t.Errorf("the snapshot did change, changed = %d", res.Changed)
	}
	if res.Notifications != 0 {
		t.Errorf("watermark ignored: %d notifications queued", res.Notifications)
	}

	// A change after the watermark is notified.
	fake.Advance(time.Minute)
	fresh := relived
	fresh.Changed = epoch.Add(time.Minute)
	fresh.Iter = 2
	res, err = st.ApplyTasks(ctx, []model.Task{fresh}, false, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if res.Notifications != 1 {
		t.Errorf("notifications = %d, want 1 for a post-watermark change", res.Notifications)
	}
}

func TestTerminalNotifyModeFiltersTransitions(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 400, []string{"grenka"}, []string{"BUILDING", "TESTED", "DONE", "FAILED"},
		[]string{"sisyphus"}, epoch.Add(-time.Hour))
	if err := st.SetNotifyMode(ctx, 400, model.NotifyTerminal); err != nil {
		t.Fatalf("SetNotifyMode: %v", err)
	}

	if _, err := st.ApplyTasks(ctx, []model.Task{task(21, "sisyphus", "grenka", "NEW", epoch)}, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	transitions := []struct {
		state string
		want  int
	}{
		{"BUILDING", 0},
		{"TESTED", 0},
		{"DONE", 1},
		{"FAILED", 1},
	}
	for i, tr := range transitions {
		changed := task(21, "sisyphus", "grenka", tr.state, epoch.Add(time.Duration(i+1)*time.Minute))
		res, err := st.ApplyTasks(ctx, []model.Task{changed}, false, terminal)
		if err != nil {
			t.Fatalf("ApplyTasks(%s): %v", tr.state, err)
		}
		if res.Notifications != tr.want {
			t.Errorf("state %s queued %d notifications, want %d", tr.state, res.Notifications, tr.want)
		}
	}
	if got := pendingCount(t, st, 400); got != 2 {
		t.Errorf("total outbox rows = %d, want 2 (DONE and FAILED)", got)
	}
}

func TestFanOutToOneNotificationPerMatchingChat(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	subscribe(t, st, 500, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	subscribe(t, st, 501, []string{"grenka", "rider"}, []string{"DONE", "FAILED"}, []string{"sisyphus", "p11"}, epoch.Add(-time.Hour))
	subscribe(t, st, 502, []string{"rider"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	if _, err := st.ApplyTasks(ctx, []model.Task{task(31, "sisyphus", "grenka", "NEW", epoch)}, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	res, err := st.ApplyTasks(ctx, []model.Task{task(31, "sisyphus", "grenka", "DONE", epoch.Add(time.Minute))}, false, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	if res.Notifications != 2 {
		t.Errorf("notifications = %d, want one per matching chat", res.Notifications)
	}
	if pendingCount(t, st, 500) != 1 || pendingCount(t, st, 501) != 1 {
		t.Error("each matching chat must get exactly one row")
	}
	if pendingCount(t, st, 502) != 0 {
		t.Error("a chat following a different maintainer must get nothing")
	}
}

func TestDatabaseUniquenessIsTheFinalDedupGuard(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 600, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	feed := []model.Task{task(41, "sisyphus", "grenka", "NEW", epoch)}
	if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	changed := task(41, "sisyphus", "grenka", "DONE", epoch.Add(time.Minute))

	res, err := st.ApplyTasks(ctx, []model.Task{changed}, false, terminal)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if res.Notifications != 1 || res.Duplicates != 0 {
		t.Errorf("first apply = %+v", res)
	}

	// Simulate a crash between the send and the snapshot write by rewinding
	// the snapshot to the previous fingerprint, then replaying the same poll.
	fp := model.Fingerprint(feed[0])
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE task_snapshots SET fingerprint = ? WHERE task_repo = 'sisyphus' AND task_id = 41`, fp); err != nil {
		t.Fatalf("rewinding the snapshot: %v", err)
	}

	replay, err := st.ApplyTasks(ctx, []model.Task{changed}, false, terminal)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Notifications != 0 {
		t.Errorf("the replay queued %d new notifications", replay.Notifications)
	}
	if replay.Duplicates != 1 {
		t.Errorf("duplicates suppressed = %d, want 1", replay.Duplicates)
	}
	if got := pendingCount(t, st, 600); got != 1 {
		t.Errorf("outbox rows = %d, want exactly 1", got)
	}

	// The same guarantee holds across a full restart.
	path := st.Path()
	_ = st.Close()
	reopened, err := Open(ctx, path, Options{Migrate: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	again, err := reopened.ApplyTasks(ctx, []model.Task{changed}, false, terminal)
	if err != nil {
		t.Fatalf("apply after restart: %v", err)
	}
	if again.Notifications != 0 {
		t.Errorf("a restart produced %d duplicate notifications", again.Notifications)
	}
	if got := pendingCount(t, reopened, 600); got != 1 {
		t.Errorf("outbox rows after restart = %d, want 1", got)
	}
	exists, err := reopened.NotificationExists(ctx, 600, model.Fingerprint(changed))
	if err != nil {
		t.Fatalf("NotificationExists: %v", err)
	}
	if !exists {
		t.Error("the original row must still be there")
	}
}

func TestApplyTasksRecordsObservedStates(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	feed := []model.Task{
		task(1, "sisyphus", "grenka", "COMMITTING", epoch),
		task(2, "sisyphus", "grenka", "DONE", epoch),
		task(3, "sisyphus", "grenka", "", epoch),
	}
	if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	names, err := st.StateNames(ctx)
	if err != nil {
		t.Fatalf("StateNames: %v", err)
	}
	if !strings.Contains(strings.Join(names, ","), "COMMITTING") {
		t.Errorf("a state seen in the feed was not recorded: %v", names)
	}
	for _, n := range names {
		if n == "" {
			t.Error("blank states must not be persisted")
		}
	}
}

func TestApplyTasksEmptyInput(t *testing.T) {
	st, _ := newTestStore(t)
	res, err := st.ApplyTasks(context.Background(), nil, false, terminal)
	if err != nil {
		t.Fatalf("ApplyTasks with no records: %v", err)
	}
	if res.Tasks != 0 || res.Changed != 0 || res.Notifications != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestPruneSnapshots(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	feed := []model.Task{
		task(1, "sisyphus", "grenka", "DONE", epoch.Add(-40*24*time.Hour)),
		task(2, "sisyphus", "grenka", "DONE", epoch.Add(-time.Hour)),
	}
	if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	n, err := st.PruneSnapshots(ctx, epoch.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneSnapshots: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}
	if _, err := st.GetSnapshot(ctx, "sisyphus", 2); err != nil {
		t.Errorf("the recent snapshot was pruned: %v", err)
	}
	if _, err := st.GetSnapshot(ctx, "sisyphus", 1); err != ErrNotFound {
		t.Errorf("the old snapshot = %v, want ErrNotFound", err)
	}
}

func TestRecentMatching(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 700, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-72*time.Hour))

	feed := []model.Task{
		task(1, "sisyphus", "grenka", "DONE", epoch.Add(-3*time.Hour)),
		task(2, "sisyphus", "grenka", "DONE", epoch.Add(-1*time.Hour)),
		task(3, "sisyphus", "grenka", "FAILED", epoch.Add(-2*time.Hour)),
		task(4, "sisyphus", "rider", "DONE", epoch.Add(-2*time.Hour)),
		task(5, "p11", "grenka", "DONE", epoch.Add(-2*time.Hour)),
	}
	if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}

	got, err := st.RecentMatching(ctx, 700, 10)
	if err != nil {
		t.Fatalf("RecentMatching: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recent = %+v, want the two matching tasks", got)
	}
	// Newest first.
	if got[0].TaskID != 2 || got[1].TaskID != 1 {
		t.Errorf("order = %d, %d; want newest first", got[0].TaskID, got[1].TaskID)
	}
	if len(got[0].Packages) != 1 || got[0].Packages[0] != "postfix 3.8.21-alt1" {
		t.Errorf("packages = %v", got[0].Packages)
	}

	limited, err := st.RecentMatching(ctx, 700, 1)
	if err != nil {
		t.Fatalf("RecentMatching with a limit: %v", err)
	}
	if len(limited) != 1 || limited[0].TaskID != 2 {
		t.Errorf("limited = %+v", limited)
	}

	// An incomplete subscription returns nothing rather than everything.
	subscribe(t, st, 701, []string{"grenka"}, nil, nil, epoch)
	empty, err := st.RecentMatching(ctx, 701, 10)
	if err != nil {
		t.Fatalf("RecentMatching: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an unconfigured chat saw %d tasks", len(empty))
	}
}

func TestApplyTasksUnderConcurrency(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 800, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	if _, err := st.ApplyTasks(ctx, []model.Task{task(900, "sisyphus", "grenka", "NEW", epoch)}, true, terminal); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// Ten goroutines all replaying the same transition: the unique constraint
	// must let exactly one row through.
	changed := task(900, "sisyphus", "grenka", "DONE", epoch.Add(time.Minute))
	var wg sync.WaitGroup
	var mu sync.Mutex
	totalInserted := 0
	errs := make([]error, 0, 10)

	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := st.ApplyTasks(ctx, []model.Task{changed}, false, terminal)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			totalInserted += res.Notifications
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent apply errors: %v", errs)
	}
	if totalInserted != 1 {
		t.Errorf("%d notifications were inserted for one event, want exactly 1", totalInserted)
	}
	if got := pendingCount(t, st, 800); got != 1 {
		t.Errorf("outbox rows = %d, want 1", got)
	}
}

func TestSnapshotsAreStoredPerRepoAndTask(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	feed := []model.Task{
		task(1, "sisyphus", "grenka", "DONE", epoch),
		task(1, "p11", "grenka", "FAILED", epoch), // same id, different repo
	}
	if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	n, err := st.CountSnapshots(ctx)
	if err != nil {
		t.Fatalf("CountSnapshots: %v", err)
	}
	if n != 2 {
		t.Errorf("snapshots = %d, want one per (repo, id)", n)
	}
	s1, err := st.GetSnapshot(ctx, "sisyphus", 1)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	s2, err := st.GetSnapshot(ctx, "p11", 1)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if s1.State != "DONE" || s2.State != "FAILED" {
		t.Errorf("states = %q / %q", s1.State, s2.State)
	}
	if s1.Fingerprint == s2.Fingerprint {
		t.Error("the same task id in two branches must have distinct fingerprints")
	}
}

func TestApplyTasksStoresPackagesAsJSON(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	feed := []model.Task{task(1, "sisyphus", "grenka", "DONE", epoch)}
	feed[0].Packages = []string{`weird "<name>" & co 1.0-alt1`, "second 2.0"}
	if _, err := st.ApplyTasks(ctx, feed, true, terminal); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}
	snap, err := st.GetSnapshot(ctx, "sisyphus", 1)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if len(snap.Packages) != 2 || snap.Packages[0] != `weird "<name>" & co 1.0-alt1` {
		t.Errorf("packages round trip = %q", snap.Packages)
	}
}

func TestPollStateRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := st.GetPollState(ctx, "sisyphus"); err != ErrNotFound {
		t.Errorf("an unpolled branch = %v, want ErrNotFound", err)
	}

	attempt := epoch
	success := epoch.Add(-time.Minute)
	oldest := epoch.Add(-6 * time.Hour)
	in := model.PollState{
		Branch: "sisyphus", LastAttemptAt: &attempt, LastSuccessAt: &success,
		LastError: "boom", ConsecutiveFailures: 3, TasksSeen: 200,
		OldestTaskChanged: &oldest, WindowSaturated: true, Baselined: true,
	}
	if err := st.SavePollState(ctx, in); err != nil {
		t.Fatalf("SavePollState: %v", err)
	}
	out, err := st.GetPollState(ctx, "sisyphus")
	if err != nil {
		t.Fatalf("GetPollState: %v", err)
	}
	if out.ConsecutiveFailures != 3 || out.TasksSeen != 200 || !out.WindowSaturated || !out.Baselined {
		t.Errorf("poll state = %+v", out)
	}
	if out.LastAttemptAt == nil || !out.LastAttemptAt.Equal(attempt) {
		t.Errorf("last attempt = %v", out.LastAttemptAt)
	}
	if out.OldestTaskChanged == nil || !out.OldestTaskChanged.Equal(oldest) {
		t.Errorf("oldest = %v", out.OldestTaskChanged)
	}
	if out.LastError != "boom" {
		t.Errorf("last error = %q", out.LastError)
	}

	// Upsert, not append.
	in.ConsecutiveFailures = 0
	in.LastError = ""
	if err := st.SavePollState(ctx, in); err != nil {
		t.Fatalf("SavePollState update: %v", err)
	}
	all, err := st.ListPollStates(ctx)
	if err != nil {
		t.Fatalf("ListPollStates: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("poll states = %d rows, want 1", len(all))
	}
	if all[0].ConsecutiveFailures != 0 || all[0].LastError != "" {
		t.Errorf("poll state after update = %+v", all[0])
	}

	if err := st.SavePollState(ctx, model.PollState{}); err == nil {
		t.Error("a poll state without a branch must be rejected")
	}
}

func TestPollStateTruncatesLongErrors(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	now := epoch
	long := strings.Repeat("x", 5000)
	if err := st.SavePollState(ctx, model.PollState{
		Branch: "p11", LastAttemptAt: &now, LastError: long,
	}); err != nil {
		t.Fatalf("SavePollState: %v", err)
	}
	out, err := st.GetPollState(ctx, "p11")
	if err != nil {
		t.Fatalf("GetPollState: %v", err)
	}
	if len(out.LastError) > 512 {
		t.Errorf("last error kept %d bytes, want at most 512", len(out.LastError))
	}
}

func TestDatabaseFileIsCreatedWhereAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "bot.db")
	st, _ := newTestStoreAt(t, path)
	if st.Path() != path {
		t.Errorf("Path() = %q, want %q", st.Path(), path)
	}
	if err := st.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestFingerprintsDifferForEveryMeaningfulField(t *testing.T) {
	// The database-level guard depends on the fingerprint being exactly as
	// sensitive as the product definition of "an event".
	base := task(1, "sisyphus", "grenka", "DONE", epoch)
	for _, mutate := range []func(*model.Task){
		func(x *model.Task) { x.TaskID = 2 },
		func(x *model.Task) { x.TaskRepo = "p11" },
		func(x *model.Task) { x.Owner = "rider" },
		func(x *model.Task) { x.Try = 2 },
		func(x *model.Task) { x.Iter = 2 },
		func(x *model.Task) { x.State = "FAILED" },
		func(x *model.Task) { x.Stage = "task-check-girar" },
		func(x *model.Task) { x.Changed = epoch.Add(time.Second) },
	} {
		modified := base
		mutate(&modified)
		if model.Fingerprint(modified) == model.Fingerprint(base) {
			t.Errorf("mutating %+v did not change the fingerprint", modified)
		}
	}
}
