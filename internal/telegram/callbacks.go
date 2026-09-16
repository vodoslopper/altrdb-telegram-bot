package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/store"
)

// errStaleCallback means a button referred to a list that has since changed.
var errStaleCallback = errors.New("this control refers to an outdated list")

// callbackMessage extracts the message a button was attached to, and reports
// whether it is still accessible. Telegram replaces a message older than 48
// hours with an InaccessibleMessage, which cannot be edited.
func callbackMessage(cb *models.CallbackQuery) (*models.Message, bool) {
	if cb == nil {
		return nil, false
	}
	if cb.Message.Type == models.MaybeInaccessibleMessageTypeMessage && cb.Message.Message != nil {
		return cb.Message.Message, true
	}
	return nil, false
}

// answerOnce guarantees exactly one answerCallbackQuery per update. Telegram
// rejects a second answer, and an unanswered callback leaves the client
// spinner running for up to a minute.
type answerOnce struct {
	bt   *Bot
	ctx  context.Context
	id   string
	done bool
}

func (a *answerOnce) reply(text string, alert bool) {
	if a.done || a.id == "" {
		return
	}
	a.done = true
	a.bt.answer(a.ctx, a.id, text, alert)
}

func (a *answerOnce) ok() { a.reply("", false) }

// handleCallback routes one button press.
func (bt *Bot) handleCallback(ctx context.Context, cb *models.CallbackQuery) {
	ans := &answerOnce{bt: bt, ctx: ctx, id: cb.ID}

	a := bt.guardCallback(ctx, cb)
	if !a.allowed {
		text := a.refuse
		if text == "" {
			text = "Not allowed."
		}
		ans.reply(text, true)
		return
	}

	msg, accessible := callbackMessage(cb)
	if !accessible {
		ans.reply("This menu has expired. Send /settings for a fresh one.", true)
		return
	}
	// The chat is taken from the message, and the message must be ours. Both
	// checks stop a forwarded panel from driving somebody else's subscription.
	if msg.Chat.ID != a.chatID {
		bt.log.Warn("callback chat mismatch",
			"message_chat", msg.Chat.ID, "update_chat", a.chatID, "data", cb.Data)
		ans.reply("This menu belongs to another chat.", true)
		return
	}
	// In a private chat the chat id *is* the user id. A panel from somebody
	// else's private chat therefore cannot be driven by this press, which is
	// what stops a forwarded or replayed button from editing another
	// subscription.
	if a.chatType == string(models.ChatTypePrivate) && a.userID != a.chatID {
		bt.log.Warn("callback user does not own the chat",
			"chat_id", a.chatID, "user_id", a.userID, "data", cb.Data)
		ans.reply("This menu belongs to another chat.", true)
		return
	}
	if bt.botID != 0 && msg.From != nil && msg.From.ID != bt.botID {
		bt.log.Warn("callback on a foreign message",
			"chat_id", a.chatID, "message_from", msg.From.ID, "data", cb.Data)
		ans.reply("I can only edit menus I sent.", true)
		return
	}

	if !validCallbackData(cb.Data) {
		bt.log.Warn("callback data out of range", "chat_id", a.chatID, "len", len(cb.Data))
		ans.reply("This button is malformed.", true)
		return
	}

	if _, err := bt.ensureChat(ctx, a); err != nil {
		bt.log.Error("ensuring chat failed", "chat_id", a.chatID, "error", err.Error())
		ans.reply("Internal error, please try /settings.", true)
		return
	}

	parts := strings.Split(cb.Data, ":")
	action := parts[0]

	// Anything that may touch the network answers the callback first, so the
	// client spinner stops before the work starts.
	if action == "rf" && len(parts) > 1 && parts[1] == viewBranches {
		ans.ok()
	}

	var (
		text   string
		markup models.ReplyMarkup
		err    error
	)

	switch action {
	case "st":
		text, markup, err = bt.cbToggleState(ctx, a, parts)
	case "br":
		text, markup, err = bt.cbToggleBranch(ctx, a, parts)
	case "sa", "ca":
		text, markup, err = bt.cbBulk(ctx, a, parts, action == "sa")
	case "pg":
		text, markup, err = bt.cbPage(ctx, a, parts)
	case "rf":
		text, markup, err = bt.cbRefresh(ctx, a, parts)
	case "dn":
		text, markup, err = bt.cbDone(ctx, a, parts)
	case "ma":
		text, markup, err = bt.cbMaintainer(ctx, a, parts, ans)
	case "nm":
		text, markup, err = bt.cbNotifyMode(ctx, a)
	case "en":
		text, markup, err = bt.cbEnable(ctx, a, parts)
	case "tt":
		err = bt.cmdTest(ctx, a)
		text, markup, err = bt.withSettings(ctx, a, err)
	case "rc":
		if err := bt.cmdRecent(ctx, a, ""); err != nil {
			bt.log.Error("recent tasks failed", "chat_id", a.chatID, "error", err.Error())
			bt.setLastError(err.Error())
			ans.reply("Could not load recent tasks: "+err.Error(), true)
			return
		}
		ans.reply("Sent as a new message", false)
		return
	case "hp":
		bt.send(ctx, a.chatID, renderHelp(bt.botUsername), nil)
		ans.ok()
		return
	default:
		bt.log.Warn("unknown callback action", "chat_id", a.chatID, "data", cb.Data)
		ans.reply("Unknown control. Send /settings for a fresh menu.", true)
		return
	}

	if err != nil {
		if errors.Is(err, errStaleCallback) {
			ans.reply("That list changed under you — tap ⟳ Refresh.", true)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			ans.reply("That entry no longer exists — tap ⟳ Refresh.", true)
			return
		}
		bt.log.Error("callback failed",
			"chat_id", a.chatID, "data", cb.Data, "error", err.Error())
		bt.setLastError(err.Error())
		ans.reply("That did not work: "+err.Error(), true)
		return
	}

	ans.ok()
	if err := bt.edit(ctx, a.chatID, msg.ID, text, markup); err != nil {
		bt.log.Debug("editing the panel failed", "chat_id", a.chatID, "error", err.Error())
	}
}

// withSettings re-renders the hub after an action that sends its own message.
func (bt *Bot) withSettings(ctx context.Context, a access, prev error) (string, models.ReplyMarkup, error) {
	p, err := bt.settingsPanel(ctx, a.chatID)
	if err != nil {
		return "", nil, err
	}
	return p.text, p.markup, prev
}

// ---------------------------------------------------------------------------
// checkbox actions
// ---------------------------------------------------------------------------

func (bt *Bot) cbToggleState(ctx context.Context, a access, parts []string) (string, models.ReplyMarkup, error) {
	if len(parts) != 3 {
		return "", nil, errStaleCallback
	}
	id := parseIntSafe(parts[1])
	page := parseIntSafe(parts[2])

	name, err := bt.store.StateName(ctx, int64(id))
	if err != nil {
		return "", nil, err
	}
	on, err := bt.store.ToggleChatState(ctx, a.chatID, name)
	if err != nil {
		return "", nil, err
	}
	bt.log.Info("state toggled", "chat_id", a.chatID, "state", name, "on", on)
	return bt.render(ctx, a.chatID, viewStates, page)
}

func (bt *Bot) cbToggleBranch(ctx context.Context, a access, parts []string) (string, models.ReplyMarkup, error) {
	if len(parts) != 3 {
		return "", nil, errStaleCallback
	}
	id := parseIntSafe(parts[1])
	page := parseIntSafe(parts[2])

	name, err := bt.store.BranchName(ctx, int64(id))
	if err != nil {
		return "", nil, err
	}
	on, watermark, err := bt.store.ToggleChatBranch(ctx, a.chatID, name)
	if err != nil {
		return "", nil, err
	}
	if on {
		bt.log.Info("branch checked", "chat_id", a.chatID, "branch", name,
			"watermark", model.FormatTime(watermark))
		// Warm the packager directory for the new branch so nickname
		// validation has something to check against.
		bt.warmMaintainerCache(ctx, name)
	} else {
		bt.log.Info("branch unchecked", "chat_id", a.chatID, "branch", name)
	}
	return bt.render(ctx, a.chatID, viewBranches, page)
}

func (bt *Bot) cbBulk(ctx context.Context, a access, parts []string, all bool) (string, models.ReplyMarkup, error) {
	if len(parts) != 3 {
		return "", nil, errStaleCallback
	}
	view := parts[1]
	page := parseIntSafe(parts[2])

	switch view {
	case viewStates:
		var states []string
		if all {
			names, err := bt.store.StateNames(ctx)
			if err != nil {
				return "", nil, err
			}
			states = names
		}
		if err := bt.store.SetAllChatStates(ctx, a.chatID, states); err != nil {
			return "", nil, err
		}
		bt.log.Info("states bulk update", "chat_id", a.chatID, "select_all", all, "count", len(states))
	case viewBranches:
		var branches []string
		if all {
			names, err := bt.store.BranchNames(ctx)
			if err != nil {
				return "", nil, err
			}
			branches = names
		}
		if err := bt.store.SetAllChatBranches(ctx, a.chatID, branches); err != nil {
			return "", nil, err
		}
		bt.log.Info("branches bulk update", "chat_id", a.chatID, "select_all", all, "count", len(branches))
	default:
		return "", nil, errStaleCallback
	}
	return bt.render(ctx, a.chatID, view, page)
}

func (bt *Bot) cbPage(ctx context.Context, a access, parts []string) (string, models.ReplyMarkup, error) {
	if len(parts) != 3 {
		return "", nil, errStaleCallback
	}
	return bt.render(ctx, a.chatID, parts[1], parseIntSafe(parts[2]))
}

func (bt *Bot) cbRefresh(ctx context.Context, a access, parts []string) (string, models.ReplyMarkup, error) {
	if len(parts) != 2 {
		return "", nil, errStaleCallback
	}
	view := parts[1]
	if view == viewBranches {
		bt.refreshBranchCatalogueNow(ctx)
	}
	return bt.render(ctx, a.chatID, view, 0)
}

func (bt *Bot) cbDone(ctx context.Context, a access, parts []string) (string, models.ReplyMarkup, error) {
	if len(parts) != 2 {
		return "", nil, errStaleCallback
	}
	view := parts[1]
	if view == viewSettings {
		// Close the hub by dropping its keyboard.
		p, err := bt.settingsPanel(ctx, a.chatID)
		if err != nil {
			return "", nil, err
		}
		return p.text, nil, nil
	}
	p, err := bt.settingsPanel(ctx, a.chatID)
	if err != nil {
		return "", nil, err
	}
	return p.text, p.markup, nil
}

func (bt *Bot) cbNotifyMode(ctx context.Context, a access) (string, models.ReplyMarkup, error) {
	chat, err := bt.store.GetChat(ctx, a.chatID)
	if err != nil {
		return "", nil, err
	}
	next := model.NotifyTerminal
	if chat.NotifyMode == model.NotifyTerminal {
		next = model.NotifyAll
	}
	if err := bt.store.SetNotifyMode(ctx, a.chatID, next); err != nil {
		return "", nil, err
	}
	bt.log.Info("notify mode changed", "chat_id", a.chatID, "mode", string(next))
	p, err := bt.settingsPanel(ctx, a.chatID)
	if err != nil {
		return "", nil, err
	}
	return p.text, p.markup, nil
}

func (bt *Bot) cbEnable(ctx context.Context, a access, parts []string) (string, models.ReplyMarkup, error) {
	if len(parts) != 2 {
		return "", nil, errStaleCallback
	}
	enabled := parts[1] == "1"
	if err := bt.store.SetChatEnabled(ctx, a.chatID, enabled, ""); err != nil {
		return "", nil, err
	}
	bt.log.Info("delivery toggled", "chat_id", a.chatID, "enabled", enabled)
	p, err := bt.settingsPanel(ctx, a.chatID)
	if err != nil {
		return "", nil, err
	}
	return p.text, p.markup, nil
}

// ---------------------------------------------------------------------------
// maintainer actions
// ---------------------------------------------------------------------------

func (bt *Bot) cbMaintainer(ctx context.Context, a access, parts []string, ans *answerOnce) (string, models.ReplyMarkup, error) {
	if len(parts) < 2 {
		return "", nil, errStaleCallback
	}
	switch parts[1] {
	case "a":
		if err := bt.store.SetConversation(ctx, a.chatID, model.StepAwaitingMaintainers, ""); err != nil {
			return "", nil, err
		}
		ans.reply("Now send the nicknames", false)
		bt.send(ctx, a.chatID,
			"Send one or more ALT packager nicknames, separated by spaces or commas.\n"+
				"Example: <code>grenka rider @glebfm</code>\n\n"+
				"A leading <code>@</code> is dropped, case does not matter, and "+
				"<code>/cancel</code> aborts.", nil)
		p, err := bt.maintainersPanel(ctx, a.chatID, 0)
		if err != nil {
			return "", nil, err
		}
		return p.text, p.markup, nil

	case "c":
		n, err := bt.store.ClearMaintainers(ctx, a.chatID)
		if err != nil {
			return "", nil, err
		}
		bt.log.Info("maintainers cleared", "chat_id", a.chatID, "removed", n)
		ans.reply(fmt.Sprintf("Removed %d", n), false)
		p, err := bt.maintainersPanel(ctx, a.chatID, 0)
		if err != nil {
			return "", nil, err
		}
		return p.text, p.markup, nil

	case "d":
		if len(parts) != 4 {
			return "", nil, errStaleCallback
		}
		list, err := bt.store.ListMaintainers(ctx, a.chatID)
		if err != nil {
			return "", nil, err
		}
		if listHash(list) != parts[3] {
			return "", nil, errStaleCallback
		}
		removed, err := bt.store.RemoveMaintainerByIndex(ctx, a.chatID, parseIntSafe(parts[2]))
		if err != nil {
			return "", nil, err
		}
		bt.log.Info("maintainer removed", "chat_id", a.chatID, "nickname", removed)
		ans.reply("Removed "+removed, false)
		p, err := bt.maintainersPanel(ctx, a.chatID, parseIntSafe(parts[2])/max(1, bt.pageSize))
		if err != nil {
			return "", nil, err
		}
		return p.text, p.markup, nil

	case "s":
		if len(parts) != 4 {
			return "", nil, errStaleCallback
		}
		nick, err := bt.resolveSuggestion(ctx, a.chatID, parseIntSafe(parts[2]), parts[3])
		if err != nil {
			return "", nil, err
		}
		added, err := bt.store.AddMaintainers(ctx, a.chatID, []string{nick})
		if err != nil {
			return "", nil, err
		}
		if len(added) == 0 {
			ans.reply("Already followed", false)
		} else {
			bt.log.Info("maintainer added from suggestion", "chat_id", a.chatID, "nickname", nick)
			ans.reply("Added "+nick, false)
		}
		if err := bt.store.ClearConversation(ctx, a.chatID); err != nil {
			bt.log.Warn("clearing suggestions failed", "chat_id", a.chatID, "error", err.Error())
		}
		p, err := bt.maintainersPanel(ctx, a.chatID, 0)
		if err != nil {
			return "", nil, err
		}
		return p.text, p.markup, nil

	default:
		return "", nil, errStaleCallback
	}
}

// ---------------------------------------------------------------------------
// rendering helpers
// ---------------------------------------------------------------------------

// render rebuilds one view. It is the single place a callback turns into a new
// panel, so every control ends up looking the same.
func (bt *Bot) render(ctx context.Context, chatID int64, view string, page int) (string, models.ReplyMarkup, error) {
	var (
		p   *panel
		err error
	)
	switch view {
	case viewStates:
		p, err = bt.statesPanel(ctx, chatID, page)
	case viewBranches:
		p, err = bt.branchesPanel(ctx, chatID, page)
	case viewMaintainers:
		p, err = bt.maintainersPanel(ctx, chatID, page)
	case viewSettings:
		p, err = bt.settingsPanel(ctx, chatID)
	default:
		return "", nil, fmt.Errorf("%w: unknown view %q", errStaleCallback, view)
	}
	if err != nil {
		return "", nil, err
	}
	return p.text, p.markup, nil
}

// refreshBranchCatalogueNow forces a catalogue fetch, ignoring the poller's
// refresh interval. Used by the ⟳ Refresh button, where the user explicitly
// asked for it.
func (bt *Bot) refreshBranchCatalogueNow(ctx context.Context) {
	if bt.refresh == nil {
		return
	}
	refreshCtx, cancel := context.WithTimeout(ctx, bt.lookupTimeout())
	defer cancel()
	if err := bt.refresh.RefreshBranchesNow(refreshCtx); err != nil {
		bt.log.Warn("forced branch refresh failed", "error", err.Error())
	}
}

// warmMaintainerCache makes sure the directory of a newly followed branch is
// cached, so nickname validation and suggestions have data to work with.
func (bt *Bot) warmMaintainerCache(ctx context.Context, branch string) {
	if bt.refresh == nil {
		return
	}
	if _, ok, err := bt.store.MaintainerCacheAge(ctx, branch); err == nil && ok {
		return
	}
	warmCtx, cancel := context.WithTimeout(ctx, bt.lookupTimeout())
	defer cancel()
	if err := bt.refresh.RefreshMaintainers(warmCtx, branch); err != nil {
		bt.log.Warn("maintainer cache warm-up failed", "branch", branch, "error", err.Error())
	}
}
