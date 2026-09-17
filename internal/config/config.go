// Package config loads and validates the process configuration.
//
// Everything comes from environment variables so the same binary works under
// systemd (EnvironmentFile=), Docker (env_file) and a plain shell. There are
// no config files to drift and no secrets on the command line.
package config

import (
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"altrdb-telegram-bot/internal/model"
)

// TelegramMode selects how updates reach the bot.
type TelegramMode string

const (
	// ModePolling uses getUpdates. Default: needs no inbound endpoint.
	ModePolling TelegramMode = "polling"
	// ModeWebhook serves Telegram's POSTs. Needs a public HTTPS endpoint.
	ModeWebhook TelegramMode = "webhook"
)

// Defaults. These are the values documented in README.md and .env.example.
const (
	DefaultRDBBaseURL       = "https://rdb.altlinux.org/api"
	DefaultDatabasePath     = "/var/lib/altrdb-telegram-bot/bot.db"
	DefaultPollInterval     = 60 * time.Second
	MinPollInterval         = 15 * time.Second
	DefaultTasksLimit       = 200
	DefaultHTTPListenAddr   = "127.0.0.1:8080"
	DefaultLogLevel         = "info"
	DefaultNotifyInterval   = 5 * time.Second
	DefaultNotifyMaxTries   = 8
	DefaultSendSpacing      = 50 * time.Millisecond
	DefaultHTTPTimeout      = 30 * time.Second
	DefaultConnectTimeout   = 10 * time.Second
	DefaultTLSTimeout       = 10 * time.Second
	DefaultBranchRefresh    = 30 * time.Minute
	DefaultMaintRefresh     = 6 * time.Hour
	DefaultOverlapWindow    = 24 * time.Hour
	DefaultSnapshotKeepFor  = 30 * 24 * time.Hour
	DefaultRecentTaskMaxAge = 30 * 24 * time.Hour
	DefaultLeaseTTL         = 90 * time.Second
	DefaultLeaseHeartbeat   = 30 * time.Second
	DefaultMaxConcurrent    = 3
	// MaxTelegramMessageLen is Telegram's hard limit on message text.
	MaxTelegramMessageLen = 4096
)

// Config is the validated process configuration.
type Config struct {
	// Telegram
	TelegramBotToken      string
	TelegramMode          TelegramMode
	TelegramWebhookURL    string
	TelegramWebhookSecret string
	AllowedTelegramUsers  []int64
	AllowGroups           bool

	// ALT RDB
	RDBBaseURL             string
	RDBTasksLimit          int
	RDBUserAgent           string
	PollInterval           time.Duration
	BranchRefreshInterval  time.Duration
	MaintainerRefreshEvery time.Duration
	MaxConcurrentBranches  int
	OverlapWindow          time.Duration

	// HTTP client used for RDB (and for the health probe helpers)
	HTTPTimeout        time.Duration
	HTTPConnectTimeout time.Duration
	HTTPTLSTimeout     time.Duration

	// Storage
	DatabasePath     string
	SnapshotKeepFor  time.Duration
	RecentTaskMaxAge time.Duration
	LeaseTTL         time.Duration
	LeaseHeartbeat   time.Duration

	// Delivery
	NotifyInterval    time.Duration
	NotifyMaxTries    int
	NotifySendSpacing time.Duration
	TerminalStates    []string

	// Observability
	HTTPListenAddr string
	LogLevel       string
	LogFormat      string // text | json

	// Build identity, set with -ldflags.
	Version string
	Commit  string
}

// Lookup reads one environment variable. Split out so tests can supply a map
// instead of mutating the process environment.
type Lookup func(key string) (string, bool)

// OS returns a Lookup backed by os.LookupEnv.
func OS() Lookup { return os.LookupEnv }

// Load reads the process environment.
func Load() (*Config, error) { return Parse(OS()) }

// Parse builds and validates a Config from the supplied environment.
func Parse(get Lookup) (*Config, error) {
	cfg := &Config{
		TelegramMode:           ModePolling,
		RDBBaseURL:             DefaultRDBBaseURL,
		RDBTasksLimit:          DefaultTasksLimit,
		PollInterval:           DefaultPollInterval,
		BranchRefreshInterval:  DefaultBranchRefresh,
		MaintainerRefreshEvery: DefaultMaintRefresh,
		MaxConcurrentBranches:  DefaultMaxConcurrent,
		OverlapWindow:          DefaultOverlapWindow,
		HTTPTimeout:            DefaultHTTPTimeout,
		HTTPConnectTimeout:     DefaultConnectTimeout,
		HTTPTLSTimeout:         DefaultTLSTimeout,
		DatabasePath:           DefaultDatabasePath,
		SnapshotKeepFor:        DefaultSnapshotKeepFor,
		RecentTaskMaxAge:       DefaultRecentTaskMaxAge,
		LeaseTTL:               DefaultLeaseTTL,
		LeaseHeartbeat:         DefaultLeaseHeartbeat,
		NotifyInterval:         DefaultNotifyInterval,
		NotifyMaxTries:         DefaultNotifyMaxTries,
		NotifySendSpacing:      DefaultSendSpacing,
		TerminalStates:         slices.Clone(model.DefaultTerminalStates),
		HTTPListenAddr:         DefaultHTTPListenAddr,
		LogLevel:               DefaultLogLevel,
		LogFormat:              "text",
		Version:                Version,
		Commit:                 Commit,
	}

	var errs []error

	// The token is required to *serve*, not to parse: -migrate and -healthcheck
	// must work on a machine that has no bot configured yet, and a package
	// post-install step must not need the secret to apply the schema.
	cfg.TelegramBotToken = strings.TrimSpace(getenv(get, "TELEGRAM_BOT_TOKEN"))

	if v := getenv(get, "TELEGRAM_MODE"); v != "" {
		switch TelegramMode(strings.ToLower(v)) {
		case ModePolling, ModeWebhook:
			cfg.TelegramMode = TelegramMode(strings.ToLower(v))
		default:
			errs = append(errs, fmt.Errorf("TELEGRAM_MODE must be %q or %q, got %q", ModePolling, ModeWebhook, v))
		}
	}
	cfg.TelegramWebhookURL = strings.TrimSpace(getenv(get, "TELEGRAM_WEBHOOK_URL"))
	cfg.TelegramWebhookSecret = strings.TrimSpace(getenv(get, "TELEGRAM_WEBHOOK_SECRET"))

	ids, err := parseInt64List(getenv(get, "ALLOWED_TELEGRAM_USER_IDS"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ALLOWED_TELEGRAM_USER_IDS: %w", err))
	}
	cfg.AllowedTelegramUsers = ids

	cfg.AllowGroups, err = parseBool(getenv(get, "ALLOW_TELEGRAM_GROUPS"), false)
	if err != nil {
		errs = append(errs, fmt.Errorf("ALLOW_TELEGRAM_GROUPS: %w", err))
	}

	if v := getenv(get, "RDB_BASE_URL"); v != "" {
		cfg.RDBBaseURL = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if u := cfg.RDBBaseURL; !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		errs = append(errs, fmt.Errorf("RDB_BASE_URL must start with http:// or https://, got %q", u))
	}

	if v, ok := get("RDB_TASKS_LIMIT"); ok && strings.TrimSpace(v) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("RDB_TASKS_LIMIT: %w", err))
		} else if n < 1 || n > 5000 {
			errs = append(errs, fmt.Errorf("RDB_TASKS_LIMIT must be between 1 and 5000, got %d", n))
		} else {
			cfg.RDBTasksLimit = n
		}
	}

	if v, ok := get("POLL_INTERVAL"); ok && strings.TrimSpace(v) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("POLL_INTERVAL: %w", err))
		} else if d < MinPollInterval {
			errs = append(errs, fmt.Errorf("POLL_INTERVAL must be at least %s, got %s", MinPollInterval, d))
		} else {
			cfg.PollInterval = d
		}
	}

	if d, err := parseDuration(get, "BRANCH_REFRESH_INTERVAL", cfg.BranchRefreshInterval); err != nil {
		errs = append(errs, err)
	} else {
		cfg.BranchRefreshInterval = d
	}
	if d, err := parseDuration(get, "MAINTAINER_CACHE_TTL", cfg.MaintainerRefreshEvery); err != nil {
		errs = append(errs, err)
	} else {
		cfg.MaintainerRefreshEvery = d
	}
	if d, err := parseDuration(get, "RDB_OVERLAP_WINDOW", cfg.OverlapWindow); err != nil {
		errs = append(errs, err)
	} else if d < time.Minute {
		errs = append(errs, fmt.Errorf("RDB_OVERLAP_WINDOW must be at least 1m, got %s", d))
	} else {
		cfg.OverlapWindow = d
	}
	if n, err := parseInt(get, "RDB_MAX_CONCURRENT_BRANCHES", cfg.MaxConcurrentBranches); err != nil {
		errs = append(errs, err)
	} else if n < 1 || n > 16 {
		errs = append(errs, fmt.Errorf("RDB_MAX_CONCURRENT_BRANCHES must be between 1 and 16, got %d", n))
	} else {
		cfg.MaxConcurrentBranches = n
	}
	if v := getenv(get, "RDB_USER_AGENT"); v != "" {
		cfg.RDBUserAgent = strings.TrimSpace(v)
	}
	if cfg.RDBUserAgent == "" {
		cfg.RDBUserAgent = fmt.Sprintf("altrdb-telegram-bot/%s (+https://git.altlinux.org/tasks/)", shortVersion(cfg.Version))
	}

	if d, err := parseDuration(get, "HTTP_TIMEOUT", cfg.HTTPTimeout); err != nil {
		errs = append(errs, err)
	} else {
		cfg.HTTPTimeout = d
	}
	if d, err := parseDuration(get, "HTTP_CONNECT_TIMEOUT", cfg.HTTPConnectTimeout); err != nil {
		errs = append(errs, err)
	} else {
		cfg.HTTPConnectTimeout = d
	}
	if d, err := parseDuration(get, "HTTP_TLS_TIMEOUT", cfg.HTTPTLSTimeout); err != nil {
		errs = append(errs, err)
	} else {
		cfg.HTTPTLSTimeout = d
	}

	if v := getenv(get, "DATABASE_PATH"); v != "" {
		cfg.DatabasePath = strings.TrimSpace(v)
	}
	if d, err := parseDuration(get, "SNAPSHOT_RETENTION", cfg.SnapshotKeepFor); err != nil {
		errs = append(errs, err)
	} else {
		cfg.SnapshotKeepFor = d
	}
	if d, err := parseDuration(get, "RECENT_TASK_MAX_AGE", cfg.RecentTaskMaxAge); err != nil {
		errs = append(errs, err)
	} else {
		cfg.RecentTaskMaxAge = d
	}
	if d, err := parseDuration(get, "LEASE_TTL", cfg.LeaseTTL); err != nil {
		errs = append(errs, err)
	} else {
		cfg.LeaseTTL = d
	}
	if d, err := parseDuration(get, "LEASE_HEARTBEAT", cfg.LeaseHeartbeat); err != nil {
		errs = append(errs, err)
	} else {
		cfg.LeaseHeartbeat = d
	}

	if d, err := parseDuration(get, "NOTIFY_INTERVAL", cfg.NotifyInterval); err != nil {
		errs = append(errs, err)
	} else if d < time.Second {
		errs = append(errs, fmt.Errorf("NOTIFY_INTERVAL must be at least 1s, got %s", d))
	} else {
		cfg.NotifyInterval = d
	}
	if n, err := parseInt(get, "NOTIFY_MAX_ATTEMPTS", cfg.NotifyMaxTries); err != nil {
		errs = append(errs, err)
	} else if n < 1 || n > 100 {
		errs = append(errs, fmt.Errorf("NOTIFY_MAX_ATTEMPTS must be between 1 and 100, got %d", n))
	} else {
		cfg.NotifyMaxTries = n
	}
	if d, err := parseDuration(get, "NOTIFY_SEND_SPACING", cfg.NotifySendSpacing); err != nil {
		errs = append(errs, err)
	} else {
		cfg.NotifySendSpacing = d
	}
	if v := getenv(get, "TERMINAL_STATES"); v != "" {
		states := splitList(v)
		if len(states) == 0 {
			errs = append(errs, fmt.Errorf("TERMINAL_STATES must not be empty when set"))
		}
		cfg.TerminalStates = states
	}

	if v := getenv(get, "HTTP_LISTEN_ADDR"); v != "" {
		cfg.HTTPListenAddr = strings.TrimSpace(v)
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("HTTP_LISTEN_ADDR must be host:port, got %q: %w", cfg.HTTPListenAddr, err))
	}

	if v := getenv(get, "LOG_LEVEL"); v != "" {
		cfg.LogLevel = strings.ToLower(strings.TrimSpace(v))
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, cfg.LogLevel) {
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", cfg.LogLevel))
	}
	if v := getenv(get, "LOG_FORMAT"); v != "" {
		cfg.LogFormat = strings.ToLower(strings.TrimSpace(v))
	}
	if !slices.Contains([]string{"text", "json"}, cfg.LogFormat) {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be text or json, got %q", cfg.LogFormat))
	}

	if err := cfg.validateModeSpecific(); err != nil {
		errs = append(errs, err)
	}
	if cfg.LeaseHeartbeat >= cfg.LeaseTTL {
		errs = append(errs, fmt.Errorf("LEASE_HEARTBEAT (%s) must be shorter than LEASE_TTL (%s)", cfg.LeaseHeartbeat, cfg.LeaseTTL))
	}

	if len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	return cfg, nil
}

// validateModeSpecific enforces the webhook-only requirements and the mutual
// exclusion of the two Telegram modes.
func (c *Config) validateModeSpecific() error {
	if c.TelegramMode != ModeWebhook {
		if c.TelegramWebhookURL != "" || c.TelegramWebhookSecret != "" {
			// Not fatal: the operator may keep them in one EnvironmentFile
			// and switch modes. They are simply ignored.
			return nil
		}
		return nil
	}
	var errs []error
	if c.TelegramWebhookURL == "" {
		errs = append(errs, fmt.Errorf("TELEGRAM_WEBHOOK_URL is required when TELEGRAM_MODE=webhook"))
	} else if !strings.HasPrefix(c.TelegramWebhookURL, "https://") {
		errs = append(errs, fmt.Errorf("TELEGRAM_WEBHOOK_URL must use https://, got %q", c.TelegramWebhookURL))
	}
	if c.TelegramWebhookSecret == "" {
		errs = append(errs, fmt.Errorf("TELEGRAM_WEBHOOK_SECRET is required when TELEGRAM_MODE=webhook"))
	} else if len(c.TelegramWebhookSecret) < 16 {
		errs = append(errs, fmt.Errorf("TELEGRAM_WEBHOOK_SECRET must be at least 16 characters"))
	}
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}

// ValidateServing checks what only the running service needs: a bot token.
// Parse already rejected everything else that could be wrong.
func (c *Config) ValidateServing() error {
	if c.TelegramBotToken == "" {
		return fmt.Errorf("invalid configuration:\n  - TELEGRAM_BOT_TOKEN is required")
	}
	return nil
}

// WebhookPath is the path the webhook mode serves Telegram updates on. It is
// mounted on the same health listener, so no extra port is opened.
func (c *Config) WebhookPath() string { return "/telegram/webhook" }

// AllowUser reports whether a Telegram user id may drive the bot.
func (c *Config) AllowUser(id int64) bool {
	if len(c.AllowedTelegramUsers) == 0 {
		return true
	}
	return slices.Contains(c.AllowedTelegramUsers, id)
}

// Redacted returns a copy without the bot token and webhook secret, safe to
// log at startup.
func (c *Config) Redacted() *Config {
	cp := *c
	if cp.TelegramBotToken != "" {
		cp.TelegramBotToken = "[redacted]"
	}
	if cp.TelegramWebhookSecret != "" {
		cp.TelegramWebhookSecret = "[redacted]"
	}
	return &cp
}

// String renders the redacted configuration for the startup log line.
func (c *Config) String() string {
	r := c.Redacted()
	return fmt.Sprintf(
		"telegram_mode=%s rdb_base_url=%s poll_interval=%s tasks_limit=%d db=%s listen=%s log=%s/%s allowed_users=%d groups=%t",
		r.TelegramMode, r.RDBBaseURL, r.PollInterval, r.RDBTasksLimit, r.DatabasePath,
		r.HTTPListenAddr, r.LogFormat, r.LogLevel, len(r.AllowedTelegramUsers), r.AllowGroups)
}

// getenv returns the trimmed value or "".
func getenv(get Lookup, key string) string {
	v, ok := get(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

func parseDuration(get Lookup, key string, def time.Duration) (time.Duration, error) {
	v := getenv(get, key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return def, fmt.Errorf("%s must be positive, got %s", key, d)
	}
	return d, nil
}

func parseInt(get Lookup, key string, def int) (int, error) {
	v := getenv(get, key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func parseBool(v string, def bool) (bool, error) {
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def, fmt.Errorf("must be a boolean (1/0/true/false), got %q", v)
	}
	return b, nil
}

func parseInt64List(v string) ([]int64, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []int64
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.ToUpper(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func shortVersion(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

func joinErrors(errs []error) error {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(msgs, "\n  - "))
}
