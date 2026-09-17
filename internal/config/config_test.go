package config

import (
	"strings"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/model"
)

// env builds a Lookup from a map so tests never touch the process environment.
func env(kv map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(env(map[string]string{"TELEGRAM_BOT_TOKEN": "  123:secret  "}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"token is trimmed", cfg.TelegramBotToken, "123:secret"},
		{"mode", cfg.TelegramMode, ModePolling},
		{"base url", cfg.RDBBaseURL, DefaultRDBBaseURL},
		{"tasks limit", cfg.RDBTasksLimit, DefaultTasksLimit},
		{"poll interval", cfg.PollInterval, DefaultPollInterval},
		{"listen addr", cfg.HTTPListenAddr, DefaultHTTPListenAddr},
		{"database", cfg.DatabasePath, DefaultDatabasePath},
		{"recent task max age", cfg.RecentTaskMaxAge, DefaultRecentTaskMaxAge},
		{"log level", cfg.LogLevel, "info"},
		{"log format", cfg.LogFormat, "text"},
		{"overlap window", cfg.OverlapWindow, DefaultOverlapWindow},
		{"max concurrent", cfg.MaxConcurrentBranches, DefaultMaxConcurrent},
		{"notify interval", cfg.NotifyInterval, DefaultNotifyInterval},
		{"notify attempts", cfg.NotifyMaxTries, DefaultNotifyMaxTries},
		{"allow groups", cfg.AllowGroups, false},
		{"user agent is set", cfg.RDBUserAgent != "", true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(cfg.TerminalStates) != len(model.DefaultTerminalStates) {
		t.Errorf("terminal states = %v, want %v", cfg.TerminalStates, model.DefaultTerminalStates)
	}
	if len(cfg.AllowedTelegramUsers) != 0 {
		t.Errorf("allowlist should default to empty, got %v", cfg.AllowedTelegramUsers)
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse(env(map[string]string{
		"TELEGRAM_BOT_TOKEN":          "1:abc",
		"RDB_BASE_URL":                "https://example.test/api/",
		"DATABASE_PATH":               "/tmp/x/bot.db",
		"POLL_INTERVAL":               "90s",
		"RDB_TASKS_LIMIT":             "500",
		"HTTP_LISTEN_ADDR":            "127.0.0.1:9999",
		"LOG_LEVEL":                   "DEBUG",
		"LOG_FORMAT":                  "json",
		"ALLOWED_TELEGRAM_USER_IDS":   " 11 , 22 ,,33 ",
		"ALLOW_TELEGRAM_GROUPS":       "true",
		"TERMINAL_STATES":             "done, failed , TESTED",
		"RDB_MAX_CONCURRENT_BRANCHES": "5",
		"RDB_OVERLAP_WINDOW":          "6h",
		"NOTIFY_INTERVAL":             "2s",
		"NOTIFY_MAX_ATTEMPTS":         "3",
		"SNAPSHOT_RETENTION":          "72h",
		"RECENT_TASK_MAX_AGE":         "48h",
		"RDB_USER_AGENT":              "test-agent/1.0",
	}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.RDBBaseURL != "https://example.test/api" {
		t.Errorf("trailing slash not trimmed: %q", cfg.RDBBaseURL)
	}
	if cfg.PollInterval != 90*time.Second {
		t.Errorf("poll interval = %s", cfg.PollInterval)
	}
	if cfg.RDBTasksLimit != 500 {
		t.Errorf("tasks limit = %d", cfg.RDBTasksLimit)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("log settings = %s/%s", cfg.LogLevel, cfg.LogFormat)
	}
	want := []int64{11, 22, 33}
	if len(cfg.AllowedTelegramUsers) != len(want) {
		t.Fatalf("allowlist = %v, want %v", cfg.AllowedTelegramUsers, want)
	}
	for i := range want {
		if cfg.AllowedTelegramUsers[i] != want[i] {
			t.Errorf("allowlist[%d] = %d, want %d", i, cfg.AllowedTelegramUsers[i], want[i])
		}
	}
	if !cfg.AllowGroups {
		t.Error("groups should be allowed")
	}
	wantStates := []string{"DONE", "FAILED", "TESTED"}
	if strings.Join(cfg.TerminalStates, ",") != strings.Join(wantStates, ",") {
		t.Errorf("terminal states = %v, want %v", cfg.TerminalStates, wantStates)
	}
	if cfg.RDBUserAgent != "test-agent/1.0" {
		t.Errorf("user agent = %q", cfg.RDBUserAgent)
	}
	if cfg.RecentTaskMaxAge != 48*time.Hour {
		t.Errorf("recent task max age = %s", cfg.RecentTaskMaxAge)
	}
}

func TestTokenIsRequiredToServeButNotToParse(t *testing.T) {
	// Parsing must succeed without a token so that -migrate and -healthcheck
	// work on a machine that has no bot configured yet.
	cfg, err := Parse(env(map[string]string{}))
	if err != nil {
		t.Fatalf("Parse without a token: %v", err)
	}
	if err := cfg.ValidateServing(); err == nil {
		t.Error("ValidateServing must reject an empty token")
	} else if !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN is required") {
		t.Errorf("error = %q", err.Error())
	}

	cfg.TelegramBotToken = "1:abc"
	if err := cfg.ValidateServing(); err != nil {
		t.Errorf("ValidateServing with a token: %v", err)
	}
}

func TestAllowUser(t *testing.T) {
	tests := []struct {
		name     string
		allow    []int64
		id       int64
		expected bool
	}{
		{"empty allowlist permits everyone", nil, 42, true},
		{"listed id is permitted", []int64{1, 42}, 42, true},
		{"unlisted id is refused", []int64{1, 42}, 7, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{AllowedTelegramUsers: tc.allow}
			if got := cfg.AllowUser(tc.id); got != tc.expected {
				t.Errorf("AllowUser(%d) = %t, want %t", tc.id, got, tc.expected)
			}
		})
	}
}

func TestParseValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "poll interval below the minimum",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "POLL_INTERVAL": "5s"},
			wantErr: "POLL_INTERVAL must be at least",
		},
		{
			name:    "poll interval unparsable",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "POLL_INTERVAL": "soon"},
			wantErr: "POLL_INTERVAL",
		},
		{
			name:    "unknown mode",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_MODE": "carrier-pigeon"},
			wantErr: "TELEGRAM_MODE must be",
		},
		{
			name:    "webhook mode without url or secret",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_MODE": "webhook"},
			wantErr: "TELEGRAM_WEBHOOK_URL is required",
		},
		{
			name: "webhook url must be https",
			env: map[string]string{
				"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_MODE": "webhook",
				"TELEGRAM_WEBHOOK_URL":    "http://example.test/hook",
				"TELEGRAM_WEBHOOK_SECRET": "0123456789abcdef",
			},
			wantErr: "must use https://",
		},
		{
			name: "webhook secret too short",
			env: map[string]string{
				"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_MODE": "webhook",
				"TELEGRAM_WEBHOOK_URL":    "https://example.test/hook",
				"TELEGRAM_WEBHOOK_SECRET": "short",
			},
			wantErr: "at least 16 characters",
		},
		{
			name:    "bad allowlist entry",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "ALLOWED_TELEGRAM_USER_IDS": "1,notanumber"},
			wantErr: "is not an integer",
		},
		{
			name:    "bad listen address",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "HTTP_LISTEN_ADDR": "localhost"},
			wantErr: "HTTP_LISTEN_ADDR must be host:port",
		},
		{
			name:    "bad log level",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "LOG_LEVEL": "shout"},
			wantErr: "LOG_LEVEL must be",
		},
		{
			name:    "bad base url",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "RDB_BASE_URL": "rdb.altlinux.org/api"},
			wantErr: "RDB_BASE_URL must start with",
		},
		{
			name:    "tasks limit out of range",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "RDB_TASKS_LIMIT": "0"},
			wantErr: "RDB_TASKS_LIMIT must be between",
		},
		{
			name:    "negative duration",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "NOTIFY_INTERVAL": "-5s"},
			wantErr: "NOTIFY_INTERVAL must be positive",
		},
		{
			name:    "lease heartbeat longer than ttl",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": "t", "LEASE_HEARTBEAT": "5m", "LEASE_TTL": "1m"},
			wantErr: "LEASE_HEARTBEAT",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(env(tc.env))
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Parse(env(map[string]string{
		"POLL_INTERVAL":   "1s",
		"LOG_LEVEL":       "nope",
		"RDB_TASKS_LIMIT": "99999",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"POLL_INTERVAL", "LOG_LEVEL", "RDB_TASKS_LIMIT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err.Error(), want)
		}
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	cfg, err := Parse(env(map[string]string{
		"TELEGRAM_BOT_TOKEN":      "123456:super-secret",
		"TELEGRAM_MODE":           "webhook",
		"TELEGRAM_WEBHOOK_URL":    "https://example.test/hook",
		"TELEGRAM_WEBHOOK_SECRET": "0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := cfg.Redacted()
	if r.TelegramBotToken != "[redacted]" {
		t.Errorf("token not redacted: %q", r.TelegramBotToken)
	}
	if r.TelegramWebhookSecret != "[redacted]" {
		t.Errorf("webhook secret not redacted: %q", r.TelegramWebhookSecret)
	}
	if s := cfg.String(); strings.Contains(s, "super-secret") || strings.Contains(s, "0123456789abcdef") {
		t.Errorf("String() leaked a secret: %s", s)
	}
	if cfg.TelegramBotToken != "123456:super-secret" {
		t.Error("Redacted() must not mutate the original")
	}
}

func TestWebhookPath(t *testing.T) {
	cfg := &Config{}
	if got := cfg.WebhookPath(); got != "/telegram/webhook" {
		t.Errorf("WebhookPath() = %q", got)
	}
}
