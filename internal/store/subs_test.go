package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/model"
)

func TestRegisterChatReactivatesADisabledChat(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	chat, err := st.RegisterChat(ctx, 11, "private", "first", "First")
	if err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if !chat.Enabled || chat.ID != 11 || chat.NotifyMode != model.NotifyAll {
		t.Errorf("chat = %+v", chat)
	}

	if err := st.DisableChat(ctx, 11, "bot was blocked by the user"); err != nil {
		t.Fatalf("DisableChat: %v", err)
	}
	chat, err = st.GetChat(ctx, 11)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.Enabled || chat.DisabledReason == "" {
		t.Errorf("chat should be disabled with a reason: %+v", chat)
	}

	// An interactive /start is the way back in.
	if _, err := st.RegisterChat(ctx, 11, "private", "first", "First Renamed"); err != nil {
		t.Fatalf("re-RegisterChat: %v", err)
	}
	chat, err = st.GetChat(ctx, 11)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !chat.Enabled || chat.DisabledReason != "" {
		t.Errorf("chat was not reactivated: %+v", chat)
	}
	if chat.FirstName != "First Renamed" {
		t.Errorf("first name = %q", chat.FirstName)
	}
}

func TestUpdateChatIdentityKeepsTheSubscriptionState(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := st.RegisterChat(ctx, 12, "private", "u", "U"); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if err := st.SetNotifyMode(ctx, 12, model.NotifyTerminal); err != nil {
		t.Fatalf("SetNotifyMode: %v", err)
	}
	if err := st.SetChatEnabled(ctx, 12, false, "paused by the user"); err != nil {
		t.Fatalf("SetChatEnabled: %v", err)
	}
	if err := st.UpdateChatIdentity(ctx, 12, "private", "newname", "New Name"); err != nil {
		t.Fatalf("UpdateChatIdentity: %v", err)
	}

	chat, err := st.GetChat(ctx, 12)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.Enabled {
		t.Error("a passive identity update must not re-enable a paused chat")
	}
	if chat.NotifyMode != model.NotifyTerminal {
		t.Errorf("notify mode = %q, want terminal", chat.NotifyMode)
	}
	if chat.Username != "newname" {
		t.Errorf("username = %q", chat.Username)
	}
}

func TestGetChatMissing(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.GetChat(context.Background(), 999); err != ErrNotFound {
		t.Errorf("GetChat for an unknown id = %v, want ErrNotFound", err)
	}
}

func TestSetNotifyModeRejectsUnknownValues(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 13, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if err := st.SetNotifyMode(ctx, 13, model.NotifyMode("sometimes")); err == nil {
		t.Error("an invented notify mode must be rejected")
	}
}

func TestAddMaintainersDeduplicatesAndReportsNewOnes(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 20, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}

	added, err := st.AddMaintainers(ctx, 20, []string{"grenka", "rider"})
	if err != nil {
		t.Fatalf("AddMaintainers: %v", err)
	}
	if strings.Join(added, ",") != "grenka,rider" {
		t.Errorf("added = %v", added)
	}

	added, err = st.AddMaintainers(ctx, 20, []string{"grenka", "glebfm"})
	if err != nil {
		t.Fatalf("AddMaintainers again: %v", err)
	}
	if strings.Join(added, ",") != "glebfm" {
		t.Errorf("second add = %v, want only the new nickname", added)
	}

	list, err := st.ListMaintainers(ctx, 20)
	if err != nil {
		t.Fatalf("ListMaintainers: %v", err)
	}
	if strings.Join(list, ",") != "glebfm,grenka,rider" {
		t.Errorf("list = %v, want sorted and deduplicated", list)
	}
}

func TestAddMaintainersEnforcesThePerChatCap(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 21, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}

	full := make([]string, model.MaxNicknamesPerChat)
	for i := range full {
		full[i] = fmt.Sprintf("user%03d", i)
	}
	if _, err := st.AddMaintainers(ctx, 21, full); err != nil {
		t.Fatalf("filling to the cap: %v", err)
	}
	_, err := st.AddMaintainers(ctx, 21, []string{"one-more"})
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("error = %v, want the cap to be reported", err)
	}
}

func TestAddMaintainersEmptyIsANoOp(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 22, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	added, err := st.AddMaintainers(ctx, 22, nil)
	if err != nil || added != nil {
		t.Errorf("added = %v, err = %v", added, err)
	}
}

func TestRemoveMaintainers(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 23, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if _, err := st.AddMaintainers(ctx, 23, []string{"aaa", "bbb", "ccc"}); err != nil {
		t.Fatalf("AddMaintainers: %v", err)
	}

	removed, err := st.RemoveMaintainer(ctx, 23, "bbb")
	if err != nil || !removed {
		t.Errorf("removed = %t, err = %v", removed, err)
	}
	removed, err = st.RemoveMaintainer(ctx, 23, "zzz")
	if err != nil || removed {
		t.Errorf("removing an absent nickname = %t, %v", removed, err)
	}

	// Index 1 of the sorted list [aaa, ccc] is ccc.
	name, err := st.RemoveMaintainerByIndex(ctx, 23, 1)
	if err != nil {
		t.Fatalf("RemoveMaintainerByIndex: %v", err)
	}
	if name != "ccc" {
		t.Errorf("removed by index = %q, want ccc", name)
	}
	if _, err := st.RemoveMaintainerByIndex(ctx, 23, 5); err == nil {
		t.Error("an out-of-range index must be rejected")
	}

	n, err := st.ClearMaintainers(ctx, 23)
	if err != nil {
		t.Fatalf("ClearMaintainers: %v", err)
	}
	if n != 1 {
		t.Errorf("cleared = %d, want 1", n)
	}
}

func TestToggleChatState(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 30, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}

	on, err := st.ToggleChatState(ctx, 30, "DONE")
	if err != nil || !on {
		t.Fatalf("first toggle = %t, %v; want on", on, err)
	}
	on, err = st.ToggleChatState(ctx, 30, "DONE")
	if err != nil || on {
		t.Fatalf("second toggle = %t, %v; want off", on, err)
	}
	if _, err := st.ToggleChatState(ctx, 30, ""); err == nil {
		t.Error("an empty state must be rejected")
	}

	for _, s := range []string{"FAILED", "DONE", "NEW"} {
		if _, err := st.ToggleChatState(ctx, 30, s); err != nil {
			t.Fatalf("toggling %s: %v", s, err)
		}
	}
	got, err := st.ListChatStates(ctx, 30)
	if err != nil {
		t.Fatalf("ListChatStates: %v", err)
	}
	// Lifecycle order (NEW=10, FAILED=80, DONE=90), not insertion order.
	if strings.Join(got, ",") != "NEW,FAILED,DONE" {
		t.Errorf("states = %v, want lifecycle order", got)
	}
}

func TestSetAllChatStatesReplacesTheSelection(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 31, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if _, err := st.ToggleChatState(ctx, 31, "DONE"); err != nil {
		t.Fatalf("ToggleChatState: %v", err)
	}
	if err := st.SetAllChatStates(ctx, 31, []string{"FAILED", ""}); err != nil {
		t.Fatalf("SetAllChatStates: %v", err)
	}
	got, _ := st.ListChatStates(ctx, 31)
	if strings.Join(got, ",") != "FAILED" {
		t.Errorf("states = %v, want the replacement only", got)
	}
	if err := st.SetAllChatStates(ctx, 31, nil); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	got, _ = st.ListChatStates(ctx, 31)
	if len(got) != 0 {
		t.Errorf("states = %v, want none", got)
	}
}

func TestToggleChatBranchStampsAFreshWatermark(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 40, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}

	on, wm, err := st.ToggleChatBranch(ctx, 40, "sisyphus")
	if err != nil || !on {
		t.Fatalf("toggle = %t, %v", on, err)
	}
	if !wm.Equal(epoch) {
		t.Errorf("watermark = %s, want the activation time %s", wm, epoch)
	}

	// Unchecking and re-checking must re-stamp: the user is starting over.
	fake.Advance(2 * time.Hour)
	if on, _, err := st.ToggleChatBranch(ctx, 40, "sisyphus"); err != nil || on {
		t.Fatalf("uncheck = %t, %v", on, err)
	}
	on, wm, err = st.ToggleChatBranch(ctx, 40, "sisyphus")
	if err != nil || !on {
		t.Fatalf("recheck = %t, %v", on, err)
	}
	if !wm.Equal(epoch.Add(2 * time.Hour)) {
		t.Errorf("watermark after re-check = %s, want %s", wm, epoch.Add(2*time.Hour))
	}
}

func TestAddChatBranchKeepsAnExistingWatermark(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 41, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	first, err := st.AddChatBranch(ctx, 41, "p11")
	if err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}
	fake.Advance(time.Hour)

	again, err := st.AddChatBranch(ctx, 41, "p11")
	if err != nil {
		t.Fatalf("AddChatBranch again: %v", err)
	}
	if !again.Equal(first) {
		t.Errorf("watermark moved from %s to %s; a repeated add must not reset it", first, again)
	}
}

func TestSetAllChatBranchesKeepsExistingWatermarks(t *testing.T) {
	st, fake := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 42, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	first, err := st.AddChatBranch(ctx, 42, "sisyphus")
	if err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}
	fake.Advance(3 * time.Hour)

	if err := st.SetAllChatBranches(ctx, 42, []string{"sisyphus", "p11"}); err != nil {
		t.Fatalf("SetAllChatBranches: %v", err)
	}
	subs, err := st.ListChatBranches(ctx, 42)
	if err != nil {
		t.Fatalf("ListChatBranches: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("branches = %+v", subs)
	}
	for _, s := range subs {
		switch s.Branch {
		case "sisyphus":
			if !s.ActiveWatermark.Equal(first) {
				t.Errorf("select-all moved an existing watermark: %s != %s", s.ActiveWatermark, first)
			}
		case "p11":
			if !s.ActiveWatermark.Equal(epoch.Add(3 * time.Hour)) {
				t.Errorf("a newly added branch = %s, want now", s.ActiveWatermark)
			}
		}
	}

	n, err := st.ClearChatBranches(ctx, 42)
	if err != nil {
		t.Fatalf("ClearChatBranches: %v", err)
	}
	if n != 2 {
		t.Errorf("cleared = %d, want 2", n)
	}
}

func TestActiveBranchesOnlyCountsEnabledChats(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	for _, id := range []int64{50, 51} {
		if _, err := st.RegisterChat(ctx, id, "private", "", ""); err != nil {
			t.Fatalf("RegisterChat %d: %v", id, err)
		}
	}
	if _, err := st.AddChatBranch(ctx, 50, "sisyphus"); err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}
	if _, err := st.AddChatBranch(ctx, 51, "sisyphus"); err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}
	if _, err := st.AddChatBranch(ctx, 51, "p11"); err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}

	got, err := st.ActiveBranches(ctx)
	if err != nil {
		t.Fatalf("ActiveBranches: %v", err)
	}
	if strings.Join(got, ",") != "p11,sisyphus" {
		t.Errorf("active branches = %v", got)
	}

	// Pausing the only p11 subscriber must drop p11 from the polling set:
	// that is what makes "one fetch per active branch" true.
	if err := st.SetChatEnabled(ctx, 51, false, ""); err != nil {
		t.Fatalf("SetChatEnabled: %v", err)
	}
	got, err = st.ActiveBranches(ctx)
	if err != nil {
		t.Fatalf("ActiveBranches: %v", err)
	}
	if strings.Join(got, ",") != "sisyphus" {
		t.Errorf("active branches after pausing = %v, want only sisyphus", got)
	}
}

func TestCountChats(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	total, enabled, complete, err := st.CountChats(ctx)
	if err != nil {
		t.Fatalf("CountChats: %v", err)
	}
	if total != 0 || enabled != 0 || complete != 0 {
		t.Errorf("empty database = %d/%d/%d", total, enabled, complete)
	}

	if _, err := st.RegisterChat(ctx, 60, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if _, err := st.RegisterChat(ctx, 61, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	if _, err := st.AddMaintainers(ctx, 60, []string{"grenka"}); err != nil {
		t.Fatalf("AddMaintainers: %v", err)
	}
	if _, err := st.ToggleChatState(ctx, 60, "DONE"); err != nil {
		t.Fatalf("ToggleChatState: %v", err)
	}
	if _, err := st.AddChatBranch(ctx, 60, "sisyphus"); err != nil {
		t.Fatalf("AddChatBranch: %v", err)
	}
	if err := st.SetChatEnabled(ctx, 61, false, ""); err != nil {
		t.Fatalf("SetChatEnabled: %v", err)
	}

	total, enabled, complete, err = st.CountChats(ctx)
	if err != nil {
		t.Fatalf("CountChats: %v", err)
	}
	if total != 2 || enabled != 1 || complete != 1 {
		t.Errorf("counts = %d/%d/%d, want 2/1/1", total, enabled, complete)
	}
}

func TestConversationState(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterChat(ctx, 70, "private", "", ""); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}

	conv, err := st.GetConversation(ctx, 70)
	if err != nil || conv != nil {
		t.Fatalf("conv = %+v, err = %v; want nil", conv, err)
	}
	if err := st.SetConversation(ctx, 70, model.StepAwaitingMaintainers, `{"page":2}`); err != nil {
		t.Fatalf("SetConversation: %v", err)
	}
	conv, err = st.GetConversation(ctx, 70)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv == nil || conv.Step != model.StepAwaitingMaintainers || conv.Payload != `{"page":2}` {
		t.Errorf("conv = %+v", conv)
	}
	if conv.UpdatedAt.IsZero() {
		t.Error("updated_at must be stamped")
	}

	if err := st.SetConversation(ctx, 70, model.StepSuggestions, `["a"]`); err != nil {
		t.Fatalf("SetConversation overwrite: %v", err)
	}
	conv, _ = st.GetConversation(ctx, 70)
	if conv.Step != model.StepSuggestions {
		t.Errorf("step = %q after overwrite", conv.Step)
	}

	if err := st.ClearConversation(ctx, 70); err != nil {
		t.Fatalf("ClearConversation: %v", err)
	}
	conv, err = st.GetConversation(ctx, 70)
	if err != nil || conv != nil {
		t.Errorf("conv after clear = %+v, %v", conv, err)
	}
}

// ---------------------------------------------------------------------------
// catalogues
// ---------------------------------------------------------------------------

func TestReplaceBranchesExcludesRetiredBranches(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	for range 2 {
		if err := st.ReplaceBranches(ctx, []string{"sisyphus", "c9f1", "c8", "c7.1", "c7", "c8.1"}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := st.BranchNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "sisyphus,c8.1" {
		t.Errorf("branches after repeated refresh = %v", names)
	}
}

func TestReplaceBranchesKeepsUnknownOnes(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if err := st.ReplaceBranches(ctx, []string{"sisyphus", "p11", "p10"}); err != nil {
		t.Fatalf("ReplaceBranches: %v", err)
	}
	// p10 disappears from the API but a chat is still subscribed to it.
	if err := st.ReplaceBranches(ctx, []string{"sisyphus", "p11"}); err != nil {
		t.Fatalf("ReplaceBranches again: %v", err)
	}
	names, err := st.BranchNames(ctx)
	if err != nil {
		t.Fatalf("BranchNames: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("branches = %v; a dropped branch must be kept", names)
	}
	if names[0] != "sisyphus" || names[1] != "p11" {
		t.Errorf("order not taken from the API response: %v", names)
	}
	if err := st.ReplaceBranches(ctx, nil); err != nil {
		t.Errorf("an empty list must be a no-op, got %v", err)
	}
}

func TestBranchAndStateIDRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if err := st.ReplaceBranches(ctx, []string{"sisyphus"}); err != nil {
		t.Fatalf("ReplaceBranches: %v", err)
	}
	id, err := st.BranchID(ctx, "sisyphus")
	if err != nil {
		t.Fatalf("BranchID: %v", err)
	}
	name, err := st.BranchName(ctx, id)
	if err != nil {
		t.Fatalf("BranchName: %v", err)
	}
	if name != "sisyphus" {
		t.Errorf("round trip = %q", name)
	}
	if _, err := st.BranchID(ctx, "nope"); err != ErrNotFound {
		t.Errorf("BranchID for an unknown branch = %v, want ErrNotFound", err)
	}
	if _, err := st.BranchName(ctx, 99999); err != ErrNotFound {
		t.Errorf("BranchName for an unknown id = %v, want ErrNotFound", err)
	}

	sid, err := st.StateID(ctx, "DONE")
	if err != nil {
		t.Fatalf("StateID: %v", err)
	}
	if got, err := st.StateName(ctx, sid); err != nil || got != "DONE" {
		t.Errorf("StateName = %q, %v", got, err)
	}
	if _, err := st.StateID(ctx, "NOPE"); err != ErrNotFound {
		t.Errorf("StateID for an unknown state = %v, want ErrNotFound", err)
	}
}

func TestObserveStatesLearnsNewOnesAndKeepsOrder(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if err := st.ObserveStates(ctx, []string{"COMMITTING", "", "DONE", "  "}); err != nil {
		t.Fatalf("ObserveStates: %v", err)
	}
	states, err := st.ListStates(ctx)
	if err != nil {
		t.Fatalf("ListStates: %v", err)
	}
	names := make([]string, 0, len(states))
	known := map[string]bool{}
	for _, s := range states {
		names = append(names, s.Name)
		known[s.Name] = s.Known
	}
	if names[len(names)-1] != "COMMITTING" {
		t.Errorf("a learned state must sort last: %v", names)
	}
	if known["COMMITTING"] {
		t.Error("COMMITTING was learned from the feed and must not be marked known")
	}
	if !known["DONE"] {
		t.Error("DONE is a seeded state and must stay known")
	}
	if known["DONE"] && states[0].Name != "NEW" {
		t.Errorf("lifecycle order broken, first = %q", states[0].Name)
	}

	// A later release that seeds COMMITTING upgrades the row.
	model.KnownStates = append(append([]string{}, model.KnownStates...), "COMMITTING")
	t.Cleanup(func() { model.KnownStates = model.KnownStates[:len(model.KnownStates)-1] })
	if err := st.SeedKnownStates(ctx); err != nil {
		t.Fatalf("SeedKnownStates: %v", err)
	}
	states, _ = st.ListStates(ctx)
	for _, s := range states {
		if s.Name == "COMMITTING" && !s.Known {
			t.Error("re-seeding must promote a learned state to known")
		}
	}
}

func TestMaintainerCacheAndSuggestions(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	dir := []model.Maintainer{
		{Nickname: "grenka", PackagerName: "Georgy Gagarin", CountSourcePkg: 42},
		{Nickname: "glebfm", PackagerName: "Gleb Fomchenko", CountSourcePkg: 300},
		{Nickname: "rider", PackagerName: "Anton Farygin", CountSourcePkg: 900},
		{Nickname: "", PackagerName: "no nickname"},
	}
	if err := st.ReplaceMaintainersCache(ctx, "sisyphus", dir); err != nil {
		t.Fatalf("ReplaceMaintainersCache: %v", err)
	}
	if err := st.ReplaceMaintainersCache(ctx, "p11", dir[:1]); err != nil {
		t.Fatalf("ReplaceMaintainersCache p11: %v", err)
	}

	fetched, ok, err := st.MaintainerCacheAge(ctx, "sisyphus")
	if err != nil || !ok {
		t.Fatalf("cache age = %s, %t, %v", fetched, ok, err)
	}
	if !fetched.Equal(epoch) {
		t.Errorf("fetched_at = %s, want %s", fetched, epoch)
	}
	if _, ok, _ := st.MaintainerCacheAge(ctx, "unknown-branch"); ok {
		t.Error("an uncached branch must report no age")
	}

	found, name, branches, err := st.FindMaintainer(ctx, "grenka", []string{"sisyphus"})
	if err != nil {
		t.Fatalf("FindMaintainer: %v", err)
	}
	if !found || name != "Georgy Gagarin" || strings.Join(branches, ",") != "sisyphus" {
		t.Errorf("found = %t, name = %q, branches = %v", found, name, branches)
	}

	found, _, branches, err = st.FindMaintainer(ctx, "grenka", nil)
	if err != nil {
		t.Fatalf("FindMaintainer without a filter: %v", err)
	}
	if !found || len(branches) != 2 {
		t.Errorf("unfiltered lookup = %t %v, want both branches", found, branches)
	}

	if found, _, _, err := st.FindMaintainer(ctx, "nobody", nil); err != nil || found {
		t.Errorf("an unknown nickname = %t, %v", found, err)
	}

	got, err := st.SuggestMaintainers(ctx, "g", 5)
	if err != nil {
		t.Fatalf("SuggestMaintainers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("suggestions = %+v, want the two g* nicknames", got)
	}
	// Ordered by package count descending, so the busiest packager comes first.
	if got[0].Nickname != "glebfm" {
		t.Errorf("first suggestion = %q, want the one with the most packages", got[0].Nickname)
	}

	// LIKE wildcards in user input must not turn into a full-table match.
	got, err = st.SuggestMaintainers(ctx, "%", 10)
	if err != nil {
		t.Fatalf("SuggestMaintainers with a wildcard: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a bare %% matched %d nicknames, want none", len(got))
	}
}

func TestReplaceMaintainersCacheRejectsAnEmptyBranch(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.ReplaceMaintainersCache(context.Background(), "", nil); err == nil {
		t.Error("an empty branch must be rejected so the cache cannot be wiped by accident")
	}
}
