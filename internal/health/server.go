package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/store"
)

// Poller is the ingestion component as the health server sees it.
type Poller interface {
	PollerHealth(ctx context.Context) model.PollerHealth
}

// Telegram is the update-loop component as the health server sees it.
type Telegram interface {
	TelegramHealth(ctx context.Context) model.TelegramHealth
}

// Notifier is the delivery component as the health server sees it.
type Notifier interface {
	NotifierHealth(ctx context.Context) (running bool, lastDelivery time.Time, err error)
}

// Lease reports who owns the database.
type Lease interface {
	LeaseInfo(ctx context.Context) (holder string, heartbeat time.Time, err error)
}

// Server is the operator HTTP endpoint. It binds to loopback by default.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	metrics *Metrics
	log     *slog.Logger
	started time.Time
	clock   func() time.Time

	mu       sync.RWMutex
	poller   Poller
	telegram Telegram
	notifier Notifier
	lease    Lease
	extra    map[string]http.Handler
}

// Options wires the server to the components it reports on. Any of them may be
// nil, in which case the corresponding part of /healthz says "not running".
type Options struct {
	Config   *config.Config
	Store    *store.Store
	Metrics  *Metrics
	Logger   *slog.Logger
	Poller   Poller
	Telegram Telegram
	Notifier Notifier
	Lease    Lease
}

// NewServer builds the health server.
func NewServer(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{
		cfg:      opts.Config,
		store:    opts.Store,
		metrics:  opts.Metrics,
		log:      opts.Logger.With("component", "health"),
		started:  time.Now(),
		clock:    func() time.Time { return time.Now().UTC() },
		poller:   opts.Poller,
		telegram: opts.Telegram,
		notifier: opts.Notifier,
		lease:    opts.Lease,
		extra:    map[string]http.Handler{},
	}
}

// Mount adds a handler at path. Webhook mode uses it to serve Telegram updates
// from the same listener instead of opening a second port.
func (s *Server) Mount(path string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extra[path] = h
}

// Handler builds the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// "GET /{$}" matches only the bare root. A plain "GET /" would also match
	// every sub-path, and Go's ServeMux refuses to register that alongside a
	// method-agnostic mount such as the webhook handler.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/healthz", http.StatusSeeOther)
	})

	if s.metrics != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.Registry(), promhttp.HandlerOpts{
			ErrorLog:      slog.NewLogLogger(s.log.Handler(), slog.LevelError),
			ErrorHandling: promhttp.ContinueOnError,
		}))
	} else {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "metrics are not enabled in this build", http.StatusNotFound)
		})
	}

	s.mu.RLock()
	for path, h := range s.extra {
		mux.Handle(path, h)
	}
	s.mu.RUnlock()

	return s.withLogging(mux)
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	addr := s.cfg.HTTPListenAddr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("health: listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelError),
	}
	s.log.Info("health server listening", "addr", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("health: shutdown: %w", err)
		}
		return <-errCh
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("health: serve: %w", err)
		}
		return nil
	}
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	h := s.build(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if h.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(h)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h := s.build(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if h.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(h)
}

// build assembles the health document and refreshes the gauges that are cheap
// to compute from the database.
func (s *Server) build(ctx context.Context) model.Health {
	now := s.clock()
	h := model.Health{
		Status:        "ok",
		Version:       s.cfg.Version,
		Commit:        s.cfg.Commit,
		UptimeSeconds: int64(now.Sub(s.started).Seconds()),
		Extra:         map[string]any{},
	}

	h.Database = model.DatabaseHealth{Driver: store.DriverName}
	if s.store != nil {
		if err := s.store.Ping(ctx); err != nil {
			h.Database.Error = err.Error()
			h.Status = "degraded"
		} else {
			h.Database.OK = true
			ver, err := s.store.SchemaVersionFromDB(ctx)
			if err != nil {
				h.Database.Error = err.Error()
				h.Status = "degraded"
			}
			h.Database.SchemaVersion = ver
		}
	} else {
		h.Database.Error = "store not configured"
		h.Status = "degraded"
	}

	if s.telegram != nil {
		h.Telegram = s.telegram.TelegramHealth(ctx)
	} else {
		h.Telegram.Error = "telegram component not configured"
	}
	if !h.Telegram.Running {
		h.Status = "degraded"
	}

	if s.poller != nil {
		h.Poller = s.poller.PollerHealth(ctx)
	}
	if s.poller == nil || !h.Poller.Running {
		// A paused poller is not an error: the process may be running in
		// webhook-only or maintenance mode. Report it, do not fail readiness.
		h.Extra["poller_note"] = "poller not running"
	}
	if h.Poller.WindowSaturated {
		h.Extra["window_warning"] = "at least one branch filled its poll window; raise RDB_TASKS_LIMIT to avoid missing transitions"
	}

	if s.store != nil {
		if counts, err := s.store.OutboxCounts(ctx); err == nil {
			h.Outbox = counts
			if s.metrics != nil {
				s.metrics.OutboxPending.Set(float64(counts.Pending))
				s.metrics.OutboxSending.Set(float64(counts.Sending))
			}
		} else {
			h.Outbox.OldestAge = -1
			h.Extra["outbox_error"] = err.Error()
			h.Status = "degraded"
		}
		if total, enabled, complete, err := s.store.CountChats(ctx); err == nil {
			h.Chats = model.ChatsHealth{Total: total, Enabled: enabled, Complete: complete}
			if s.metrics != nil {
				s.metrics.ChatsTotal.Set(float64(total))
				s.metrics.ChatsEnabled.Set(float64(enabled))
			}
		} else {
			h.Extra["chats_error"] = err.Error()
		}
	}

	if s.notifier != nil {
		running, lastDelivery, err := s.notifier.NotifierHealth(ctx)
		if err != nil {
			h.Extra["notifier_error"] = err.Error()
			h.Status = "degraded"
		}
		h.Extra["notifier_running"] = running
		if !lastDelivery.IsZero() {
			h.Extra["notifier_last_delivery"] = model.FormatTime(lastDelivery)
		}
	}

	if s.lease != nil {
		holder, heartbeat, err := s.lease.LeaseInfo(ctx)
		if err != nil {
			h.Extra["lease_error"] = err.Error()
			h.Status = "degraded"
		} else if holder != "" {
			h.Extra["lease_holder"] = holder
			h.Extra["lease_heartbeat"] = model.FormatTime(heartbeat)
		}
	}

	if len(h.Extra) == 0 {
		h.Extra = nil
	}
	return h
}

// withLogging logs one line per request, never a body.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" || r.URL.Path == "/livez" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Debug("http request",
			"method", r.Method, "path", r.URL.Path,
			"status", rw.status, "bytes", rw.bytes, "duration", time.Since(start).String())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}
