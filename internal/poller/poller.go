// Package poller ingests the ALT RDB task feed.
//
// There is no event API to subscribe to, so this is a polling loop with
// persistent change detection. The design points that matter:
//
//   - One fetch per active branch, not per user. The set of branches is the
//     distinct branches that at least one enabled chat follows, so adding
//     subscribers costs nothing until they pick a new branch.
//   - Every poll re-reads the newest N records (a sliding, overlapping window)
//     and compares each fingerprint against the stored snapshot. That is what
//     tolerates records arriving late or carrying equal timestamps.
//   - Snapshot writes and notification inserts happen in one transaction, so a
//     crash cannot record "seen" without queueing the event.
//   - The first successful poll of a branch that has no stored snapshots is a
//     baseline: it records snapshots and queues nothing.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/health"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/rdb"
	"altrdb-telegram-bot/internal/store"
)

// Feed is the slice of the RDB client the poller needs.
type Feed interface {
	Branches(ctx context.Context) ([]string, error)
	LastTasks(ctx context.Context, branch string, limit int) (*rdb.TasksListResponse, []model.Task, int, error)
	Maintainers(ctx context.Context, branch string) ([]model.Maintainer, error)
	Snapshot() rdb.Stats
}

// Poller polls the task feed and turns changes into outbox rows.
type Poller struct {
	feed    Feed
	store   *store.Store
	clock   clock.Clock
	log     *slog.Logger
	metrics *health.Metrics

	interval        time.Duration
	tasksLimit      int
	overlap         time.Duration
	maxConcurrent   int
	branchRefresh   time.Duration
	maintRefresh    time.Duration
	snapshotKeepFor time.Duration
	notifyKeepFor   time.Duration
	terminal        model.TerminalStates

	mu              sync.RWMutex
	running         bool
	started         bool
	lastCycleAt     time.Time
	lastCycleErr    error
	lastBranchSync  time.Time
	lastMaintenance time.Time
	branches        map[string]model.PollState
	lastStats       rdb.Stats

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// New builds a poller from the validated configuration.
func New(feed Feed, st *store.Store, cfg *config.Config, log *slog.Logger, cl clock.Clock, m *health.Metrics) *Poller {
	if log == nil {
		log = slog.Default()
	}
	if cl == nil {
		cl = clock.New()
	}
	keep := cfg.SnapshotKeepFor
	if keep <= 0 {
		keep = config.DefaultSnapshotKeepFor
	}
	notifyKeep := 14 * 24 * time.Hour
	if keep < notifyKeep {
		notifyKeep = keep
	}
	return &Poller{
		feed:            feed,
		store:           st,
		clock:           cl,
		log:             log.With("component", "poller"),
		metrics:         m,
		interval:        cfg.PollInterval,
		tasksLimit:      cfg.RDBTasksLimit,
		overlap:         cfg.OverlapWindow,
		maxConcurrent:   cfg.MaxConcurrentBranches,
		branchRefresh:   cfg.BranchRefreshInterval,
		maintRefresh:    cfg.MaintainerRefreshEvery,
		snapshotKeepFor: keep,
		notifyKeepFor:   notifyKeep,
		terminal:        model.NewTerminalStates(cfg.TerminalStates),
		branches:        map[string]model.PollState{},
		locks:           map[string]*sync.Mutex{},
	}
}

// Run polls until ctx is cancelled. The first cycle starts immediately so a
// fresh process baselines without waiting a full interval.
func (p *Poller) Run(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("poller: already started")
	}
	p.started = true
	p.running = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	p.log.Info("poller started",
		"interval", p.interval.String(), "tasks_limit", p.tasksLimit,
		"overlap_window", p.overlap.String(), "max_concurrent_branches", p.maxConcurrent)

	// An immediate first pass: baseline existing branches before anything else
	// can queue a notification.
	if err := p.Cycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
		p.log.Error("initial poll cycle failed", "error", err.Error())
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping", "reason", ctx.Err().Error())
			return nil
		case <-ticker.C:
			if err := p.Cycle(ctx); err != nil {
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return nil
				}
				p.log.Error("poll cycle failed", "error", err.Error())
			}
		}
	}
}

// Cycle performs one full pass: refresh the branch catalogue, poll every active
// branch, then run maintenance. It is exported so tests can drive it directly.
func (p *Poller) Cycle(ctx context.Context) error {
	start := p.clock.Now()

	if err := p.RefreshBranches(ctx); err != nil {
		// Not fatal: the cached list still drives the selector, and the
		// active-branch set comes from the database.
		p.log.Warn("branch catalogue refresh failed", "error", err.Error())
	}

	branches, err := p.store.ActiveBranches(ctx)
	if err != nil {
		p.mu.Lock()
		p.lastCycleAt = start
		p.lastCycleErr = err
		p.mu.Unlock()
		return fmt.Errorf("poller: active branches: %w", err)
	}

	var cycleErr error
	if len(branches) > 0 {
		sem := make(chan struct{}, max(1, p.maxConcurrent))
		var wg sync.WaitGroup
		var mu sync.Mutex

		for _, branch := range branches {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(branch string) {
				defer wg.Done()
				defer func() { <-sem }()

				if err := p.PollBranch(ctx, branch); err != nil {
					mu.Lock()
					if cycleErr == nil {
						cycleErr = err
					}
					mu.Unlock()
				}
			}(branch)
		}
		wg.Wait()
	}

	if err := p.Maintenance(ctx); err != nil {
		p.log.Warn("maintenance failed", "error", err.Error())
		if cycleErr == nil {
			cycleErr = err
		}
	}

	p.mu.Lock()
	prev := p.lastStats
	p.lastCycleAt = p.clock.Now()
	p.lastCycleErr = cycleErr
	p.lastStats = p.feed.Snapshot()
	st := p.lastStats
	p.mu.Unlock()

	if p.metrics != nil {
		p.metrics.RDBRequestsTotal.Add(float64(st.Requests - prev.Requests))
		p.metrics.RDBRetriesTotal.Add(float64(st.Retries - prev.Retries))
		p.metrics.RDBRateLimitTotal.Add(float64(st.RateLimited - prev.RateLimited))
	}

	p.log.Debug("poll cycle finished",
		"branches", len(branches), "duration", p.clock.Now().Sub(start).String())
	return cycleErr
}

// PollBranch fetches one branch and applies the result. It never overlaps with
// itself for the same branch because Cycle waits for every goroutine, and the
// per-branch lock below also protects direct callers such as /refresh tests.
func (p *Poller) PollBranch(ctx context.Context, branch string) error {
	unlock := p.lockBranch(branch)
	defer unlock()

	start := time.Now()
	now := p.clock.Now()

	resp, tasks, skipped, err := p.feed.LastTasks(ctx, branch, p.tasksLimit)
	if err != nil {
		p.recordFailure(ctx, branch, now, err)
		if p.metrics != nil {
			p.metrics.PollsTotal.WithLabelValues(branch, "error").Inc()
			p.metrics.PollDurationSeconds.WithLabelValues(branch).Observe(time.Since(start).Seconds())
		}
		return fmt.Errorf("poller: %s: %w", branch, err)
	}

	baseline, err := p.store.NeedsBaseline(ctx, branch)
	if err != nil {
		p.recordFailure(ctx, branch, now, err)
		return fmt.Errorf("poller: %s baseline probe: %w", branch, err)
	}

	result, err := p.store.ApplyTasks(ctx, tasks, baseline, p.terminal)
	if err != nil {
		p.recordFailure(ctx, branch, now, err)
		return fmt.Errorf("poller: %s apply: %w", branch, err)
	}

	st := model.PollState{
		Branch:          branch,
		LastAttemptAt:   &now,
		LastSuccessAt:   &now,
		TasksSeen:       len(tasks),
		WindowSaturated: p.windowSaturated(tasks, now),
		// After a successful pass the branch has snapshots, so it is
		// baselined from here on, whether this pass was the baseline or not.
		Baselined:           true,
		ConsecutiveFailures: 0,
	}
	if oldest, ok := rdb.OldestChanged(tasks); ok {
		st.OldestTaskChanged = &oldest
	}
	if err := p.store.SavePollState(ctx, st); err != nil {
		p.log.Error("saving poll state failed", "branch", branch, "error", err.Error())
	}
	p.setBranchState(branch, st)

	if p.metrics != nil {
		p.metrics.PollsTotal.WithLabelValues(branch, "ok").Inc()
		p.metrics.PollDurationSeconds.WithLabelValues(branch).Observe(time.Since(start).Seconds())
		p.metrics.TasksSeen.WithLabelValues(branch).Set(float64(len(tasks)))
		p.metrics.LastSuccessfulPoll.WithLabelValues(branch).Set(float64(now.Unix()))
		sat := 0.0
		if st.WindowSaturated {
			sat = 1
		}
		p.metrics.WindowSaturated.WithLabelValues(branch).Set(sat)
		p.metrics.EventsCreated.Add(float64(result.Changed))
		p.metrics.NotificationsQueued.Add(float64(result.Notifications))
		p.metrics.DuplicateEvents.Add(float64(result.Duplicates))
		p.metrics.RecordsSkipped.Add(float64(skipped))
	}

	switch {
	case baseline:
		p.log.Info("baseline recorded",
			"branch", branch, "tasks", len(tasks), "note", "no notifications queued for pre-existing tasks")
	case result.Notifications > 0:
		p.log.Info("task changes queued",
			"branch", branch, "tasks", len(tasks), "changed", result.Changed,
			"notifications", result.Notifications, "duplicates_suppressed", result.Duplicates)
	default:
		p.log.Debug("poll ok",
			"branch", branch, "tasks", len(tasks), "changed", result.Changed,
			"duration", time.Since(start).String())
	}

	if st.WindowSaturated {
		p.log.Warn("poll window is saturated: transitions may be missed",
			"branch", branch, "tasks_limit", p.tasksLimit,
			"oldest_task_changed", st.OldestTaskChanged,
			"hint", "raise RDB_TASKS_LIMIT")
	}
	_ = resp // request_args is diagnostic only; length is covered by len(tasks)
	return nil
}

// RefreshBranches reloads the branch catalogue from RDB when it is due. The
// cached list survives an outage, so the selector keeps working.
func (p *Poller) RefreshBranches(ctx context.Context) error {
	p.mu.RLock()
	due := p.lastBranchSync.IsZero() || p.clock.Now().Sub(p.lastBranchSync) >= p.branchRefresh
	p.mu.RUnlock()
	if !due {
		return nil
	}
	return p.RefreshBranchesNow(ctx)
}

// RefreshBranchesNow reloads the catalogue immediately, ignoring the refresh
// interval. The UI calls it when a user presses ⟳ Refresh.
func (p *Poller) RefreshBranchesNow(ctx context.Context) error {
	branches, err := p.feed.Branches(ctx)
	if err != nil {
		return fmt.Errorf("poller: refresh branches: %w", err)
	}
	if err := p.store.ReplaceBranches(ctx, branches); err != nil {
		return fmt.Errorf("poller: store branches: %w", err)
	}
	p.mu.Lock()
	p.lastBranchSync = p.clock.Now()
	p.mu.Unlock()
	p.log.Info("branch catalogue refreshed", "count", len(branches))
	return nil
}

// RefreshMaintainers reloads one branch's packager directory into the cache.
func (p *Poller) RefreshMaintainers(ctx context.Context, branch string) error {
	maintainers, err := p.feed.Maintainers(ctx, branch)
	if err != nil {
		return fmt.Errorf("poller: maintainers %s: %w", branch, err)
	}
	if err := p.store.ReplaceMaintainersCache(ctx, branch, maintainers); err != nil {
		return fmt.Errorf("poller: cache maintainers %s: %w", branch, err)
	}
	p.log.Debug("maintainer directory cached", "branch", branch, "count", len(maintainers))
	return nil
}

// Maintenance prunes old rows and refreshes stale caches. It runs at most once
// per cycle and is cheap enough to keep in the polling goroutine.
func (p *Poller) Maintenance(ctx context.Context) error {
	p.mu.RLock()
	due := p.lastMaintenance.IsZero() || p.clock.Now().Sub(p.lastMaintenance) >= time.Hour
	p.mu.RUnlock()
	if !due {
		return nil
	}
	p.mu.Lock()
	p.lastMaintenance = p.clock.Now()
	p.mu.Unlock()

	now := p.clock.Now()
	if n, err := p.store.PruneSnapshots(ctx, now.Add(-p.snapshotKeepFor)); err != nil {
		p.log.Warn("snapshot pruning failed", "error", err.Error())
	} else if n > 0 {
		p.log.Info("pruned old task snapshots", "count", n, "older_than", p.snapshotKeepFor.String())
	}
	if n, err := p.store.PruneSentNotifications(ctx, now.Add(-p.notifyKeepFor)); err != nil {
		p.log.Warn("outbox pruning failed", "error", err.Error())
	} else if n > 0 {
		p.log.Info("pruned delivered notifications", "count", n)
	}

	// Keep the maintainer directory fresh for the branches people follow.
	active, err := p.store.ActiveBranches(ctx)
	if err != nil {
		return fmt.Errorf("poller: maintenance active branches: %w", err)
	}
	for _, branch := range active {
		if ctx.Err() != nil {
			return nil
		}
		fetched, ok, err := p.store.MaintainerCacheAge(ctx, branch)
		if err != nil {
			p.log.Warn("maintainer cache probe failed", "branch", branch, "error", err.Error())
			continue
		}
		if ok && now.Sub(fetched) < p.maintRefresh {
			continue
		}
		if err := p.RefreshMaintainers(ctx, branch); err != nil {
			p.log.Warn("maintainer cache refresh failed", "branch", branch, "error", err.Error())
		}
	}
	return nil
}

// windowSaturated reports whether the poll filled its window without reaching
// back over the overlap period. When true, a task that changed more than
// tasks_limit records ago may have been skipped entirely: the feed is a capped
// current-state view, not a log.
func (p *Poller) windowSaturated(tasks []model.Task, now time.Time) bool {
	if len(tasks) < p.tasksLimit {
		return false
	}
	oldest, ok := rdb.OldestChanged(tasks)
	if !ok {
		return false
	}
	return now.Sub(oldest) < p.overlap
}

func (p *Poller) recordFailure(ctx context.Context, branch string, now time.Time, cause error) {
	// The database is authoritative: after a restart the in-memory copy is
	// empty, and losing the last successful poll time would make an outage
	// look like the branch was never polled at all.
	prev := p.branchState(branch)
	if stored, err := p.store.GetPollState(ctx, branch); err == nil {
		prev = *stored
	}
	st := model.PollState{
		Branch:              branch,
		LastAttemptAt:       &now,
		LastSuccessAt:       prev.LastSuccessAt,
		LastError:           cause.Error(),
		ConsecutiveFailures: prev.ConsecutiveFailures + 1,
		TasksSeen:           prev.TasksSeen,
		OldestTaskChanged:   prev.OldestTaskChanged,
		WindowSaturated:     prev.WindowSaturated,
		Baselined:           prev.Baselined,
	}
	if err := p.store.SavePollState(ctx, st); err != nil {
		p.log.Error("saving poll failure state failed", "branch", branch, "error", err.Error())
	}
	p.setBranchState(branch, st)

	p.log.Warn("branch poll failed",
		"branch", branch, "consecutive_failures", st.ConsecutiveFailures, "error", cause.Error())
}

// PollerHealth implements health.Poller.
func (p *Poller) PollerHealth(ctx context.Context) model.PollerHealth {
	p.mu.RLock()
	running := p.running
	p.mu.RUnlock()

	h := model.PollerHealth{
		IntervalSeconds: int(p.interval.Seconds()),
		TasksLimit:      p.tasksLimit,
		Running:         running,
	}
	states, err := p.store.ListPollStates(ctx)
	if err != nil {
		p.log.Warn("reading poll states failed", "error", err.Error())
		states = p.cachedBranchStates()
	}
	h.Branches = states
	for _, st := range states {
		if st.WindowSaturated {
			h.WindowSaturated = true
			break
		}
	}
	return h
}

// LastCycle reports when the last full cycle ran, the RDB client counters at
// that point, and whether the cycle errored.
func (p *Poller) LastCycle() (time.Time, rdb.Stats, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastCycleAt, p.lastStats, p.lastCycleErr
}

func (p *Poller) branchState(branch string) model.PollState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.branches[branch]
}

func (p *Poller) setBranchState(branch string, st model.PollState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.branches[branch] = st
}

func (p *Poller) cachedBranchStates() []model.PollState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]model.PollState, 0, len(p.branches))
	for _, st := range p.branches {
		out = append(out, st)
	}
	return out
}

// lockBranch serialises polls of the same branch even if Cycle is called
// concurrently, for example from a test or a manual refresh. Two overlapping
// polls of one branch would each see the other's writes as "no change" and
// could queue nothing at all.
func (p *Poller) lockBranch(branch string) func() {
	p.locksMu.Lock()
	l, ok := p.locks[branch]
	if !ok {
		l = &sync.Mutex{}
		p.locks[branch] = l
	}
	p.locksMu.Unlock()
	l.Lock()
	return l.Unlock
}
