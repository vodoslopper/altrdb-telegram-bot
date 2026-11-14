package poller

import (
	"altrdb-telegram-bot/internal/config"
	"context"
	"testing"
	"time"
)

func TestMalformedRecordsDoNotMaskFullWindow(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.RDBTasksLimit = 2; c.OverlapWindow = 24 * time.Hour })
	h.subscribe(t, 1, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	good := taskRecord(1, "sisyphus", "grenka", "DONE", epoch.Add(-time.Minute))
	bad := taskRecord(2, "sisyphus", "grenka", "DONE", epoch)
	bad.TaskChanged = "invalid"
	for _, allBad := range []bool{false, true} {
		if allBad {
			good.TaskChanged = "also invalid"
		}
		h.fake.setFeed("sisyphus", good, bad)
		if err := h.p.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, err := h.store.GetPollState(context.Background(), "sisyphus")
		if err != nil || !state.WindowSaturated {
			t.Fatalf("malformed records masked saturation: %+v, %v", state, err)
		}
	}
}
