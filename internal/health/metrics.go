// Package health exposes the operator surface: liveness, readiness, a JSON
// status document and Prometheus metrics.
package health

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the program's Prometheus collector set. Everything lives in one
// private registry so tests can assert on exposition output and the default
// process collector does not leak into /metrics.
type Metrics struct {
	registry *prometheus.Registry

	PollsTotal          *prometheus.CounterVec
	PollDurationSeconds *prometheus.HistogramVec
	TasksSeen           *prometheus.GaugeVec
	WindowSaturated     *prometheus.GaugeVec
	LastSuccessfulPoll  *prometheus.GaugeVec
	RecordsSkipped      prometheus.Counter

	EventsCreated       prometheus.Counter
	DuplicateEvents     prometheus.Counter
	NotificationsQueued prometheus.Counter

	OutboxPending        prometheus.Gauge
	OutboxSending        prometheus.Gauge
	OutboxDead           prometheus.Counter
	TelegramSendsTotal   *prometheus.CounterVec
	TelegramUpdatesTotal *prometheus.CounterVec
	TelegramLoopRunning  prometheus.Gauge

	RDBRequestsTotal  prometheus.Counter
	RDBRetriesTotal   prometheus.Counter
	RDBRateLimitTotal prometheus.Counter

	ChatsTotal   prometheus.Gauge
	ChatsEnabled prometheus.Gauge

	BuildInfo *prometheus.GaugeVec
}

// NewMetrics builds and registers the collector set.
func NewMetrics(version, commit string) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,

		PollsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "polls_total",
			Help:      "RDB task-feed polls, by branch and result.",
		}, []string{"branch", "result"}),

		PollDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "altrdb",
			Name:      "poll_duration_seconds",
			Help:      "How long one branch poll took, including retries.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
		}, []string{"branch"}),

		TasksSeen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "tasks_seen",
			Help:      "Records returned by the most recent poll of a branch.",
		}, []string{"branch"}),

		WindowSaturated: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "poll_window_saturated",
			Help:      "1 when a poll filled tasks_limit without reaching back over the overlap window: transitions may be missed.",
		}, []string{"branch"}),

		LastSuccessfulPoll: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "last_successful_poll_timestamp_seconds",
			Help:      "Unix time of the last successful poll, per branch.",
		}, []string{"branch"}),

		RecordsSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "records_skipped_total",
			Help:      "Task records dropped because they failed validation.",
		}),

		EventsCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "events_created_total",
			Help:      "Task transitions detected by the poller.",
		}),

		DuplicateEvents: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "duplicate_events_suppressed_total",
			Help:      "Outbox inserts rejected by the UNIQUE (chat_id, fingerprint) constraint.",
		}),

		NotificationsQueued: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "notifications_queued_total",
			Help:      "Outbox rows created.",
		}),

		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "outbox_pending",
			Help:      "Notifications waiting to be delivered.",
		}),

		OutboxSending: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "outbox_sending",
			Help:      "Notifications currently claimed by the delivery worker.",
		}),

		OutboxDead: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "outbox_dead_total",
			Help:      "Notifications given up on, either exhausted retries or a permanently rejected chat.",
		}),

		TelegramSendsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "telegram_sends_total",
			Help:      "Telegram API calls made by the delivery worker, by result.",
		}, []string{"result"}),

		TelegramUpdatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "telegram_updates_total",
			Help:      "Updates received from Telegram, by kind.",
		}, []string{"kind"}),

		TelegramLoopRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "telegram_loop_running",
			Help:      "1 while the Telegram update loop (polling or webhook) is serving.",
		}),

		RDBRequestsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "rdb_requests_total",
			Help:      "HTTP requests made to the ALT RDB API.",
		}),

		RDBRetriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "rdb_retries_total",
			Help:      "RDB request retries.",
		}),

		RDBRateLimitTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "altrdb",
			Name:      "rdb_rate_limited_total",
			Help:      "HTTP 429 responses from the ALT RDB API.",
		}),

		ChatsTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "chats_total",
			Help:      "Chats that have ever talked to the bot.",
		}),

		ChatsEnabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "chats_enabled",
			Help:      "Chats currently receiving notifications.",
		}),

		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "altrdb",
			Name:      "build_info",
			Help:      "Constant 1, labelled with the running version and commit.",
		}, []string{"version", "commit"}),
	}

	reg.MustRegister(
		m.PollsTotal, m.PollDurationSeconds, m.TasksSeen, m.WindowSaturated,
		m.LastSuccessfulPoll, m.RecordsSkipped,
		m.EventsCreated, m.DuplicateEvents, m.NotificationsQueued,
		m.OutboxPending, m.OutboxSending, m.OutboxDead,
		m.TelegramSendsTotal, m.TelegramUpdatesTotal, m.TelegramLoopRunning,
		m.RDBRequestsTotal, m.RDBRetriesTotal, m.RDBRateLimitTotal,
		m.ChatsTotal, m.ChatsEnabled, m.BuildInfo,
	)
	m.BuildInfo.WithLabelValues(version, commit).Set(1)
	return m
}

// Registry is the private Prometheus registry served at /metrics.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
