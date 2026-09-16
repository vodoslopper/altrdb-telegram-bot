package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/model"
)

// stubDirectory records the catalogue refreshes the UI asks for.
type stubDirectory struct {
	branches     int
	maintainers  []string
	branchesErr  error
	maintRespErr error
}

func (s *stubDirectory) RefreshBranchesNow(_ context.Context) error {
	s.branches++
	return s.branchesErr
}

func (s *stubDirectory) RefreshMaintainers(_ context.Context, branch string) error {
	s.maintainers = append(s.maintainers, branch)
	return s.maintRespErr
}

func TestToggleStateEditsTheSameMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	// Page 2 is a number left over from a panel sent before the chooser became
	// a single page; it is clamped, and the toggle still lands on DONE.
	id := h.stateID("DONE")
	h.press(fmt.Sprintf("st:%d:2", id))

	states, err := h.store.ListChatStates(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListChatStates: %v", err)
	}
	if !contains(states, "DONE") {
		t.Errorf("states = %v, want DONE checked", states)
	}

	edits := h.api.byMethod("editMessageText")
	if len(edits) != 1 {
		t.Fatalf("editMessageText calls = %d, want the same message edited", len(edits))
	}
	if edits[0].Form.Get("message_id") != fmt.Sprint(testMessageID) {
		t.Errorf("edited message = %q, want %d", edits[0].Form.Get("message_id"), testMessageID)
	}
	if edits[0].ChatID() != testChatID {
		t.Errorf("edited chat = %d", edits[0].ChatID())
	}
	if !contains(edits[0].Buttons(), checkedBox+" DONE") {
		t.Errorf("the box was not ticked: %v", edits[0].Buttons())
	}
	if len(h.api.byMethod("sendMessage")) != 0 {
		t.Error("toggling must not send a new message")
	}

	// Toggling again unticks it.
	h.press(fmt.Sprintf("st:%d:2", id))
	states, _ = h.store.ListChatStates(h.ctx, testChatID)
	if contains(states, "DONE") {
		t.Errorf("states = %v, want DONE unchecked", states)
	}
	if !contains(h.api.byMethod("editMessageText")[1].Buttons(), emptyBox+" DONE") {
		t.Error("the box was not unticked")
	}
}

func TestToggleUnknownStateIsRefusedGracefully(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	h.press("st:999999:0")
	if len(h.api.byMethod("editMessageText")) != 0 {
		t.Error("nothing should be edited for an unknown state id")
	}
	ans, ok := h.api.lastByMethod("answerCallbackQuery")
	if !ok {
		t.Fatal("the callback was not answered")
	}
	if ans.Form.Get("show_alert") != "true" {
		t.Errorf("an unknown id must raise an alert, got %q", ans.Form.Get("text"))
	}
}

// TestStatesPanelIsOneTwoColumnPage pins the layout of the state chooser: every
// known state on a single screen, two buttons to a row. Nine seeded states over
// a harness PageSize of four would be three pages of one column if the states
// screen paginated like the others.
func TestStatesPanelIsOneTwoColumnPage(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/states")

	c, ok := h.api.lastByMethod("sendMessage")
	if !ok {
		t.Fatal("no states panel was sent")
	}
	all, err := h.store.StateNames(h.ctx)
	if err != nil {
		t.Fatalf("StateNames: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("the seeded catalogue has %d states, too few to prove the layout", len(all))
	}

	offered := map[string]bool{}
	rows, twoWide := 0, 0
	for _, row := range c.keyboard().InlineKeyboard {
		toggles := 0
		for _, b := range row {
			if !strings.HasPrefix(b.CallbackData, "st:") {
				continue
			}
			toggles++
			offered[stateLabel(b.Text)] = true
			// The screen is one page, so every toggle must say so. A stale
			// page number in an old button is clamped, never honoured.
			if !strings.HasSuffix(b.CallbackData, ":0") {
				t.Errorf("callback data %q is not on page 0", b.CallbackData)
			}
		}
		if toggles == 0 {
			continue
		}
		rows++
		if toggles > statesColumns {
			t.Errorf("a row holds %d state toggles, want at most %d", toggles, statesColumns)
		}
		if toggles == statesColumns {
			twoWide++
		}
	}

	if len(offered) != len(all) {
		t.Errorf("the panel offers %d of %d states: %v", len(offered), len(all), offered)
	}
	if want := (len(all) + statesColumns - 1) / statesColumns; rows != want {
		t.Errorf("%d toggle rows for %d states, want %d", rows, len(all), want)
	}
	if len(all)%statesColumns == 0 && twoWide != rows {
		t.Errorf("only %d of %d rows are two columns wide", twoWide, rows)
	}
	for _, b := range c.Buttons() {
		if b == "« Prev" || b == "Next »" || strings.Contains(b, "/") {
			t.Errorf("the states screen is a single page but offers %q", b)
		}
	}
	if !contains(c.Buttons(), "✔ Select all") || !contains(c.Buttons(), "✖ Clear all") {
		t.Errorf("the bulk controls are missing: %v", c.Buttons())
	}
}

// TestStatePageCallbacksClampToOnePage keeps old buttons working: a panel sent
// before the chooser became a single page still carries pg:st:<n> and st:<id>:<n>,
// and pressing one must re-render the current screen rather than an empty one.
func TestStatePageCallbacksClampToOnePage(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/states")
	all, _ := h.store.StateNames(h.ctx)

	for _, data := range []string{"pg:st:0", "pg:st:1", "pg:st:99", "pg:st:-5"} {
		h.api.reset()
		h.press(data)
		last, ok := h.api.lastByMethod("editMessageText")
		if !ok {
			t.Fatalf("%s did not re-render the panel", data)
		}
		if _, ok := h.api.lastByMethod("answerCallbackQuery"); !ok {
			t.Errorf("%s was never answered", data)
		}
		if !contains(last.Buttons(), emptyBox+" "+all[0]) {
			t.Errorf("%s lost the state list: %v", data, last.Buttons())
		}
		if contains(last.Buttons(), "« Prev") || contains(last.Buttons(), "Next »") {
			t.Errorf("%s produced a paged panel: %v", data, last.Buttons())
		}
	}
}

// stateLabel strips the checkbox glyph and the ⁇ learned marker from a toggle.
func stateLabel(text string) string {
	text = strings.TrimPrefix(text, checkedBox+" ")
	text = strings.TrimPrefix(text, emptyBox+" ")
	return strings.TrimSpace(strings.TrimSuffix(text, "⁇"))
}

func TestSelectAllAndClearAllStates(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	h.press("sa:st:0")
	states, _ := h.store.ListChatStates(h.ctx, testChatID)
	all, _ := h.store.StateNames(h.ctx)
	if len(states) != len(all) {
		t.Errorf("select all gave %d of %d states", len(states), len(all))
	}
	last, _ := h.api.lastByMethod("editMessageText")
	unticked := 0
	for _, b := range last.Buttons() {
		if strings.HasPrefix(b, emptyBox+" ") {
			unticked++
		}
	}
	if unticked != 0 {
		t.Errorf("%d boxes are still empty after select all", unticked)
	}

	h.press("ca:st:0")
	states, _ = h.store.ListChatStates(h.ctx, testChatID)
	if len(states) != 0 {
		t.Errorf("clear all left %v", states)
	}
}

func TestToggleBranchStampsTheWatermark(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus", "p11")
	h.api.reset()

	id, err := h.store.BranchID(h.ctx, "sisyphus")
	if err != nil {
		t.Fatalf("BranchID: %v", err)
	}
	h.press(fmt.Sprintf("br:%d:0", id))

	subs, err := h.store.ListChatBranches(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("ListChatBranches: %v", err)
	}
	if len(subs) != 1 || subs[0].Branch != "sisyphus" {
		t.Fatalf("branches = %+v", subs)
	}
	if !subs[0].ActiveWatermark.Equal(epoch) {
		t.Errorf("watermark = %s, want the activation time %s", subs[0].ActiveWatermark, epoch)
	}
	if !strings.Contains(h.lastEditedPlain(), "not replayed") {
		t.Error("the panel must explain that history is not replayed")
	}

	// Unchecking removes it; re-checking stamps a new watermark.
	h.press(fmt.Sprintf("br:%d:0", id))
	subs, _ = h.store.ListChatBranches(h.ctx, testChatID)
	if len(subs) != 0 {
		t.Errorf("branches = %+v, want none", subs)
	}
	h.clock.Advance(3 * time.Hour)
	h.press(fmt.Sprintf("br:%d:0", id))
	subs, _ = h.store.ListChatBranches(h.ctx, testChatID)
	if len(subs) != 1 || !subs[0].ActiveWatermark.Equal(epoch.Add(3*time.Hour)) {
		t.Errorf("watermark after re-checking = %+v", subs)
	}
}

func TestBranchSelectAllAndClearAll(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus", "p11", "p10")
	h.api.reset()

	h.press("sa:br:0")
	subs, _ := h.store.ListChatBranches(h.ctx, testChatID)
	if len(subs) != 3 {
		t.Errorf("select all gave %d branches, want 3", len(subs))
	}

	h.press("ca:br:0")
	subs, _ = h.store.ListChatBranches(h.ctx, testChatID)
	if len(subs) != 0 {
		t.Errorf("clear all left %+v", subs)
	}
}

func TestRefreshBranchesForcesACatalogueFetch(t *testing.T) {
	h := newHarness(t, nil)
	dir := &stubDirectory{}
	h.tg.refresh = dir
	h.send("/start")
	h.api.reset()

	h.press("rf:br")
	if dir.branches != 1 {
		t.Errorf("catalogue refreshes = %d, want one forced fetch", dir.branches)
	}
	if len(h.api.byMethod("answerCallbackQuery")) == 0 {
		t.Error("the callback must be answered even while the network call runs")
	}

	// A refresh of another view must not hit the network.
	h.press("rf:st")
	if dir.branches != 1 {
		t.Errorf("refreshing the states view fetched the catalogue %d times", dir.branches)
	}
}

func TestRefreshFailureStillRendersTheCachedList(t *testing.T) {
	h := newHarness(t, nil)
	h.tg.refresh = &stubDirectory{branchesErr: errors.New("rdb is down")}
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus", "p11")
	h.api.reset()

	h.press("rf:br")
	buttons := h.lastEdited().Buttons()
	if !contains(buttons, emptyBox+" sisyphus") || !contains(buttons, emptyBox+" p11") {
		t.Errorf("the cached list disappeared when RDB failed: %v", buttons)
	}
	if !strings.Contains(h.lastEditedPlain(), "Selected: 0 of 2") {
		t.Errorf("the cached catalogue was not counted:\n%s", h.lastEditedPlain())
	}
}

func TestCheckingABranchWarmsTheMaintainerCache(t *testing.T) {
	h := newHarness(t, nil)
	dir := &stubDirectory{}
	h.tg.refresh = dir
	h.send("/start")
	h.ensureBranchCatalogue("p11")
	id, err := h.store.BranchID(h.ctx, "p11")
	if err != nil {
		t.Fatalf("BranchID: %v", err)
	}
	h.api.reset()

	h.press(fmt.Sprintf("br:%d:0", id))
	if len(dir.maintainers) != 1 || dir.maintainers[0] != "p11" {
		t.Errorf("maintainer warm-up = %v, want one fetch for p11", dir.maintainers)
	}

	// Once the cache exists it is not fetched again.
	if err := h.store.ReplaceMaintainersCache(h.ctx, "p11", []model.Maintainer{
		{Branch: "p11", Nickname: "grenka"},
	}); err != nil {
		t.Fatalf("ReplaceMaintainersCache: %v", err)
	}
	h.press(fmt.Sprintf("br:%d:0", id)) // off
	h.press(fmt.Sprintf("br:%d:0", id)) // on again
	if len(dir.maintainers) != 1 {
		t.Errorf("the directory was fetched %d times, want it reused from the cache", len(dir.maintainers))
	}
}

func TestDoneReturnsToSettings(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	h.press("dn:st")
	last, ok := h.api.lastByMethod("editMessageText")
	if !ok {
		t.Fatal("Done did not edit the panel")
	}
	if !strings.Contains(last.Plain(), "ALT RDB build-task notifications") {
		t.Errorf("Done from a sub-panel should show the hub:\n%s", last.Plain())
	}
	if !contains(last.Buttons(), "👤 Maintainers") {
		t.Errorf("the hub keyboard is missing: %v", last.Buttons())
	}

	h.api.reset()
	h.press("dn:se")
	last, _ = h.api.lastByMethod("editMessageText")
	if len(last.Buttons()) != 0 {
		t.Errorf("Done on the hub should drop the keyboard, got %v", last.Buttons())
	}
}

func TestNotifyModeToggle(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	h.press("nm")
	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.NotifyMode != model.NotifyTerminal {
		t.Errorf("mode = %q, want terminal", chat.NotifyMode)
	}
	if !strings.Contains(h.lastEditedPlain(), "terminal") {
		t.Errorf("the new mode is not shown:\n%s", h.lastEditedPlain())
	}

	h.press("nm")
	chat, _ = h.store.GetChat(h.ctx, testChatID)
	if chat.NotifyMode != model.NotifyAll {
		t.Errorf("mode = %q, want it back to all", chat.NotifyMode)
	}
}

func TestEnableToggleFromThePanel(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	h.press("en:0")
	chat, _ := h.store.GetChat(h.ctx, testChatID)
	if chat.Enabled {
		t.Error("en:0 must pause the chat")
	}
	if !contains(h.lastEdited().Buttons(), "▶ Resume") {
		t.Errorf("the button should now offer Resume: %v", h.lastEdited().Buttons())
	}

	h.press("en:1")
	chat, _ = h.store.GetChat(h.ctx, testChatID)
	if !chat.Enabled {
		t.Error("en:1 must resume the chat")
	}
	if chat.DisabledReason != "" {
		t.Errorf("the reason was not cleared: %q", chat.DisabledReason)
	}
}

func TestPanelButtonsThatSendTheirOwnMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.subscribe(testChatID, []string{"grenka"}, []string{"DONE"}, []string{"sisyphus"}, epoch.Add(-time.Hour))
	if _, err := h.store.ApplyTasks(h.ctx, []model.Task{
		{TaskID: 5, TaskRepo: "sisyphus", State: "DONE", Owner: "grenka", Changed: epoch.Add(-time.Minute)},
	}, true, model.NewTerminalStates([]string{"DONE"})); err != nil {
		t.Fatalf("ApplyTasks: %v", err)
	}

	h.send("/settings")
	h.api.reset()

	h.press("rc")
	if !strings.Contains(h.lastText("sendMessage"), "Recent matching tasks") {
		t.Errorf("/recent button reply = %q", h.lastText("sendMessage"))
	}
	ans, ok := h.api.lastByMethod("answerCallbackQuery")
	if !ok || ans.Form.Get("text") == "" {
		t.Error("a button that opens a new message must say so in the toast")
	}

	h.api.reset()
	h.press("hp")
	if !strings.Contains(h.lastText("sendMessage"), "Commands") {
		t.Error("the help button did not send help")
	}

	h.api.reset()
	h.press("tt")
	if !strings.Contains(h.lastText("sendMessage"), "Task #433219") {
		t.Error("the test button did not send a sample")
	}
}

// ---------------------------------------------------------------------------
// callback safety
// ---------------------------------------------------------------------------

func TestCallbackOnAForeignMessageIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	u := h.callback("nm")
	u.CallbackQuery.Message.Message.From = &models.User{ID: 31337, Username: "someoneelse", IsBot: true}
	h.pressUpdate(u)

	if len(h.api.byMethod("editMessageText")) != 0 {
		t.Error("a button on somebody else's message must not edit anything")
	}
	ans, ok := h.api.lastByMethod("answerCallbackQuery")
	if !ok {
		t.Fatal("the callback was not answered")
	}
	if ans.Form.Get("show_alert") != "true" {
		t.Errorf("answer = %q", ans.Form.Get("text"))
	}
}

func TestCallbackFromAnotherChatIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	// Somebody presses a button on a panel that belongs to another private
	// chat. In a private chat the chat id is the owner's user id, so a
	// different pressing user proves the panel is not theirs.
	u := h.callbackIn(777, testUserID, testMessageID, "nm")
	h.pressUpdate(u)

	if len(h.api.byMethod("editMessageText")) != 0 {
		t.Error("a panel from another private chat must not be edited")
	}
	ans, ok := h.api.lastByMethod("answerCallbackQuery")
	if !ok {
		t.Fatal("the callback was not answered")
	}
	if ans.Form.Get("show_alert") != "true" {
		t.Errorf("answer = %q", ans.Form.Get("text"))
	}
	// The victim's subscription must be untouched.
	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.NotifyMode != model.NotifyAll {
		t.Errorf("another chat's mode was changed to %q", chat.NotifyMode)
	}
}

func TestInaccessibleMessageIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	u := &models.Update{
		ID: h.nextUpdateID(),
		CallbackQuery: &models.CallbackQuery{
			ID:   "old",
			From: models.User{ID: testUserID},
			Message: models.MaybeInaccessibleMessage{
				Type: models.MaybeInaccessibleMessageTypeInaccessibleMessage,
				InaccessibleMessage: &models.InaccessibleMessage{
					Chat: models.Chat{ID: testChatID, Type: models.ChatTypePrivate}, MessageID: 1,
				},
			},
			Data: "nm",
		},
	}
	h.pressUpdate(u)

	if len(h.api.byMethod("editMessageText")) != 0 {
		t.Error("an inaccessible message cannot be edited")
	}
	ans, ok := h.api.lastByMethod("answerCallbackQuery")
	if !ok {
		t.Fatal("the callback was not answered")
	}
	if !strings.Contains(ans.Form.Get("text"), "expired") {
		t.Errorf("answer = %q, want an explanation about the expired menu", ans.Form.Get("text"))
	}
}

func TestMalformedCallbackDataIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")

	for _, data := range []string{
		"", "zz", "st:notanumber:0", "st:1", "pg:xx:0", "dn",
		strings.Repeat("x", maxCallbackDataLen+1),
	} {
		h.api.reset()
		h.press(data)
		if len(h.api.byMethod("editMessageText")) != 0 {
			t.Errorf("callback data %q edited a message", data)
		}
		if _, ok := h.api.lastByMethod("answerCallbackQuery"); !ok {
			t.Errorf("callback data %q was not answered", data)
		}
	}
}

func TestEveryCallbackPathAnswersTheQuery(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus", "p11")
	stateID := h.stateID("DONE")
	branchID, err := h.store.BranchID(h.ctx, "sisyphus")
	if err != nil {
		t.Fatalf("BranchID: %v", err)
	}

	actions := []string{
		fmt.Sprintf("st:%d:0", stateID),
		"sa:st:0", "ca:st:0", "pg:st:1", "rf:st", "dn:st",
		fmt.Sprintf("br:%d:0", branchID),
		"sa:br:0", "ca:br:0", "rf:br", "dn:br",
		"ma:a", "ma:c", "nm", "en:0", "en:1", "tt", "rc", "hp",
		"st:999999:0", "nonsense", "",
	}
	for _, data := range actions {
		h.api.reset()
		h.press(data)
		if _, ok := h.api.lastByMethod("answerCallbackQuery"); !ok {
			t.Errorf("callback %q was never answered; the client spinner would hang", data)
		}
	}
}

func TestCallbackDataStaysUnderTheTelegramLimit(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.ensureBranchCatalogue("sisyphus", "p11", "p10", "p9", "p8", "c10f1")
	h.subscribe(testChatID, []string{"grenka"}, nil, nil, epoch)

	// Walk every panel the bot can render and check every button it produces.
	for _, text := range []string{"/states", "/branches", "/maintainers", "/settings"} {
		h.api.reset()
		h.send(text)
		for _, c := range h.api.recorded() {
			for _, b := range c.buttonsFull() {
				if len(b.CallbackData) > maxCallbackDataLen {
					t.Errorf("%s produced callback data %q of %d bytes",
						text, b.CallbackData, len(b.CallbackData))
				}
				if b.CallbackData == "" && b.URL == "" {
					t.Errorf("%s produced a button with no action: %q", text, b.Text)
				}
			}
		}
	}
}

func TestNoRawValuesInCallbackData(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	// A nickname with characters that must never appear in callback data.
	h.subscribe(testChatID, []string{"a+b.c_d"}, nil, nil, epoch)
	h.ensureBranchCatalogue("c10f1")

	h.api.reset()
	h.send("/maintainers")
	for _, c := range h.api.recorded() {
		for _, d := range c.CallbackData() {
			if strings.Contains(d, "a+b.c_d") {
				t.Errorf("a raw nickname leaked into callback data: %q", d)
			}
			if strings.Contains(d, "c10f1") {
				t.Errorf("a raw branch name leaked into callback data: %q", d)
			}
		}
	}

	h.api.reset()
	h.send("/states")
	for _, c := range h.api.recorded() {
		for _, d := range c.CallbackData() {
			if strings.Contains(d, "DONE") {
				t.Errorf("a raw state name leaked into callback data: %q", d)
			}
		}
	}
}

func TestUnchangedPanelEditIsNotAnError(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	// Telegram answers an edit that changes nothing with a 400; the library
	// turns that into ErrorBadRequest, which must not be treated as a failure.
	h.api.failWith("editMessageText", 400, 1, "Bad Request: message is not modified")
	// The page number is stale by design: the chooser is one page now.
	id := h.stateID("DONE")
	h.press(fmt.Sprintf("st:%d:2", id))

	states, _ := h.store.ListChatStates(h.ctx, testChatID)
	if !contains(states, "DONE") {
		t.Errorf("the toggle was not applied: %v", states)
	}
	if _, ok := h.api.lastByMethod("answerCallbackQuery"); !ok {
		t.Error("the callback must still be answered")
	}
}

func TestPermanentTelegramErrorDisablesTheChat(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.api.reset()

	h.api.failWith("sendMessage", 403, 0, "Forbidden: bot was blocked by the user")
	h.send("/help")

	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.Enabled {
		t.Error("a 403 on an interactive send must disable the chat")
	}
	if !strings.Contains(chat.DisabledReason, "blocked") {
		t.Errorf("reason = %q", chat.DisabledReason)
	}
}

// lastEditedPlain is the plain text of the most recent edit.
func (h *harness) lastEditedPlain() string {
	return h.lastEdited().Plain()
}

// lastEdited returns the most recent editMessageText call.
func (h *harness) lastEdited() call {
	h.t.Helper()
	c, ok := h.api.lastByMethod("editMessageText")
	if !ok {
		h.t.Fatal("no editMessageText call was made")
	}
	return c
}
