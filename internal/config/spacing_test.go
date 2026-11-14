package config

import (
	"testing"
	"time"
)

func TestSendSpacingAllowsZeroButRejectsNegative(t *testing.T) {
	for _, value := range []string{"0", "0s", "-1ms", "20ms"} {
		t.Run(value, func(t *testing.T) {
			cfg, err := Parse(env(map[string]string{"TELEGRAM_BOT_TOKEN": "123:secret", "NOTIFY_SEND_SPACING": value}))
			if value == "-1ms" {
				if err == nil {
					t.Fatal("negative spacing accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := time.Duration(0)
			if value == "20ms" {
				want = 20 * time.Millisecond
			}
			if cfg.NotifySendSpacing != want {
				t.Fatalf("spacing: %s, want %s", cfg.NotifySendSpacing, want)
			}
		})
	}
	// Other timers still require positive durations.
	if _, err := Parse(env(map[string]string{"TELEGRAM_BOT_TOKEN": "123:secret", "NOTIFY_INTERVAL": "0s"})); err == nil {
		t.Fatal("zero polling interval accepted")
	}
}
