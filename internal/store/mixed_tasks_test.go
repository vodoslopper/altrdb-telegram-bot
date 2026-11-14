package store

import (
	"altrdb-telegram-bot/internal/model"
	"context"
	"testing"
	"time"
)

func TestMixedRepositoryFeedUsesIndependentSnapshots(t *testing.T) {
	st, cl := newTestStore(t)
	ctx := context.Background()
	subscribe(t, st, 1, []string{"grenka"}, []string{"DONE", "FAILED"}, []string{"sisyphus", "p11"}, epoch.Add(-time.Hour))
	tasks := []model.Task{task(1, "sisyphus", "grenka", "DONE", epoch), task(1, "p11", "grenka", "FAILED", epoch)}
	if _, err := st.ApplyTasks(ctx, tasks, true, terminal); err != nil {
		t.Fatal(err)
	}
	cl.Advance(time.Hour)
	res, err := st.ApplyTasks(ctx, tasks, false, terminal)
	if err != nil || res.Changed != 0 || res.Notifications != 0 {
		t.Fatalf("unchanged mixed feed: %+v, %v", res, err)
	}
	tasks[1].State = "DONE"
	tasks[1].Changed = cl.Now()
	res, err = st.ApplyTasks(ctx, tasks, false, terminal)
	if err != nil || len(res.Events) != 1 {
		t.Fatalf("changed mixed feed: %+v, %v", res, err)
	}
	ev := res.Events[0]
	if ev.FirstObserved || ev.PreviousState != "FAILED" || ev.TaskRepo != "p11" {
		t.Fatalf("wrong previous snapshot: %+v", ev)
	}
	snap, err := st.GetSnapshot(ctx, "p11", 1)
	if err != nil || !snap.FirstSeenAt.Equal(epoch) {
		t.Fatalf("first seen lost: %+v, %v", snap, err)
	}
}
