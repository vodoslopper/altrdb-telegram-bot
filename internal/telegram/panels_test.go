package telegram

import (
	"altrdb-telegram-bot/internal/model"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBranchArgumentsReplaceSelectionAndPreserveWatermark(t *testing.T) {
	h := newHarness(t, nil)
	h.ensureBranchCatalogue("sisyphus", "p11")
	h.subscribe(testChatID, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus", "p11"}, epoch)
	h.clock.Advance(time.Hour)
	h.send("/branches p11")
	branches, err := h.store.ListChatBranches(h.ctx, testChatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].Branch != "p11" || !branches[0].ActiveWatermark.Equal(epoch) {
		t.Fatalf("branches: %+v", branches)
	}
	h.send("/branches unknown-branch")
	branches, err = h.store.ListChatBranches(h.ctx, testChatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].Branch != "p11" {
		t.Fatal("invalid arguments cleared subscription")
	}
}

func TestMaintainerRefreshFetchesSelectedDirectories(t *testing.T) {
	h := newHarness(t, nil)
	h.subscribe(testChatID, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus", "p11"}, epoch)
	dir := &stubDirectory{}
	h.tg.refresh = dir
	h.press("rf:ma")
	got := strings.Join(dir.maintainers, ",")
	if len(dir.maintainers) != 2 || !strings.Contains(got, "sisyphus") || !strings.Contains(got, "p11") {
		t.Fatalf("refreshed: %v", dir.maintainers)
	}
}

func TestStartShowsConfiguredTerminalStates(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	if err := h.store.SetNotifyMode(h.ctx, testChatID, model.NotifyTerminal); err != nil {
		t.Fatal(err)
	}
	h.api.reset()
	h.send("/start")
	text := h.lastText("sendMessage")
	if strings.Contains(text, "terminal states only ()") || !strings.Contains(text, "DONE") || !strings.Contains(text, "FAILED") {
		t.Fatalf("onboarding: %s", text)
	}
}

func TestPaginatedStatesStayWithinButtonBudget(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	states := make([]string, 200)
	for i := range states {
		states[i] = fmt.Sprintf("EXTRA_%03d", i)
	}
	if err := h.store.ObserveStates(h.ctx, states); err != nil {
		t.Fatal(err)
	}
	for page := range 3 {
		p, err := h.tg.statesPanel(h.ctx, testChatID, page)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, row := range p.markup.InlineKeyboard {
			total += len(row)
		}
		if total > 100 {
			t.Fatalf("page %d has %d buttons", page, total)
		}
	}
}
