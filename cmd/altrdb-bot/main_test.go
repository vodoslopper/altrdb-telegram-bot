package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/store"
)

func TestNewLoggerRedactsSecrets(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			if newLogger(&config.Config{LogLevel: "info", LogFormat: format}) == nil {
				t.Fatal("newLogger returned nil")
			}

			// The attribute replacer must neutralise every secret-bearing key,
			// whatever the output format is.
			redact := redactAttrs()
			for key := range secretKeys {
				got := redact(nil, slog.String(key, "123456:super-secret"))
				if got.Value.String() != "[redacted]" {
					t.Errorf("%s: %s was logged as %q", format, key, got.Value.String())
				}
			}
			kept := redact(nil, slog.String("branch", "sisyphus"))
			if kept.Value.String() != "sisyphus" {
				t.Errorf("an ordinary attribute was altered: %q", kept.Value.String())
			}
			upper := redact(nil, slog.String("Token", "x"))
			if upper.Value.String() != "[redacted]" {
				t.Error("redaction must be case-insensitive")
			}
		})
	}
}

func TestNewLoggerHonoursLevelAndFormat(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "nonsense"} {
		cfg := &config.Config{LogLevel: level, LogFormat: "json"}
		log := newLogger(cfg)
		if log == nil {
			t.Fatalf("newLogger returned nil for LOG_LEVEL=%s", level)
		}
	}
	cfg := &config.Config{LogLevel: "debug", LogFormat: "json"}
	log := newLogger(cfg)
	if !log.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("LOG_LEVEL=debug must enable debug records")
	}
}

func TestHealthCheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":"test"}`))
	}))
	defer healthy.Close()

	degraded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded"}`))
	}))
	defer degraded.Close()

	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"healthy instance", strings.TrimPrefix(healthy.URL, "http://"), false},
		{"degraded instance", strings.TrimPrefix(degraded.URL, "http://"), true},
		{"nothing listening", "127.0.0.1:1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HTTP_LISTEN_ADDR", tc.addr)
			err := healthCheck()
			if tc.wantErr && err == nil {
				t.Error("expected a non-zero exit for an unhealthy instance")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("healthCheck: %v", err)
			}
		})
	}
}

func TestHealthCheckUsesTheDefaultAddress(t *testing.T) {
	t.Setenv("HTTP_LISTEN_ADDR", "")
	err := healthCheck()
	if err == nil {
		t.Skip("something is actually listening on the default address")
	}
	if !strings.Contains(err.Error(), config.DefaultHTTPListenAddr) {
		t.Errorf("error = %v, want it to name the default address", err)
	}
}

func TestLeaseHolderIsIdentifiable(t *testing.T) {
	cfg := &config.Config{DatabasePath: "/var/lib/altrdb-telegram-bot/bot.db"}
	holder := leaseHolder(cfg)
	if !strings.Contains(holder, "bot.db") {
		t.Errorf("the holder should name the database: %q", holder)
	}
	if !strings.Contains(holder, os.Getenv("HOSTNAME")+":") && strings.Count(holder, ":") < 2 {
		t.Errorf("the holder should identify host and pid: %q", holder)
	}
	parts := strings.Split(holder, ":")
	if len(parts) != 3 {
		t.Errorf("holder = %q, want host:pid:db", holder)
	}
	if parts[1] != itoa(os.Getpid()) {
		t.Errorf("pid component = %q, want %d", parts[1], os.Getpid())
	}

	// A path without a directory component must still work.
	short := leaseHolder(&config.Config{DatabasePath: "bot.db"})
	if !strings.HasSuffix(short, ":bot.db") {
		t.Errorf("holder = %q", short)
	}
}

func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Errorf("orDash(\"\") = %q", got)
	}
	if got := orDash("abc1234"); got != "abc1234" {
		t.Errorf("orDash(\"abc1234\") = %q", got)
	}
}

func TestLeaseAdapterReadsTheStore(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir()+"/bot.db", store.Options{Migrate: true})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.AcquireLease(ctx, "test-host:1:bot.db", 1, config.DefaultLeaseTTL); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	adapter := leaseAdapter{st}
	holder, heartbeat, err := adapter.LeaseInfo(ctx)
	if err != nil {
		t.Fatalf("LeaseInfo: %v", err)
	}
	if holder != "test-host:1:bot.db" {
		t.Errorf("holder = %q", holder)
	}
	if heartbeat.IsZero() {
		t.Error("the heartbeat must be reported")
	}
	if got := model.FormatTime(heartbeat); !strings.HasSuffix(got, "Z") {
		t.Errorf("heartbeat is not UTC: %s", got)
	}
}

// itoa avoids pulling strconv into the assertion for one integer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
