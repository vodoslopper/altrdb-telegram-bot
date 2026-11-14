package poller

import (
	"altrdb-telegram-bot/internal/health"
	"context"
	dto "github.com/prometheus/client_model/go"
	"testing"
	"time"
)

func TestEventCounterCountsNewEventsInsteadOfBaselineOrReplay(t *testing.T) {
	h := newHarness(t, nil)
	h.p.metrics = health.NewMetrics("test", "none")
	ctx := context.Background()
	for _, chatID := range []int64{1, 2} {
		h.subscribe(t, chatID, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	}
	record := taskRecord(1, "sisyphus", "grenka", "BUILDING", epoch)
	h.fake.setFeed("sisyphus", record)
	if err := h.p.PollBranch(ctx, "sisyphus"); err != nil {
		t.Fatal(err)
	}
	check := func(wantEvents, wantNotifications float64) {
		t.Helper()
		ev := &dto.Metric{}
		queued := &dto.Metric{}
		if err := h.p.metrics.EventsCreated.Write(ev); err != nil {
			t.Fatal(err)
		}
		if err := h.p.metrics.NotificationsQueued.Write(queued); err != nil {
			t.Fatal(err)
		}
		if got := ev.GetCounter().GetValue(); got != wantEvents {
			t.Fatalf("events: %g, want %g", got, wantEvents)
		}
		if got := queued.GetCounter().GetValue(); got != wantNotifications {
			t.Fatalf("notifications: %g, want %g", got, wantNotifications)
		}
	}
	check(0, 0)
	record.TaskState = "DONE"
	h.fake.setFeed("sisyphus", record)
	if err := h.p.PollBranch(ctx, "sisyphus"); err != nil {
		t.Fatal(err)
	}
	check(1, 2) // One task event fans out to two subscribers.
	if err := h.p.PollBranch(ctx, "sisyphus"); err != nil {
		t.Fatal(err)
	}
	check(1, 2)
	if _, err := h.store.PruneSnapshots(ctx, epoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := h.p.PollBranch(ctx, "sisyphus"); err != nil {
		t.Fatal(err)
	}
	check(1, 2) // Durable fingerprint dedup suppresses the rediscovered event.
}
