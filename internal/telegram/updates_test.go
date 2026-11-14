package telegram

import "testing"

func TestUnseenLowerUpdateIsDispatched(t *testing.T) {
	h := newHarness(t, nil)
	first := h.message("/start")
	second := h.message("/help")
	h.tg.HandleUpdate(h.ctx, h.tg.API(), second)
	h.tg.HandleUpdate(h.ctx, h.tg.API(), first)
	if n := len(h.api.byMethod("sendMessage")); n != 2 {
		t.Fatalf("processed %d updates, want 2", n)
	}
	h.tg.HandleUpdate(h.ctx, h.tg.API(), first)
	if n := len(h.api.byMethod("sendMessage")); n != 2 {
		t.Fatal("replayed update dispatched twice")
	}
	if id := h.tg.TelegramHealth(h.ctx).LastUpdateID; id != second.ID {
		t.Fatal("health update ID moved backwards")
	}
}
