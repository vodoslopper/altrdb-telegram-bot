package notifier

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/model"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestTokenFaultStopsRunnerAndPreservesAllQueues(t *testing.T) {
	h := newHarness(t, bot.ErrorUnauthorized)
	ids := []int64{h.enqueue(t, 1, "a", epoch), h.enqueue(t, 1, "b", epoch), h.enqueue(t, 2, "c", epoch)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.n.Run(ctx); !errors.Is(err, bot.ErrorUnauthorized) {
		t.Fatalf("Run: %v", err)
	}
	if h.sender.calls() != 1 {
		t.Fatal("continued sending after token fault")
	}
	for _, id := range ids {
		r := h.row(t, id)
		if r.Status != model.OutboxPending || r.Attempts != 0 {
			t.Fatalf("queue destroyed: %+v", r)
		}
	}
	for _, id := range []int64{1, 2} {
		c, err := h.store.GetChat(ctx, id)
		if err != nil || !c.Enabled {
			t.Fatalf("chat disabled: %+v, %v", c, err)
		}
	}
}

func TestPayloadFailureDoesNotDisableChatOrCancelOtherRows(t *testing.T) {
	h := newHarness(t, fmt.Errorf("%w: can't parse entities", bot.ErrorBadRequest))
	first := h.enqueue(t, 1, "a", epoch)
	second := h.enqueue(t, 1, "b", epoch)
	h.n.Drain(context.Background())
	c, err := h.store.GetChat(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || h.row(t, first).Status != model.OutboxDead || h.row(t, second).Status != model.OutboxSent {
		t.Fatal("payload error affected healthy chat or other rows")
	}
}

func TestCancelledBatchRowsAreNotSent(t *testing.T) {
	h := newHarness(t, bot.ErrorForbidden)
	h.enqueue(t, 1, "a", epoch)
	second := h.enqueue(t, 1, "b", epoch)
	h.n.Drain(context.Background())
	if h.sender.calls() != 1 || h.row(t, second).Status != model.OutboxDead {
		t.Fatal("sent cancelled batch row")
	}
	if err := h.store.RetryNotification(context.Background(), second, "late retry", epoch, false); err != nil {
		t.Fatal(err)
	}
	if h.row(t, second).Status != model.OutboxDead {
		t.Fatal("retry resurrected cancellation")
	}
}

func TestRateLimitPausesEntireDrain(t *testing.T) {
	h := newHarness(t, &bot.TooManyRequestsError{RetryAfter: 30})
	h.enqueue(t, 1, "a", epoch)
	second := h.enqueue(t, 2, "b", epoch)
	h.n.Drain(context.Background())
	h.n.Drain(context.Background())
	if h.sender.calls() != 1 {
		t.Fatal("continued sending during retry_after")
	}
	if r := h.row(t, second); r.Status != model.OutboxPending || r.Attempts != 0 {
		t.Fatalf("unsent claim not released: %+v", r)
	}
	h.clock.Advance(31 * time.Second)
	if h.n.Drain(context.Background()) != 2 {
		t.Fatal("did not resume delivery")
	}
}

func TestHealthRecoversAfterSuccessfulRetry(t *testing.T) {
	h := newHarness(t, errors.New("temporary failure"))
	h.enqueue(t, 1, "a", epoch)
	h.n.Drain(context.Background())
	_, last, err := h.n.NotifierHealth(context.Background())
	if err == nil || !last.IsZero() {
		t.Fatal("failure health incorrect")
	}
	h.clock.Advance(time.Hour)
	h.n.Drain(context.Background())
	_, last, err = h.n.NotifierHealth(context.Background())
	if err != nil || last.IsZero() {
		t.Fatalf("health did not recover: %v", err)
	}
}

type cancelSender struct {
	cancel context.CancelFunc
	calls  int
}

func (s *cancelSender) SendMessage(ctx context.Context, p *bot.SendMessageParams) (*models.Message, error) {
	s.calls++
	s.cancel()
	return nil, context.Canceled
}

func TestShutdownReleasesWholeBatchWithoutChargingAttempts(t *testing.T) {
	h := newHarness(t)
	ids := []int64{h.enqueue(t, 1, "a", epoch), h.enqueue(t, 2, "b", epoch), h.enqueue(t, 3, "c", epoch)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &cancelSender{cancel: cancel}
	h.n.sender = s
	h.n.Drain(ctx)
	if s.calls != 1 {
		t.Fatal("sent during shutdown")
	}
	for _, id := range ids {
		r := h.row(t, id)
		if r.Status != model.OutboxPending || r.Attempts != 0 {
			t.Fatalf("shutdown left claim: %+v", r)
		}
	}
}
