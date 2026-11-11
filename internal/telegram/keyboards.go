package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/model"
)

// Callback data grammar.
//
// Telegram caps callback_data at 64 bytes, so nothing free-form ever goes in
// there. Every value is a database surrogate id, a small integer, or a short
// hash. List positions carry a hash of the list they came from, so a stale
// button is detected instead of silently acting on the wrong row.
//
//	st:<stateID>:<page>          toggle a task state (page is 0: one screen)
//	br:<branchID>:<page>         toggle a branch
//	sa:<view>:<page>             select all in a view
//	ca:<view>:<page>             clear all in a view
//	pg:<view>:<page>             change page
//	rf:<view>                    re-render; branches also refetch the catalogue
//	dn:<view>                    close the panel
//	ma:a                         start adding maintainers
//	ma:c                         clear all maintainers
//	ma:d:<index>:<hash>          remove a maintainer (its ☑ button)
//	ma:s:<index>:<hash>          accept a suggestion (its ☐ button)
//	nm                           cycle the notification mode
//	en:<0|1>                     pause / resume
//	tt  rc  hp                   test / recent / help
const (
	viewStates      = "st"
	viewBranches    = "br"
	viewMaintainers = "ma"
	viewSettings    = "se"

	maxCallbackDataLen = 64
)

// checkedBox and emptyBox are the checkbox glyphs the UI spec requires every
// toggle button label to start with.
const (
	checkedBox = "☑"
	emptyBox   = "☐"
)

// The states screen is laid out two buttons to a row and fits on one page.
// State names are short and the lifecycle is bounded, so paging through them
// hid exactly the states a user wants to see side by side. Telegram caps an
// inline keyboard at 100 buttons, four of which are reserved here for
// Select all / Clear all / Refresh / Done, so 96 toggles — 48 rows of two — is
// the most one screen can carry. Reaching that would take dozens of states the
// feed invented after this binary was built; if it ever happens the panel
// paginates rather than having the whole edit rejected.
const (
	statesColumns   = 2
	statesPerPage   = 96
	branchesColumns = 2
	branchesPerPage = 96
)

// panel is one editable screen: its text and its keyboard.
type panel struct {
	text   string
	markup models.InlineKeyboardMarkup
}

// testPanel offers synthetic notifications without changing subscriptions.
func testPanel() *panel {
	p := &panel{text: "<b>Test notifications</b>\n\nChoose a task state to send a sample notification. Samples use your saved maintainer and branch. You can try several without leaving this menu."}
	var buttons []models.InlineKeyboardButton
	for _, state := range []string{"DONE", "FAILED", "BUILDING"} {
		buttons = append(buttons, models.InlineKeyboardButton{Text: state, CallbackData: "tt:" + state})
	}
	buttons = append(buttons, models.InlineKeyboardButton{Text: "TESTED", CallbackData: "tt:TESTED:testonly"})
	p.markup.InlineKeyboard = buttonRows(buttons, 2)
	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard, []models.InlineKeyboardButton{{Text: "‹ Settings", CallbackData: "dn:tt"}})
	return p
}

// listHash fingerprints an ordered list so a callback can prove the screen the
// user tapped is still the screen the bot is looking at.
func listHash(items []string) string {
	sum := sha256.Sum256([]byte(strings.Join(items, "\x00")))
	return hex.EncodeToString(sum[:4])
}

// clampPage keeps a page number inside the real range.
func clampPage(page, pages int) int {
	if pages <= 1 {
		return 0
	}
	if page < 0 {
		return 0
	}
	if page >= pages {
		return pages - 1
	}
	return page
}

// pageCount is how many pages count items need. An empty list still has one
// page, so the navigation row stays coherent.
func pageCount(count, pageSize int) int {
	if pageSize <= 0 || count == 0 {
		return 1
	}
	n := count / pageSize
	if count%pageSize != 0 {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n
}

// buttonRows lays buttons out cols to a keyboard row. Telegram sizes the
// buttons of a row to its width, so two short labels sit side by side where
// one would leave half the row empty.
func buttonRows(buttons []models.InlineKeyboardButton, cols int) [][]models.InlineKeyboardButton {
	if cols < 1 {
		cols = 1
	}
	rows := make([][]models.InlineKeyboardButton, 0, (len(buttons)+cols-1)/cols)
	for start := 0; start < len(buttons); start += cols {
		end := min(start+cols, len(buttons))
		rows = append(rows, buttons[start:end:end])
	}
	return rows
}

// navRow builds « Prev | 2/3 | Next » followed by the Refresh/Done row.
func navRow(view string, page, pages int) [][]models.InlineKeyboardButton {
	var rows [][]models.InlineKeyboardButton
	if pages > 1 {
		row := make([]models.InlineKeyboardButton, 0, 3)
		if page > 0 {
			row = append(row, models.InlineKeyboardButton{
				Text: "« Prev", CallbackData: fmt.Sprintf("pg:%s:%d", view, page-1)})
		}
		row = append(row, models.InlineKeyboardButton{
			Text: fmt.Sprintf("%d/%d", page+1, pages), CallbackData: fmt.Sprintf("pg:%s:%d", view, page)})
		if page < pages-1 {
			row = append(row, models.InlineKeyboardButton{
				Text: "Next »", CallbackData: fmt.Sprintf("pg:%s:%d", view, page+1)})
		}
		rows = append(rows, row)
	}
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "⟳ Refresh", CallbackData: "rf:" + view},
		{Text: "✅ Done", CallbackData: "dn:" + view},
	})
	return rows
}

// ---------------------------------------------------------------------------
// states
// ---------------------------------------------------------------------------

// statesPanel renders the task state checklist. States come from the catalogue
// table, which the poller keeps up to date with whatever the feed reports, so a
// state ALT introduces after this binary was built still appears here.
//
// Unlike the other checklists this one is a single screen of two-column rows:
// the state list is short enough to show whole, and seeing it whole is the
// point. bt.pageSize deliberately does not apply here.
func (bt *Bot) statesPanel(ctx context.Context, chatID int64, page int) (*panel, error) {
	all, err := bt.store.ListStates(ctx)
	if err != nil {
		return nil, err
	}
	selected, err := bt.store.ListChatStates(ctx, chatID)
	if err != nil {
		return nil, err
	}
	sel := toSet(selected)

	pages := pageCount(len(all), statesPerPage)
	page = clampPage(page, pages)
	start := page * statesPerPage
	end := min(start+statesPerPage, len(all))

	p := &panel{}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Task states</b>\n\nSelected: %d of %d\n", len(selected), len(all))
	if len(all) == 0 {
		b.WriteString("\nNo states known yet. They appear after the first successful poll of the task feed.")
	}
	toggles := make([]models.InlineKeyboardButton, 0, end-start)
	for _, st := range all[start:end] {
		box := emptyBox
		if _, ok := sel[st.Name]; ok {
			box = checkedBox
		}
		label := box + " " + st.Name
		if !st.Known {
			// Learned from a live response rather than seeded.
			label += " ⁇"
		}
		toggles = append(toggles, models.InlineKeyboardButton{
			Text: label, CallbackData: fmt.Sprintf("st:%d:%d", st.ID, page)})
	}
	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard, buttonRows(toggles, statesColumns)...)

	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard,
		[]models.InlineKeyboardButton{
			{Text: "✔ Select all", CallbackData: fmt.Sprintf("sa:%s:%d", viewStates, page)},
			{Text: "✖ Clear all", CallbackData: fmt.Sprintf("ca:%s:%d", viewStates, page)},
		})
	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard, navRow(viewStates, page, pages)...)

	b.WriteString("\nStates are learned from the feed; anything ALT starts using later shows up here marked ⁇.")
	p.text = b.String()
	return p, nil
}

// ---------------------------------------------------------------------------
// branches
// ---------------------------------------------------------------------------

// branchesPanel shows the cached RDB catalogue in two columns on one screen.
// Only catalogues exceeding Telegram's button limit need a fallback page.
func (bt *Bot) branchesPanel(ctx context.Context, chatID int64, page int) (*panel, error) {
	all, err := bt.store.ListBranches(ctx)
	if err != nil {
		return nil, err
	}
	subs, err := bt.store.ListChatBranches(ctx, chatID)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]model.BranchSubscription, len(subs))
	for _, s := range subs {
		selected[s.Branch] = s
	}

	pages := pageCount(len(all), branchesPerPage)
	page = clampPage(page, pages)
	start := page * branchesPerPage
	end := min(start+branchesPerPage, len(all))

	p := &panel{}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Repository branches</b>\n\nSelected: %d of %d\n", len(subs), len(all))
	if len(all) == 0 {
		b.WriteString("\nThe branch list could not be loaded from RDB yet.\n" +
			"Tap ⟳ Refresh to try again. The last known list is kept when RDB is unreachable.")
	}
	toggles := make([]models.InlineKeyboardButton, 0, end-start)
	for _, br := range all[start:end] {
		box := emptyBox
		if _, ok := selected[br.Name]; ok {
			box = checkedBox
		}
		toggles = append(toggles, models.InlineKeyboardButton{
			Text: box + " " + br.Name, CallbackData: fmt.Sprintf("br:%d:%d", br.ID, page)})
	}
	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard, buttonRows(toggles, branchesColumns)...)

	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard,
		[]models.InlineKeyboardButton{
			{Text: "✔ Select all", CallbackData: fmt.Sprintf("sa:%s:%d", viewBranches, page)},
			{Text: "✖ Clear all", CallbackData: fmt.Sprintf("ca:%s:%d", viewBranches, page)},
		})
	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard, navRow(viewBranches, page, pages)...)

	b.WriteString("\nTicking a branch starts from now: tasks already sitting in the feed are not replayed.")
	p.text = b.String()
	return p, nil
}

// ---------------------------------------------------------------------------
// maintainers
// ---------------------------------------------------------------------------

// maintainersPanel renders the followed-maintainers checklist. Each followed
// nickname is a ☑ button that toggles it off, mirroring the other panels.
func (bt *Bot) maintainersPanel(ctx context.Context, chatID int64, page int) (*panel, error) {
	list, err := bt.store.ListMaintainers(ctx, chatID)
	if err != nil {
		return nil, err
	}

	pages := pageCount(len(list), bt.pageSize)
	page = clampPage(page, pages)
	start := page * bt.pageSize
	end := min(start+bt.pageSize, len(list))
	hash := listHash(list)

	p := &panel{}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Maintainers</b> · %d followed\n\n", len(list))
	if len(list) == 0 {
		b.WriteString("Nobody yet. Tap <b>➕ Add</b>, then send one or more nicknames " +
			"separated by spaces or commas.\n\nExample: <code>grenka rider @glebfm</code>")
	} else {
		b.WriteString("Tap a name to stop following it.\n")
		for i, nick := range list[start:end] {
			p.markup.InlineKeyboard = append(p.markup.InlineKeyboard,
				[]models.InlineKeyboardButton{{
					Text:         checkedBox + " " + nick,
					CallbackData: fmt.Sprintf("ma:d:%d:%s", start+i, hash),
				}})
		}
	}

	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard,
		[]models.InlineKeyboardButton{
			{Text: "➕ Add", CallbackData: "ma:a"},
			{Text: "🗑 Clear all", CallbackData: "ma:c"},
		})
	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard, navRow(viewMaintainers, page, pages)...)
	p.text = b.String()
	return p, nil
}

// suggestionsPanel offers cached nicknames close to what the user typed, so a
// typo is one tap from being fixed. The offer list is persisted with a hash so
// the ☐ buttons can be resolved later without trusting an index alone.
func (bt *Bot) suggestionsPanel(ctx context.Context, chatID int64, candidates []string) (*panel, error) {
	list, err := bt.store.ListMaintainers(ctx, chatID)
	if err != nil {
		return nil, err
	}
	already := toSet(list)

	var offers []string
	seen := map[string]struct{}{}
	for _, c := range candidates {
		for _, s := range bt.suggestionsFor(ctx, c) {
			if _, dup := seen[s.Nickname]; dup {
				continue
			}
			if _, have := already[s.Nickname]; have {
				continue
			}
			seen[s.Nickname] = struct{}{}
			offers = append(offers, s.Nickname)
		}
	}
	if len(offers) == 0 {
		return nil, nil
	}
	if len(offers) > 8 {
		offers = offers[:8]
	}

	hash := listHash(offers)
	if err := bt.rememberSuggestions(ctx, chatID, offers); err != nil {
		return nil, err
	}

	p := &panel{}
	p.text = "<b>Did you mean?</b>\n\nThese nicknames are in the cached ALT packager directory."
	for i, nick := range offers {
		p.markup.InlineKeyboard = append(p.markup.InlineKeyboard,
			[]models.InlineKeyboardButton{{
				Text:         emptyBox + " " + nick,
				CallbackData: fmt.Sprintf("ma:s:%d:%s", i, hash),
			}})
	}
	p.markup.InlineKeyboard = append(p.markup.InlineKeyboard,
		[]models.InlineKeyboardButton{{Text: "✅ Done", CallbackData: "dn:" + viewMaintainers}})
	return p, nil
}

// rememberSuggestions persists the offer list behind the ☐ buttons.
func (bt *Bot) rememberSuggestions(ctx context.Context, chatID int64, offers []string) error {
	payload, err := json.Marshal(offers)
	if err != nil {
		return fmt.Errorf("telegram: encode suggestions: %w", err)
	}
	return bt.store.SetConversation(ctx, chatID, model.StepSuggestions, string(payload))
}

// resolveSuggestion turns a button index back into a nickname, refusing when
// the panel it came from is no longer current.
func (bt *Bot) resolveSuggestion(ctx context.Context, chatID int64, index int, hash string) (string, error) {
	conv, err := bt.store.GetConversation(ctx, chatID)
	if err != nil {
		return "", err
	}
	if conv == nil || conv.Step != model.StepSuggestions {
		return "", errStaleCallback
	}
	var offers []string
	if err := json.Unmarshal([]byte(conv.Payload), &offers); err != nil {
		return "", errStaleCallback
	}
	if listHash(offers) != hash {
		return "", errStaleCallback
	}
	if index < 0 || index >= len(offers) {
		return "", errStaleCallback
	}
	return offers[index], nil
}

// suggestionsFor asks the cached directory for nicknames that start with what
// the user typed.
func (bt *Bot) suggestionsFor(ctx context.Context, prefix string) []model.Maintainer {
	if prefix == "" {
		return nil
	}
	out, err := bt.store.SuggestMaintainers(ctx, prefix, 4)
	if err != nil {
		bt.log.Debug("maintainer suggestions failed", "prefix", prefix, "error", err.Error())
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// settings
// ---------------------------------------------------------------------------

// settingsPanel is the hub: a summary plus one button per editable screen.
func (bt *Bot) settingsPanel(ctx context.Context, chatID int64) (*panel, error) {
	chat, err := bt.store.GetChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	sub, err := bt.store.LoadSubscription(ctx, chatID)
	if err != nil {
		return nil, err
	}

	p := &panel{}
	p.text = renderSettingsText(chat, sub, bt.terminal)

	statusBtn := models.InlineKeyboardButton{Text: "⏸ Pause", CallbackData: "en:0"}
	if !chat.Enabled {
		statusBtn = models.InlineKeyboardButton{Text: "▶ Resume", CallbackData: "en:1"}
	}
	modeLabel := "🔔 Every change"
	if chat.NotifyMode == model.NotifyTerminal {
		modeLabel = "🔔 Terminal only"
	}

	p.markup.InlineKeyboard = [][]models.InlineKeyboardButton{
		{{Text: "👤 Maintainers", CallbackData: "rf:" + viewMaintainers},
			{Text: "🧪 States", CallbackData: "rf:" + viewStates}},
		{{Text: "🌿 Branches", CallbackData: "rf:" + viewBranches},
			{Text: modeLabel, CallbackData: "nm"}},
		{statusBtn, {Text: "📨 Test", CallbackData: "tt"}},
		{{Text: "🕘 Recent", CallbackData: "rc"}, {Text: "❓ Help", CallbackData: "hp"}},
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func toSet(items []string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, i := range items {
		out[i] = struct{}{}
	}
	return out
}

// parseIntSafe is strconv.Atoi without the error, for callback payloads the
// bot itself produced.
func parseIntSafe(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// validCallbackData rejects an empty or over-long payload before routing.
func validCallbackData(s string) bool {
	return s != "" && len(s) <= maxCallbackDataLen
}

// joinList renders a short inline list, collapsing the tail.
func joinList(items []string, max int) string {
	if len(items) == 0 {
		return "—"
	}
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" … +%d", len(items)-max)
}
