package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/config"
	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/rdb"
	"altrdb-telegram-bot/internal/store"
)

func TestStartRegistersTheChatAndShowsOnboarding(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")

	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("the chat was not registered: %v", err)
	}
	if !chat.Enabled || chat.Type != "private" {
		t.Errorf("chat = %+v", chat)
	}

	sends := h.api.byMethod("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(sends))
	}
	text := sends[0].Plain()
	for _, want := range []string{"/maintainers", "/states", "/branches"} {
		if !strings.Contains(text, want) {
			t.Errorf("onboarding does not mention %s:\n%s", want, text)
		}
	}
	// The settings keyboard comes with the onboarding message.
	buttons := sends[0].Buttons()
	if len(buttons) == 0 {
		t.Fatal("no inline keyboard on the onboarding message")
	}
	joined := strings.Join(buttons, "|")
	for _, want := range []string{"Maintainers", "States", "Branches", "Pause", "Test", "Help"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the keyboard is missing a %q button: %v", want, buttons)
		}
	}
	if sends[0].Form.Get("parse_mode") != "HTML" {
		t.Errorf("parse mode = %q, want HTML", sends[0].Form.Get("parse_mode"))
	}
}

func TestStartAfterBeingBlockedReactivatesTheChat(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	if err := h.store.DisableChat(h.ctx, testChatID, "bot was blocked by the user"); err != nil {
		t.Fatalf("DisableChat: %v", err)
	}

	h.send("/start")
	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !chat.Enabled || chat.DisabledReason != "" {
		t.Errorf("chat = %+v, want reactivated", chat)
	}
}

func TestSettingsSummarisesTheSubscription(t *testing.T) {
	h := newHarness(t, nil)
	h.subscribe(testChatID, []string{"grenka", "rider"}, []string{"DONE", "FAILED"}, []string{"sisyphus"}, epoch.Add(-time.Hour))

	h.send("/settings")
	text := h.lastText("sendMessage")
	for _, want := range []string{"enabled", "grenka", "rider", "DONE", "FAILED", "sisyphus", "every matching change"} {
		if !strings.Contains(text, want) {
			t.Errorf("settings text is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Not configured yet") {
		t.Errorf("a complete subscription must not be reported incomplete:\n%s", text)
	}
}

func TestSettingsReportsWhatIsMissing(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/settings")
	text := h.lastText("sendMessage")
	for _, want := range []string{"maintainers", "states", "branches"} {
		if !strings.Contains(text, want) {
			t.Errorf("the missing list should mention %q:\n%s", want, text)
		}
	}
}

func TestSettingsShowsAPausedChat(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/pause")
	text := h.lastText("sendMessage")
	if !strings.Contains(text, "paused") {
		t.Errorf("paused state not reported:\n%s", text)
	}

	h.send("/settings")
	if !strings.Contains(h.lastText("sendMessage"), "paused") {
		t.Error("/settings must show the paused status")
	}
}

func TestPauseAndResume(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")

	h.send("/pause")
	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.Enabled {
		t.Error("/pause left the chat enabled")
	}
	if !strings.Contains(h.lastText("sendMessage"), "paused") {
		t.Errorf("confirmation = %q", h.lastText("sendMessage"))
	}

	h.send("/resume")
	chat, _ = h.store.GetChat(h.ctx, testChatID)
	if !chat.Enabled {
		t.Error("/resume did not re-enable the chat")
	}
	if chat.DisabledReason != "" {
		t.Errorf("the reason was not cleared: %q", chat.DisabledReason)
	}
}

func TestMaintainersPanelListsCurrentEntries(t *testing.T) {
	h := newHarness(t, nil)
	h.subscribe(testChatID, []string{"grenka", "rider"}, nil, nil, epoch)

	h.send("/maintainers")
	c, _ := h.api.lastByMethod("sendMessage")
	text := c.Plain()
	if !strings.Contains(text, "2 followed") {
		t.Errorf("header = %q", text)
	}
	buttons := c.Buttons()
	if !contains(buttons, checkedBox+" grenka") || !contains(buttons, checkedBox+" rider") {
		t.Errorf("followed maintainers must appear as ☑ buttons: %v", buttons)
	}
	if !contains(buttons, "➕ Add") || !contains(buttons, "🗑 Clear all") {
		t.Errorf("the panel is missing add/clear controls: %v", buttons)
	}
	for _, data := range c.CallbackData() {
		if len(data) > maxCallbackDataLen {
			t.Errorf("callback data %q is %d bytes, over Telegram's limit", data, len(data))
		}
	}
}

func TestMaintainersPanelPaginates(t *testing.T) {
	h := newHarness(t, nil) // PageSize is 4 in the harness
	names := []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg", "hhh", "iii"}
	h.subscribe(testChatID, names, nil, nil, epoch)

	h.send("/maintainers")
	c, _ := h.api.lastByMethod("sendMessage")
	if len(c.Buttons()) < 2 {
		t.Fatalf("buttons = %v", c.Buttons())
	}
	if !contains(c.Buttons(), "Next »") {
		t.Errorf("nine entries over a page size of four must paginate: %v", c.Buttons())
	}
	if !contains(c.Buttons(), "1/3") {
		t.Errorf("the page indicator is missing: %v", c.Buttons())
	}
	if contains(c.Buttons(), "« Prev") {
		t.Error("the first page must not offer a Prev button")
	}

	// Only the first page of names is shown.
	if contains(c.Buttons(), checkedBox+" eee") {
		t.Errorf("page one leaked a later entry: %v", c.Buttons())
	}
}

func TestMaintainerAddFlow(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.send("/maintainers")

	// ➕ Add puts the chat into a persisted conversation state.
	h.press("ma:a")
	conv, err := h.store.GetConversation(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv == nil || conv.Step != model.StepAwaitingMaintainers {
		t.Fatalf("conversation state = %+v, want awaiting_maintainers", conv)
	}
	if !strings.Contains(h.lastText("sendMessage"), "nicknames") {
		t.Errorf("the prompt was not sent:\n%s", h.lastText("sendMessage"))
	}

	// Free text is consumed by the pending step.
	h.send("  Grenka , @Rider\nGLEBFM  bad!!token  ")

	list, err := h.store.ListMaintainers(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListMaintainers: %v", err)
	}
	if strings.Join(list, ",") != "glebfm,grenka,rider" {
		t.Errorf("maintainers = %v", list)
	}

	text := strings.Join(allTexts(h), "\n")
	if !strings.Contains(text, "bad!!token") {
		t.Errorf("the rejected token was not reported:\n%s", text)
	}
	if !strings.Contains(text, "Added 3") {
		t.Errorf("the confirmation is missing:\n%s", text)
	}

	conv, err = h.store.GetConversation(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv != nil {
		t.Errorf("the conversation state was not cleared: %+v", conv)
	}
}

func TestMaintainerAddDeduplicatesAndKeepsGoing(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.subscribe(testChatID, []string{"grenka"}, nil, nil, epoch)

	h.send("/maintainers grenka rider") // inline arguments
	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if strings.Join(list, ",") != "grenka,rider" {
		t.Errorf("maintainers = %v", list)
	}
	text := strings.Join(allTexts(h), "\n")
	if !strings.Contains(text, "Already followed") {
		t.Errorf("a duplicate must be reported, not silently dropped:\n%s", text)
	}
	if !strings.Contains(text, "Added 1") {
		t.Errorf("the new nickname must be confirmed:\n%s", text)
	}
}

func TestMaintainerAddRejectsEverythingInvalid(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.send("/maintainers !!! ???")

	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if len(list) != 0 {
		t.Errorf("invalid nicknames were stored: %v", list)
	}
	if !strings.Contains(strings.Join(allTexts(h), "\n"), "Rejected") {
		t.Error("the rejection was not explained")
	}
}

func TestUnknownMaintainerIsSavedWithAWarning(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus")
	h.subscribe(testChatID, nil, []string{"DONE"}, []string{"sisyphus"}, epoch)

	// A cached directory that does not contain the nickname.
	if err := h.store.ReplaceMaintainersCache(h.ctx, "sisyphus", []model.Maintainer{
		{Branch: "sisyphus", Nickname: "grenka", PackagerName: "Georgy", CountSourcePkg: 4},
		{Branch: "sisyphus", Nickname: "grenadier", PackagerName: "Someone Else", CountSourcePkg: 1},
	}); err != nil {
		t.Fatalf("ReplaceMaintainersCache: %v", err)
	}

	h.send("/maintainers grenk")

	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if len(list) != 1 || list[0] != "grenk" {
		t.Fatalf("the nickname must still be saved: %v", list)
	}
	text := strings.Join(allTexts(h), "\n")
	if !strings.Contains(text, "not found in the cached directory") {
		t.Errorf("no warning about the unknown nickname:\n%s", text)
	}
	if !strings.Contains(text, "Did you mean?") {
		t.Errorf("no suggestions were offered:\n%s", text)
	}

	// The suggestion is a ☐ button carrying an index and a hash, never the raw
	// nickname.
	suggestions := findButton(h, "ma:s:")
	if suggestions == nil {
		t.Errorf("no ☐ suggestion button was offered in %v", allButtons(h))
	} else if !strings.HasPrefix(suggestions.Text, emptyBox+" ") {
		t.Errorf("a suggestion must start with ☐: %q", suggestions.Text)
	}
}

func TestSuggestionButtonAddsTheNickname(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus")
	h.subscribe(testChatID, nil, nil, []string{"sisyphus"}, epoch)
	if err := h.store.ReplaceMaintainersCache(h.ctx, "sisyphus", []model.Maintainer{
		{Branch: "sisyphus", Nickname: "grenka", PackagerName: "Georgy", CountSourcePkg: 4},
	}); err != nil {
		t.Fatalf("ReplaceMaintainersCache: %v", err)
	}

	h.send("/maintainers gren")
	btn := findButton(h, "ma:s:")
	if btn == nil {
		t.Fatalf("no suggestion button in %v", allButtons(h))
	}

	h.api.reset()
	h.press(btn.CallbackData)

	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if !contains(list, "grenka") {
		t.Errorf("maintainers = %v, want the suggested nickname added", list)
	}
	if _, ok := h.api.lastByMethod("answerCallbackQuery"); !ok {
		t.Error("the callback was not acknowledged")
	}
}

func TestSuggestionButtonWithAStaleHashIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	// A selected branch is what makes an unconfirmed nickname worth warning
	// about, and therefore what produces suggestions.
	h.subscribe(testChatID, nil, nil, []string{"sisyphus"}, epoch)
	if err := h.store.ReplaceMaintainersCache(h.ctx, "sisyphus", []model.Maintainer{
		{Branch: "sisyphus", Nickname: "grenka"},
	}); err != nil {
		t.Fatalf("ReplaceMaintainersCache: %v", err)
	}
	h.send("/maintainers gren")

	btn := findButton(h, "ma:s:")
	if btn == nil {
		t.Fatalf("no suggestion button in %v", allButtons(h))
	}

	// Change the offer list behind the button's back.
	if err := h.store.ClearConversation(h.ctx, testChatID); err != nil {
		t.Fatalf("ClearConversation: %v", err)
	}
	h.api.reset()
	h.press(btn.CallbackData)

	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if len(list) != 1 || list[0] != "gren" {
		t.Errorf("a stale suggestion changed the list to %v", list)
	}
	ans, ok := h.api.lastByMethod("answerCallbackQuery")
	if !ok {
		t.Fatal("the stale callback was not answered")
	}
	if ans.Form.Get("show_alert") != "true" {
		t.Errorf("a stale control must raise an alert, got %q", ans.Form.Get("show_alert"))
	}
}

func TestRemoveMaintainerByButton(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.subscribe(testChatID, []string{"aaa", "bbb", "ccc"}, nil, nil, epoch)

	h.send("/maintainers")
	c, _ := h.api.lastByMethod("sendMessage")
	var data string
	for _, d := range c.CallbackData() {
		if strings.HasPrefix(d, "ma:d:") {
			data = d // the first one is index 0 = "aaa"
			break
		}
	}
	if data == "" {
		t.Fatalf("no remove button in %v", c.CallbackData())
	}

	h.api.reset()
	h.press(data)

	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if strings.Join(list, ",") != "bbb,ccc" {
		t.Errorf("maintainers = %v", list)
	}
	if _, ok := h.api.lastByMethod("editMessageText"); !ok {
		t.Error("the panel must be edited in place, not re-sent")
	}
	if _, ok := h.api.lastByMethod("answerCallbackQuery"); !ok {
		t.Error("the callback was not acknowledged")
	}
}

func TestRemoveMaintainerWithAStaleHashIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.subscribe(testChatID, []string{"aaa", "bbb"}, nil, nil, epoch)

	h.send("/maintainers")
	c, _ := h.api.lastByMethod("sendMessage")
	var data string
	for _, d := range c.CallbackData() {
		if strings.HasPrefix(d, "ma:d:0:") {
			data = d
		}
	}
	if data == "" {
		t.Fatal("no remove button")
	}

	// Somebody else (or another client) changes the list first.
	if _, err := h.store.AddMaintainers(h.ctx, testChatID, []string{"zzz"}); err != nil {
		t.Fatalf("AddMaintainers: %v", err)
	}
	h.api.reset()
	h.press(data)

	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if strings.Join(list, ",") != "aaa,bbb,zzz" {
		t.Errorf("a stale button removed something: %v", list)
	}
	ans, ok := h.api.lastByMethod("answerCallbackQuery")
	if !ok {
		t.Fatal("the stale callback was not answered")
	}
	if !strings.Contains(ans.Form.Get("text"), "Refresh") {
		t.Errorf("the alert should tell the user to refresh: %q", ans.Form.Get("text"))
	}
}

func TestClearAllMaintainersByButton(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.subscribe(testChatID, []string{"aaa", "bbb"}, nil, nil, epoch)
	h.send("/maintainers")
	h.api.reset()

	h.press("ma:c")
	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if len(list) != 0 {
		t.Errorf("maintainers = %v, want none", list)
	}
	if _, ok := h.api.lastByMethod("answerCallbackQuery"); !ok {
		t.Error("the callback was not acknowledged")
	}
}

func TestCancelAbortsThePrompt(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	if err := h.store.SetConversation(h.ctx, testChatID, model.StepAwaitingMaintainers, ""); err != nil {
		t.Fatalf("SetConversation: %v", err)
	}

	h.send("/cancel")
	conv, err := h.store.GetConversation(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv != nil {
		t.Errorf("conversation state = %+v, want cleared", conv)
	}
	if !strings.Contains(h.lastText("sendMessage"), "Cancelled") {
		t.Errorf("confirmation = %q", h.lastText("sendMessage"))
	}

	h.send("/cancel")
	if !strings.Contains(h.lastText("sendMessage"), "Nothing to cancel") {
		t.Errorf("a second cancel = %q", h.lastText("sendMessage"))
	}
}

func TestACommandDuringAPromptEndsThePrompt(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	if err := h.store.SetConversation(h.ctx, testChatID, model.StepAwaitingMaintainers, ""); err != nil {
		t.Fatalf("SetConversation: %v", err)
	}
	h.api.reset()

	h.send("/help")
	if !strings.Contains(h.lastText("sendMessage"), "Commands") {
		t.Error("the command was swallowed by the pending prompt")
	}
	conv, _ := h.store.GetConversation(h.ctx, testChatID)
	if conv != nil {
		t.Errorf("the prompt was not cleared: %+v", conv)
	}
	list, _ := h.store.ListMaintainers(h.ctx, testChatID)
	if len(list) != 0 {
		t.Errorf("the command text was stored as a nickname: %v", list)
	}
}

func TestEmptyPromptIsAnsweredWithAnExample(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	if err := h.store.SetConversation(h.ctx, testChatID, model.StepAwaitingMaintainers, ""); err != nil {
		t.Fatalf("SetConversation: %v", err)
	}
	h.api.reset()

	h.send("   ")
	if !strings.Contains(h.lastText("sendMessage"), "Example") {
		t.Errorf("reply = %q", h.lastText("sendMessage"))
	}
	conv, _ := h.store.GetConversation(h.ctx, testChatID)
	if conv == nil {
		t.Error("an empty answer must keep the prompt open")
	}
}

func TestStatesAndBranchesCommandsOpenPanels(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus", "p11", "p10", "p9", "p8")

	h.send("/states")
	c, _ := h.api.lastByMethod("sendMessage")
	if !strings.Contains(c.Plain(), "Task states") {
		t.Errorf("states panel = %q", c.Plain())
	}
	if len(c.Buttons()) == 0 {
		t.Error("the states panel has no buttons")
	}
	toggles := 0
	for _, b := range c.buttonsFull() {
		if !strings.HasPrefix(b.CallbackData, "st:") {
			continue
		}
		toggles++
		if !strings.HasPrefix(b.Text, checkedBox+" ") && !strings.HasPrefix(b.Text, emptyBox+" ") {
			t.Errorf("a state toggle does not start with ☑ or ☐: %q", b.Text)
		}
		if len(b.CallbackData) > maxCallbackDataLen {
			t.Errorf("callback data %q exceeds the Telegram limit", b.CallbackData)
		}
	}
	if toggles == 0 {
		t.Error("the states panel has no toggle buttons")
	}

	h.api.reset()
	h.send("/branches")
	c, _ = h.api.lastByMethod("sendMessage")
	if !strings.Contains(c.Plain(), "Repository branches") {
		t.Errorf("branches panel = %q", c.Plain())
	}
	if !contains(c.Buttons(), emptyBox+" sisyphus") {
		t.Errorf("the branch list is missing from %v", c.Buttons())
	}
}

func TestStatesCommandWithArgumentsReplacesTheSelection(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.send("/states DONE, failed")

	got, err := h.store.ListChatStates(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListChatStates: %v", err)
	}
	if strings.Join(got, ",") != "FAILED,DONE" {
		t.Errorf("states = %v, want lifecycle order FAILED then DONE", got)
	}

	h.send("/states NOSUCHSTATE DONE")
	text := strings.Join(allTexts(h), "\n")
	if !strings.Contains(text, "Not in the catalogue yet") {
		t.Errorf("an unknown state must be reported:\n%s", text)
	}
	got, _ = h.store.ListChatStates(h.ctx, testChatID)
	if strings.Join(got, ",") != "DONE" {
		t.Errorf("states = %v, want only the accepted one", got)
	}
}

func TestBranchesCommandWithArguments(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus", "p11")

	h.send("/branches sisyphus nope p11")
	subs, err := h.store.ListChatBranches(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListChatBranches: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("branches = %+v", subs)
	}
	for _, s := range subs {
		if s.ActiveWatermark.IsZero() {
			t.Errorf("branch %s has no watermark", s.Branch)
		}
	}
	text := strings.Join(allTexts(h), "\n")
	if !strings.Contains(text, "not replayed") {
		t.Errorf("the watermark behaviour must be explained:\n%s", text)
	}
	if !strings.Contains(text, "Unknown to RDB") {
		t.Errorf("the rejected branch must be reported:\n%s", text)
	}
}

func TestRecentListsMatchingTasksWithoutTouchingTheWatermark(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.subscribe(testChatID, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-24*time.Hour))

	feed := []model.Task{
		{TaskID: 1, TaskRepo: "sisyphus", State: "DONE", Owner: "grenka", Changed: epoch.Add(-2 * time.Hour), Packages: []string{"postfix 3.8.21-alt1"}},
		{TaskID: 2, TaskRepo: "sisyphus", State: "DONE", Owner: "grenka", Changed: epoch.Add(-1 * time.Hour)},
		{TaskID: 3, TaskRepo: "sisyphus", State: "FAILED", Owner: "grenka", Changed: epoch},
	}
	if _, err := h.store.ApplyTasks(h.ctx, feed, true, model.NewTerminalStates([]string{"DONE"})); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}

	before, err := h.store.ListChatBranches(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListChatBranches: %v", err)
	}

	h.api.reset()
	h.send("/recent")
	text := h.lastText("sendMessage")
	if !strings.Contains(text, "#2") || !strings.Contains(text, "#1") {
		t.Errorf("matching tasks missing from:\n%s", text)
	}
	if strings.Contains(text, "#3") {
		t.Errorf("a non-matching state was listed:\n%s", text)
	}
	raw, _ := h.api.lastByMethod("sendMessage")
	if !strings.Contains(raw.Text(), "https://git.altlinux.org/tasks/2/") {
		t.Errorf("the task link is missing from:\n%s", raw.Text())
	}

	after, err := h.store.ListChatBranches(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListChatBranches: %v", err)
	}
	if !before[0].ActiveWatermark.Equal(after[0].ActiveWatermark) {
		t.Errorf("the watermark moved: %s -> %s", before[0].ActiveWatermark, after[0].ActiveWatermark)
	}
}

func TestRecentExplainsAnIncompleteSubscription(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.send("/recent")
	text := h.lastText("sendMessage")
	if !strings.Contains(text, "Nothing to show yet") {
		t.Errorf("reply = %q", text)
	}
}

func TestRecentRejectsABadCount(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.subscribe(testChatID, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch)
	h.send("/recent abc")
	if !strings.Contains(strings.Join(allTexts(h), "\n"), "usage") {
		t.Error("a non-numeric count must be explained")
	}
}

func TestTestCommandSendsASample(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.subscribe(testChatID, []string{"grenka"}, []string{"DONE"}, []string{"p11"}, epoch)
	h.api.reset()

	h.send("/test")
	menu, ok := h.api.lastByMethod("sendMessage")
	if !ok || len(menu.Buttons()) != 5 || !strings.Contains(menu.Plain(), "Test notifications") {
		t.Fatal("expected four sample choices and a settings button")
	}
	h.api.reset()
	h.press("tt:DONE")
	sends := h.api.byMethod("sendMessage")
	if len(sends) == 0 {
		t.Fatal("no sample notification was sent")
	}
	c := sends[0]
	if !strings.Contains(c.Plain(), "Task #433219") {
		t.Errorf("sample text = %q", c.Plain())
	}
	if !strings.Contains(c.Plain(), "@grenka") {
		t.Errorf("the sample must use a saved maintainer: %q", c.Plain())
	}
	if !strings.Contains(c.Plain(), "Branch: p11") {
		t.Errorf("the sample must use a saved branch: %q", c.Plain())
	}
	if !strings.Contains(strings.Join(c.Buttons(), "|"), "Open task #433219") {
		t.Errorf("the sample has no task button: %v", c.Buttons())
	}

	// The sample must not go through the outbox.
	var n int
	if err := h.store.DB().QueryRowContext(h.ctx, `SELECT COUNT(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("/test queued %d outbox rows", n)
	}
}

func TestTestCommandWarnsAboutAnIncompleteSubscription(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()
	h.send("/test")
	h.press("tt:DONE")
	if !strings.Contains(strings.Join(allTexts(h), "\n"), "incomplete") {
		t.Error("an incomplete subscription must be pointed out")
	}
}

func TestHelpAndUnknownCommands(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/help")
	text := h.lastText("sendMessage")
	for _, want := range []string{"/start", "/settings", "/maintainers", "/states", "/branches", "/recent", "/pause", "/resume", "/test", "/help"} {
		if !strings.Contains(text, want) {
			t.Errorf("help does not document %s", want)
		}
	}

	h.api.reset()
	h.send("/frobnicate")
	if !strings.Contains(h.lastText("sendMessage"), "Unknown command") {
		t.Errorf("reply = %q", h.lastText("sendMessage"))
	}
}

func TestCommandAddressedToAnotherBotIsIgnored(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/help@" + testBotName)
	if !strings.Contains(h.lastText("sendMessage"), "Commands") {
		t.Error("a command addressed to us must be handled")
	}

	h.api.reset()
	h.send("/help@somebodyelsesbot")
	if len(h.api.byMethod("sendMessage")) != 0 {
		t.Error("a command addressed to another bot must be ignored")
	}
}

func TestPlainTextWithoutAPendingPromptGetsAHint(t *testing.T) {
	h := newHarness(t, nil)
	h.send("hello there")
	text := h.lastText("sendMessage")
	if !strings.Contains(text, "/help") {
		t.Errorf("reply = %q", text)
	}
}

func TestNonTextMessagesAreIgnored(t *testing.T) {
	h := newHarness(t, nil)
	u := h.message("")
	u.Message.Text = ""
	u.Message.Photo = []models.PhotoSize{{FileID: "x"}}
	h.pressUpdate(u)
	if len(h.api.byMethod("sendMessage")) != 0 {
		t.Error("a sticker or photo must not trigger a reply")
	}
}

func TestGroupChatsAreRejectedByDefault(t *testing.T) {
	h := newHarness(t, nil)
	u := h.messageFrom(999, 5555, "/start")
	u.Message.Chat.Type = models.ChatTypeGroup
	u.Message.Chat.Title = "ALT Linux"
	h.pressUpdate(u)

	if len(h.api.byMethod("sendMessage")) != 1 {
		t.Fatalf("calls = %+v", h.api.byMethod("sendMessage"))
	}
	if !strings.Contains(h.lastText("sendMessage"), "private chats only") {
		t.Errorf("reply = %q", h.lastText("sendMessage"))
	}
	if _, err := h.store.GetChat(h.ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a rejected group must not be registered: %v", err)
	}
}

func TestGroupChatsCanBeAllowedByConfiguration(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.AllowGroups = true })

	u := h.messageFrom(999, 5555, "/start")
	u.Message.Chat.Type = models.ChatTypeSupergroup
	u.Message.Chat.Title = "ALT Linux"
	u.Message.Chat.FirstName = ""
	h.pressUpdate(u)

	chat, err := h.store.GetChat(h.ctx, 999)
	if err != nil {
		t.Fatalf("a group must be registered when ALLOW_TELEGRAM_GROUPS is on: %v", err)
	}
	if chat.Type != "supergroup" {
		t.Errorf("chat type = %q", chat.Type)
	}
	if len(h.api.byMethod("sendMessage")) == 0 {
		t.Error("the group was not answered")
	}
}

func TestRefusalIsNotRepeatedWithinTheCooldown(t *testing.T) {
	h := newHarness(t, nil)
	u := h.messageFrom(999, 5555, "/start")
	u.Message.Chat.Type = models.ChatTypeSupergroup
	h.pressUpdate(u)
	h.pressUpdate(u)
	h.pressUpdate(u)
	if got := len(h.api.byMethod("sendMessage")); got != 1 {
		t.Errorf("sendMessage calls = %d, want the refusal sent once", got)
	}
}

func TestAllowlistRejectsStrangers(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.AllowedTelegramUsers = []int64{777} })
	h.send("/start") // from testUserID, which is not on the list
	if !strings.Contains(strings.Join(allTexts(h), "\n"), "allowlist") {
		t.Error("a stranger must be told why nothing happens")
	}
	if _, err := h.store.GetChat(h.ctx, testChatID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger must not be registered: %v", err)
	}

	h.tg.HandleUpdate(h.ctx, h.tg.API(), h.messageFrom(777, 777, "/start"))
	if _, err := h.store.GetChat(h.ctx, 777); err != nil {
		t.Errorf("an allowlisted user was refused: %v", err)
	}
}

func TestMyChatMemberDisablesAndReEnables(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")

	h.tg.HandleUpdate(h.ctx, h.tg.API(), &models.Update{
		ID: h.nextUpdateID(),
		MyChatMember: &models.ChatMemberUpdated{
			Chat:          models.Chat{ID: testChatID, Type: models.ChatTypePrivate},
			From:          models.User{ID: testUserID},
			OldChatMember: models.ChatMember{Type: models.ChatMemberTypeMember, Member: &models.ChatMemberMember{}},
			NewChatMember: models.ChatMember{Type: models.ChatMemberTypeBanned, Banned: &models.ChatMemberBanned{}},
		},
	})
	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.Enabled {
		t.Error("being blocked must disable the chat")
	}
	if !strings.Contains(chat.DisabledReason, "kicked") {
		t.Errorf("reason = %q", chat.DisabledReason)
	}

	h.tg.HandleUpdate(h.ctx, h.tg.API(), &models.Update{
		ID: h.nextUpdateID(),
		MyChatMember: &models.ChatMemberUpdated{
			Chat:          models.Chat{ID: testChatID, Type: models.ChatTypePrivate},
			From:          models.User{ID: testUserID},
			OldChatMember: models.ChatMember{Type: models.ChatMemberTypeBanned, Banned: &models.ChatMemberBanned{}},
			NewChatMember: models.ChatMember{Type: models.ChatMemberTypeMember, Member: &models.ChatMemberMember{}},
		},
	})
	chat, _ = h.store.GetChat(h.ctx, testChatID)
	if !chat.Enabled {
		t.Error("being added back must re-enable the chat")
	}
}

func TestOffsetIsPersistedAndStaleUpdatesAreIgnored(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")

	off, err := h.store.TelegramOffset(h.ctx)
	if err != nil {
		t.Fatalf("TelegramOffset: %v", err)
	}
	if off != 1 {
		t.Errorf("offset = %d, want 1", off)
	}

	h.api.reset()
	// The same update replayed, as would happen after a restart with a stale
	// in-memory offset.
	stale := h.message("/help")
	stale.ID = 1
	h.pressUpdate(stale)
	if len(h.api.byMethod("sendMessage")) != 0 {
		t.Error("a replayed update must not be processed twice")
	}
}

func TestTaskCommandUsesTheDetailEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	h.tg.tasks = &stubTaskLookup{
		info: &rdb.TaskInfoResponse{
			TaskID: 433219, TaskRepo: "sisyphus", TaskState: "EPERM", TaskOwner: "grenka",
			TaskTry: 2, TaskIter: 1, TaskChanged: "2026-09-16T13:56:00+00:00",
			TaskStage: "task-check-acl",
			Approval:  []rdb.Approval{{Type: "approve", Nickname: "rider"}},
			Subtasks:  []rdb.Subtask{{SubtaskSrpmName: "postfix", SubtaskSrpmEvr: "3.8.21-alt1"}},
		},
	}
	h.send("/start")
	h.api.reset()

	h.send("/task 433219")
	text := h.lastText("sendMessage")
	for _, want := range []string{"433219", "EPERM", "approve: rider", "postfix 3.8.21-alt1", "Open in Girar"} {
		if !strings.Contains(text, want) {
			t.Errorf("task detail is missing %q:\n%s", want, text)
		}
	}
}

func TestTaskCommandRejectsABadID(t *testing.T) {
	h := newHarness(t, nil)
	h.tg.tasks = &stubTaskLookup{}
	h.send("/start")
	h.api.reset()

	h.send("/task abc")
	if !strings.Contains(strings.Join(allTexts(h), "\n"), "usage") {
		t.Error("a non-numeric id must be explained")
	}
	h.send("/task")
	if !strings.Contains(strings.Join(allTexts(h), "\n"), "usage") {
		t.Error("a missing id must be explained")
	}
}

func TestTaskCommandHandlesAnUnknownTask(t *testing.T) {
	h := newHarness(t, nil)
	h.tg.tasks = &stubTaskLookup{err: rdb.ErrNotFound}
	h.send("/start")
	h.api.reset()

	h.send("/task 1")
	if !strings.Contains(h.lastText("sendMessage"), "does not know task") {
		t.Errorf("reply = %q", h.lastText("sendMessage"))
	}
}

func TestTaskCommandReportsALookupFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.tg.tasks = &stubTaskLookup{err: errors.New("rdb is down")}
	h.send("/start")
	h.api.reset()

	h.send("/task 1")
	if !strings.Contains(strings.Join(allTexts(h), "\n"), "rdb is down") {
		t.Error("the failure must be surfaced to the user")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func allTexts(h *harness) []string {
	var out []string
	for _, c := range h.api.recorded() {
		if t := c.Plain(); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// allButtons flattens every inline button of every recorded call.
func allButtons(h *harness) []models.InlineKeyboardButton {
	var out []models.InlineKeyboardButton
	for _, c := range h.api.recorded() {
		out = append(out, c.buttonsFull()...)
	}
	return out
}

// findButton returns the first button whose callback data starts with prefix.
func findButton(h *harness, prefix string) *models.InlineKeyboardButton {
	for _, b := range allButtons(h) {
		if strings.HasPrefix(b.CallbackData, prefix) {
			found := b
			return &found
		}
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// stubTaskLookup replaces the RDB client for /task.
type stubTaskLookup struct {
	info *rdb.TaskInfoResponse
	err  error
}

func (s *stubTaskLookup) TaskInfo(_ context.Context, _ int64) (*rdb.TaskInfoResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.info == nil {
		return nil, rdb.ErrNotFound
	}
	return s.info, nil
}
