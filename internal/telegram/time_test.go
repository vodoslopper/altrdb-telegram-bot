package telegram

import (
	"altrdb-telegram-bot/internal/model"
	"strings"
	"testing"
	"time"
)

func TestFormatWhenUsesProvidedClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		hint string
	}{
		{"recent", 30 * time.Second, "(just now)"},
		{"minutes", 20 * time.Minute, "(20m ago)"},
		{"hours", 3 * time.Hour, "(3h ago)"},
		{"old", 72 * time.Hour, ""},
		{"future", -time.Hour, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := epoch.Add(-tc.age)
			want := changed.UTC().Format("2006-01-02 15:04 UTC")
			if tc.hint != "" {
				want += " " + tc.hint
			}
			if got := formatWhen(changed, epoch); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
	if got := formatWhen(time.Time{}, epoch); got != "unknown" {
		t.Fatal(got)
	}
}

func TestRecentRelativeTimeAdvancesWithBotClock(t *testing.T) {
	h := newHarness(t, nil)
	h.subscribe(testChatID, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-2*time.Hour))
	task := model.Task{TaskID: 123, TaskRepo: "sisyphus", Owner: "grenka", State: "DONE", Changed: epoch.Add(-time.Hour)}
	if _, err := h.store.ApplyTasks(h.ctx, []model.Task{task}, true, h.tg.terminal); err != nil {
		t.Fatal(err)
	}
	h.send("/recent")
	if text := h.lastText("sendMessage"); !strings.Contains(text, "(1h ago)") {
		t.Fatalf("relative hint: %s", text)
	}
	h.clock.Advance(time.Hour)
	h.send("/recent")
	if text := h.lastText("sendMessage"); !strings.Contains(text, "(2h ago)") {
		t.Fatalf("relative hint did not advance: %s", text)
	}
	detail := renderTaskDetail(model.TaskEvent{Changed: task.Changed}, nil, 0, h.clock.Now())
	if !strings.Contains(detail, "(2h ago)") {
		t.Fatalf("detail relative hint: %s", detail)
	}
}
