// Command altrdb-telegram-bot notifies Telegram chats about ALT Linux Girar
// build-task changes that match each chat's own subscription.
//
// It is a single self-hosted process: SQLite for state, HTTP polling against
// the ALT RDB API for ingestion, and a transactional outbox for delivery.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-telegram/bot"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/health"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/notifier"
	"altrdb-telegram-bot/internal/poller"
	"altrdb-telegram-bot/internal/rdb"
	"altrdb-telegram-bot/internal/store"
	"altrdb-telegram-bot/internal/telegram"
	"altrdb-telegram-bot/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "altrdb-telegram-bot: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		flagMigrate     = flag.Bool("migrate", false, "apply pending database migrations and exit")
		flagVersion     = flag.Bool("version", false, "print version and exit")
		flagCheckConfig = flag.Bool("check-config", false, "validate the environment and exit")
		flagListMigrate = flag.Bool("list-migrations", false, "list embedded migrations and exit")
		flagSetWebhook  = flag.Bool("set-webhook", false, "register TELEGRAM_WEBHOOK_URL with Telegram and exit")
		flagDelWebhook  = flag.Bool("delete-webhook", false, "remove any Telegram webhook and exit")
		flagDropPending = flag.Bool("drop-pending-updates", false, "with -set-webhook/-delete-webhook, discard queued updates")
		flagHealthCheck = flag.Bool("healthcheck", false, "probe HTTP_LISTEN_ADDR/healthz and exit 0 when healthy")
		flagPollOnce    = flag.Bool("poll-once", false, "run one RDB ingestion cycle against the database and exit (no Telegram needed)")
	)
	flag.Parse()

	if *flagVersion {
		fmt.Printf("altrdb-telegram-bot %s (commit %s)\n", config.Version, orDash(config.Commit))
		return nil
	}
	if *flagListMigrate {
		all, err := migrations.All()
		if err != nil {
			return err
		}
		for _, m := range all {
			fmt.Printf("%4d  %s\n", m.Version, m.Name)
		}
		return nil
	}

	// -healthcheck is resolved before the rest of the configuration so a
	// container probe does not need the bot token to be present.
	if *flagHealthCheck {
		return healthCheck()
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	if *flagCheckConfig {
		if err := cfg.ValidateServing(); err != nil {
			return err
		}
		fmt.Printf("configuration ok: %s\n", cfg)
		return nil
	}

	st, err := store.Open(context.Background(), cfg.DatabasePath, store.Options{
		Migrate: true,
		Logger:  log,
		Clock:   clock.New(),
	})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			log.Warn("closing the database failed", "error", cerr.Error())
		}
	}()

	if *flagMigrate {
		ver, err := st.SchemaVersionFromDB(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("database %s is at schema version %d\n", cfg.DatabasePath, ver)
		return nil
	}

	// One process per database. A second poller would fetch the same feed and
	// race the first one for the same outbox rows.
	holder := leaseHolder(cfg)
	if err := st.AcquireLease(context.Background(), holder, os.Getpid(), cfg.LeaseTTL); err != nil {
		if errors.Is(err, store.ErrLeaseHeld) {
			return fmt.Errorf("another instance is already using %s: %w\n"+
				"If you are sure nothing is running, wait for the lease to expire (%s) "+
				"or remove the app_lock row", cfg.DatabasePath, err, cfg.LeaseTTL)
		}
		return err
	}
	// Everything past this point talks to Telegram, so the token is required.
	// -migrate and -healthcheck returned before reaching it.
	if !*flagPollOnce {
		if err := cfg.ValidateServing(); err != nil {
			return err
		}
	}

	log.Info("database lease acquired", "holder", holder, "path", cfg.DatabasePath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Release the lease on any clean exit. Without this a one-shot run (-migrate,
	// -poll-once) or a fast restart would leave the next process waiting for the
	// full TTL. A crash still leaves the lease behind, which is the point.
	defer func() {
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelRelease()
		if err := st.ReleaseLease(releaseCtx, holder); err != nil {
			log.Warn("releasing the database lease failed", "error", err.Error())
		}
	}()

	metrics := health.NewMetrics(cfg.Version, cfg.Commit)
	feed := newRDBClient(cfg, log)

	p := poller.New(feed, st, cfg, log, clock.New(), metrics)

	// -poll-once is the operator's smoke test: it proves the database, the
	// network path and the live RDB schema all agree, without a bot token and
	// without sending anything to anybody.
	if *flagPollOnce {
		return pollOnce(ctx, st, p, log)
	}

	tg, err := telegram.New(telegram.Options{
		Config:    cfg,
		Store:     st,
		Clock:     clock.New(),
		Logger:    log,
		Metrics:   metrics,
		Refresher: p,
		Tasks:     feed,
		Workers:   4,
	})
	if err != nil {
		return err
	}

	ntf := notifier.New(st, tg.API(), clock.New(), log, metrics, notifier.Options{
		Interval:    cfg.NotifyInterval,
		MaxAttempts: cfg.NotifyMaxTries,
		Spacing:     cfg.NotifySendSpacing,
	})
	tg.SetNotifier(ntf)

	// Webhook administration happens before the loop starts, because both
	// -set-webhook and -delete-webhook are one-shot operations.
	if *flagSetWebhook || *flagDelWebhook {
		return webhookAdmin(ctx, tg.API(), cfg, *flagSetWebhook, *flagDropPending)
	}

	h := health.NewServer(health.Options{
		Config:   cfg,
		Store:    st,
		Metrics:  metrics,
		Logger:   log,
		Poller:   p,
		Telegram: tg,
		Notifier: ntf,
		Lease:    leaseAdapter{st},
	})
	if cfg.TelegramMode == config.ModeWebhook {
		h.Mount(cfg.WebhookPath(), tg.WebhookHandler())
	}

	// In polling mode a webhook left over from a previous deployment makes
	// getUpdates fail with 409, so it is removed explicitly.
	if cfg.TelegramMode == config.ModePolling {
		if ok, err := tg.API().DeleteWebhook(ctx, &bot.DeleteWebhookParams{}); err != nil {
			log.Warn("clearing a stale webhook failed; polling may return 409 until it is removed",
				"error", err.Error())
		} else if !ok {
			log.Warn("Telegram refused to clear the webhook")
		}
	}

	r := newRunner(ctx, log)
	r.add("lease", func(ctx context.Context) error {
		st.KeepLease(ctx, holder, cfg.LeaseHeartbeat, func(err error) {
			log.Error("database lease lost, shutting down", "error", err.Error())
			r.fail(fmt.Errorf("lost the database lease: %w", err))
		})
		return nil
	})
	r.add("health", func(ctx context.Context) error { return h.Run(ctx) })
	r.add("poller", p.Run)
	r.add("notifier", ntf.Run)
	r.add("telegram", tg.Run)

	log.Info("starting",
		"version", cfg.Version, "commit", orDash(cfg.Commit),
		"mode", string(cfg.TelegramMode), "listen", cfg.HTTPListenAddr,
		"poll_interval", cfg.PollInterval.String(), "tasks_limit", cfg.RDBTasksLimit,
		"database", cfg.DatabasePath)

	err = r.wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// newRDBClient builds the HTTP client for the ALT RDB API with explicit
// connect, TLS, response-header and overall timeouts.
func newRDBClient(cfg *config.Config, log *slog.Logger) *rdb.Client {
	return rdb.NewClient(rdb.Options{
		BaseURL:               cfg.RDBBaseURL,
		UserAgent:             cfg.RDBUserAgent,
		Timeout:               cfg.HTTPTimeout,
		ConnectTimeout:        cfg.HTTPConnectTimeout,
		TLSHandshakeTimeout:   cfg.HTTPTLSTimeout,
		ResponseHeaderTimeout: cfg.HTTPTimeout,
		MaxRetries:            rdb.DefaultMaxRetries,
		MinRequestSpacing:     rdb.DefaultMinRequestSpacing,
		Logger:                log,
		Clock:                 clock.New(),
	})
}

// pollOnce runs a single ingestion cycle and prints what it saw.
func pollOnce(ctx context.Context, st *store.Store, p *poller.Poller, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := p.RefreshBranchesNow(ctx); err != nil {
		log.Warn("the branch catalogue could not be refreshed", "error", err.Error())
	}
	branches, err := st.BranchNames(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("branches from RDB: %s\n", strings.Join(branches, ", "))

	if err := p.Cycle(ctx); err != nil {
		log.Warn("the poll cycle reported a problem", "error", err.Error())
	}

	health := p.PollerHealth(ctx)
	if len(health.Branches) == 0 {
		fmt.Println("no branch is followed yet, so nothing was polled.")
		fmt.Println("Start the bot and tick a branch with /branches, then run this again.")
		return nil
	}
	snapshots, err := st.CountSnapshots(ctx)
	if err != nil {
		return err
	}
	outbox, err := st.OutboxCounts(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("poll interval: %ds, tasks_limit: %d, tracked snapshots: %d\n",
		health.IntervalSeconds, health.TasksLimit, snapshots)
	fmt.Printf("outbox: %d pending, %d sending, %d dead\n",
		outbox.Pending, outbox.Sending, outbox.Dead)
	for _, b := range health.Branches {
		last := "never"
		if b.LastSuccessAt != nil {
			last = model.FormatTime(*b.LastSuccessAt)
		}
		oldest := "n/a"
		if b.OldestTaskChanged != nil {
			oldest = model.FormatTime(*b.OldestTaskChanged)
		}
		fmt.Printf("  %-10s tasks=%-4d baselined=%-5t last_success=%s oldest_change=%s saturated=%t failures=%d\n",
			b.Branch, b.TasksSeen, b.Baselined, last, oldest, b.WindowSaturated, b.ConsecutiveFailures)
		if b.LastError != "" {
			fmt.Printf("               last error: %s\n", b.LastError)
		}
	}
	if health.WindowSaturated {
		fmt.Println("warning: at least one branch filled its window; raise RDB_TASKS_LIMIT")
	}
	return nil
}

// healthCheck asks the running instance whether it is ready. It is what the
// container HEALTHCHECK runs, so the image needs no shell and no extra tooling.
func healthCheck() error {
	addr := os.Getenv("HTTP_LISTEN_ADDR")
	if addr == "" {
		addr = config.DefaultHTTPListenAddr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + probeAddr(addr) + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Printf("%s\n", strings.TrimSpace(string(body)))
	return nil
}

// probeAddr turns a listen address into something a client can dial. A server
// binds to 0.0.0.0 or :: to accept on every interface, but a probe inside the
// same network namespace should ask for loopback explicitly rather than rely on
// the kernel's interpretation of the wildcard.
func probeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// webhookAdmin performs the one-shot setWebhook/deleteWebhook calls.
func webhookAdmin(ctx context.Context, api *bot.Bot, cfg *config.Config, set, dropPending bool) error {
	adminCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if set {
		ok, err := api.SetWebhook(adminCtx, &bot.SetWebhookParams{
			URL:                cfg.TelegramWebhookURL,
			SecretToken:        cfg.TelegramWebhookSecret,
			DropPendingUpdates: dropPending,
			AllowedUpdates:     []string{"message", "callback_query", "my_chat_member"},
			MaxConnections:     20,
		})
		if err != nil {
			return fmt.Errorf("setWebhook: %w", err)
		}
		if !ok {
			return errors.New("setWebhook was refused by Telegram")
		}
		fmt.Printf("webhook set to %s (path %s)\n", cfg.TelegramWebhookURL, cfg.WebhookPath())
		fmt.Println("Remember to terminate TLS at your reverse proxy and forward to HTTP_LISTEN_ADDR.")
		return nil
	}

	ok, err := api.DeleteWebhook(adminCtx, &bot.DeleteWebhookParams{DropPendingUpdates: dropPending})
	if err != nil {
		return fmt.Errorf("deleteWebhook: %w", err)
	}
	if !ok {
		return errors.New("deleteWebhook was refused by Telegram")
	}
	fmt.Println("webhook removed; long polling will receive updates again")
	return nil
}

// leaseAdapter exposes the store's lease to the health server.
type leaseAdapter struct{ st *store.Store }

func (l leaseAdapter) LeaseInfo(ctx context.Context) (string, time.Time, error) {
	return l.st.LeaseHolder(ctx)
}

// leaseHolder builds a stable-enough identity for the single-instance lock.
func leaseHolder(cfg *config.Config) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	base := filepath.Base(cfg.DatabasePath)
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), base)
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactAttrs(),
	}
	var handler slog.Handler
	if strings.ToLower(cfg.LogFormat) == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler).With("app", "altrdb-telegram-bot")
}

// secretKeys are the attribute names that must never reach a log line. The
// token and the webhook secret are the two credentials this program holds, and
// a stray %+v of a config struct is exactly how they leak.
var secretKeys = map[string]struct{}{
	"token": {}, "bot_token": {}, "telegram_bot_token": {},
	"secret": {}, "webhook_secret": {}, "telegram_webhook_secret": {},
	"password": {}, "authorization": {},
}

// redactAttrs builds the slog attribute replacer that blanks secret keys.
func redactAttrs() func(groups []string, a slog.Attr) slog.Attr {
	return func(_ []string, a slog.Attr) slog.Attr {
		if _, isSecret := secretKeys[strings.ToLower(a.Key)]; isSecret {
			a.Value = slog.StringValue("[redacted]")
		}
		return a
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------------
// tiny supervised runner
// ---------------------------------------------------------------------------

// runner starts a set of long-lived goroutines and cancels everything as soon
// as one of them fails, so a broken component cannot leave a half-alive
// process behind.
type runner struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    *slog.Logger

	wg   sync.WaitGroup
	once sync.Once
	err  error
}

func newRunner(ctx context.Context, log *slog.Logger) *runner {
	c, cancel := context.WithCancel(ctx)
	return &runner{ctx: c, cancel: cancel, log: log}
}

// fail records the first fatal error and cancels the group.
func (r *runner) fail(err error) {
	r.once.Do(func() {
		r.err = err
		r.cancel()
	})
}

func (r *runner) add(name string, fn func(ctx context.Context) error) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := fn(r.ctx)
		if err != nil && !errors.Is(err, context.Canceled) && r.ctx.Err() == nil {
			r.log.Error("component failed", "component", name, "error", err.Error())
			r.fail(fmt.Errorf("%s: %w", name, err))
			return
		}
		r.log.Info("component stopped", "component", name)
	}()
}

// wait blocks until every component has stopped.
func (r *runner) wait() error {
	r.wg.Wait()
	r.cancel()
	return r.err
}
