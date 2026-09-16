package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/notifier"
	"altrdb-telegram-bot/internal/rdb"
)

// command is a parsed "/name@bot args".
type command struct {
	Name string
	Args string
}

// parseCommand extracts the command name and arguments. A command addressed to
// a different bot (possible in groups) is reported as not-a-command so the
// caller stays quiet instead of answering somebody else's bot.
func parseCommand(text, botUsername string) (command, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return command{}, false
	}
	head, args, _ := strings.Cut(text, " ")
	head = strings.TrimPrefix(head, "/")
	if head == "" {
		return command{}, false
	}
	name := head
	if user, bot, found := strings.Cut(head, "@"); found {
		name = user
		if botUsername != "" && !strings.EqualFold(bot, botUsername) {
			return command{}, false
		}
	}
	name = strings.ToLower(name)
	if name == "" {
		return command{}, false
	}
	return command{Name: name, Args: strings.TrimSpace(args)}, true
}

// handleMessage routes one incoming message.
func (bt *Bot) handleMessage(ctx context.Context, m *models.Message) {
	a := bt.guardMessage(ctx, m)
	if !a.allowed {
		if a.refuse != "" {
			bt.send(ctx, a.chatID, a.refuse, nil)
		}
		return
	}

	text := strings.TrimSpace(m.Text)
	if cmd, ok := parseCommand(text, bt.botUsername); ok {
		bt.commands.Add(1)
		bt.runCommand(ctx, a, cmd)
		return
	}
	if isCommandForAnotherBot(text, bt.botUsername) {
		// Not ours, and not even worth a hint: in a shared chat every bot
		// answering somebody else's command is noise.
		bt.log.Debug("ignoring a command addressed to another bot", "chat_id", a.chatID)
		return
	}

	conv, err := bt.store.GetConversation(ctx, a.chatID)
	if err != nil {
		bt.log.Error("reading conversation state failed", "chat_id", a.chatID, "error", err.Error())
		bt.send(ctx, a.chatID, "I could not read my own state just now. Please try again.", nil)
		return
	}
	if conv != nil && conv.Step == model.StepAwaitingMaintainers {
		bt.handleMaintainerInput(ctx, a, text)
		return
	}

	if text == "" {
		bt.log.Debug("ignoring non-text message", "chat_id", a.chatID)
		return
	}
	bt.send(ctx, a.chatID,
		"Send /help for the command list, or /settings to review your subscription.", nil)
}

// runCommand dispatches one command. Unknown commands get a short hint rather
// than silence, because Telegram clients autocomplete arbitrary slash words.
func (bt *Bot) runCommand(ctx context.Context, a access, cmd command) {
	bt.log.Debug("command", "chat_id", a.chatID, "command", cmd.Name, "has_args", cmd.Args != "")

	// Any command ends a pending prompt: the user changed their mind. /cancel
	// is left to report on the prompt itself.
	if cmd.Name != "cancel" {
		if conv, err := bt.store.GetConversation(ctx, a.chatID); err != nil {
			bt.log.Warn("reading conversation state failed", "chat_id", a.chatID, "error", err.Error())
		} else if conv != nil && conv.Step != model.StepNone {
			if err := bt.store.ClearConversation(ctx, a.chatID); err != nil {
				bt.log.Warn("clearing conversation failed", "chat_id", a.chatID, "error", err.Error())
			}
		}
	}

	var err error
	switch cmd.Name {
	case "start":
		err = bt.cmdStart(ctx, a)
	case "settings":
		err = bt.cmdSettings(ctx, a)
	case "maintainers":
		err = bt.cmdMaintainers(ctx, a, cmd.Args)
	case "states":
		err = bt.cmdStates(ctx, a, cmd.Args)
	case "branches":
		err = bt.cmdBranches(ctx, a, cmd.Args)
	case "recent":
		err = bt.cmdRecent(ctx, a, cmd.Args)
	case "pause":
		err = bt.cmdSetEnabled(ctx, a, false)
	case "resume":
		err = bt.cmdSetEnabled(ctx, a, true)
	case "test":
		err = bt.cmdTest(ctx, a)
	case "task":
		err = bt.cmdTask(ctx, a, cmd.Args)
	case "cancel":
		err = bt.cmdCancel(ctx, a)
	case "help":
		bt.send(ctx, a.chatID, renderHelp(bt.botUsername), nil)
	default:
		bt.send(ctx, a.chatID,
			fmt.Sprintf("Unknown command <code>/%s</code>. Send /help for the list.",
				notifier.EscapeHTML(cmd.Name)), nil)
		return
	}
	if err != nil {
		bt.log.Error("command failed", "chat_id", a.chatID, "command", cmd.Name, "error", err.Error())
		bt.setLastError(err.Error())
		bt.send(ctx, a.chatID, "That did not work: "+notifier.EscapeHTML(err.Error())+
			"\nTry again in a moment, or /help.", nil)
	}
}

// ---------------------------------------------------------------------------
// individual commands
// ---------------------------------------------------------------------------

func (bt *Bot) cmdStart(ctx context.Context, a access) error {
	if _, err := bt.store.RegisterChat(ctx, a.chatID, a.chatType, a.username, a.firstName); err != nil {
		return err
	}
	// A fresh start clears a half-finished prompt.
	if err := bt.store.ClearConversation(ctx, a.chatID); err != nil {
		bt.log.Warn("clearing conversation failed", "chat_id", a.chatID, "error", err.Error())
	}
	chat, err := bt.store.GetChat(ctx, a.chatID)
	if err != nil {
		return err
	}
	sub, err := bt.store.LoadSubscription(ctx, a.chatID)
	if err != nil {
		return err
	}
	p, err := bt.settingsPanel(ctx, a.chatID)
	if err != nil {
		return err
	}
	bt.send(ctx, a.chatID, renderOnboarding(chat, sub, bt.botUsername)+"\n\n"+p.text, p.markup)
	return nil
}

func (bt *Bot) cmdSettings(ctx context.Context, a access) error {
	if _, err := bt.ensureChat(ctx, a); err != nil {
		return err
	}
	p, err := bt.settingsPanel(ctx, a.chatID)
	if err != nil {
		return err
	}
	bt.send(ctx, a.chatID, p.text, p.markup)
	return nil
}

func (bt *Bot) cmdMaintainers(ctx context.Context, a access, args string) error {
	if _, err := bt.ensureChat(ctx, a); err != nil {
		return err
	}
	if strings.TrimSpace(args) != "" {
		bt.handleMaintainerInput(ctx, a, args)
		return nil
	}
	p, err := bt.maintainersPanel(ctx, a.chatID, 0)
	if err != nil {
		return err
	}
	bt.send(ctx, a.chatID, p.text, p.markup)
	return nil
}

func (bt *Bot) cmdStates(ctx context.Context, a access, args string) error {
	if _, err := bt.ensureChat(ctx, a); err != nil {
		return err
	}
	if strings.TrimSpace(args) != "" {
		return bt.setStatesFromArgs(ctx, a, args)
	}
	p, err := bt.statesPanel(ctx, a.chatID, 0)
	if err != nil {
		return err
	}
	bt.send(ctx, a.chatID, p.text, p.markup)
	return nil
}

func (bt *Bot) cmdBranches(ctx context.Context, a access, args string) error {
	if _, err := bt.ensureChat(ctx, a); err != nil {
		return err
	}
	if strings.TrimSpace(args) != "" {
		return bt.setBranchesFromArgs(ctx, a, args)
	}
	// Opening the branch screen is a good moment to make sure the catalogue is
	// not stale, but never fatal: the cached list is shown either way.
	bt.refreshBranchCatalogueNow(ctx)
	p, err := bt.branchesPanel(ctx, a.chatID, 0)
	if err != nil {
		return err
	}
	bt.send(ctx, a.chatID, p.text, p.markup)
	return nil
}

// setStatesFromArgs implements "/states DONE FAILED": an explicit replacement
// of the selection, restricted to states the catalogue knows.
func (bt *Bot) setStatesFromArgs(ctx context.Context, a access, args string) error {
	known, err := bt.store.StateNames(ctx)
	if err != nil {
		return err
	}
	catalogue := toSet(known)

	var want, rejected []string
	for _, tok := range strings.FieldsFunc(args, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if _, ok := catalogue[tok]; !ok {
			rejected = append(rejected, tok)
			continue
		}
		want = append(want, tok)
	}
	if len(want) == 0 && len(rejected) == 0 {
		return errors.New("no states given; try /states DONE FAILED")
	}
	if err := bt.store.SetAllChatStates(ctx, a.chatID, want); err != nil {
		return err
	}
	model.SortStates(want)
	msg := "✅ States set to: " + notifier.EscapeHTML(strings.Join(want, ", "))
	if len(rejected) > 0 {
		msg += "\n⚠️ Not in the catalogue yet: " + notifier.EscapeHTML(strings.Join(rejected, ", "))
	}
	bt.send(ctx, a.chatID, msg, nil)
	return nil
}

// setBranchesFromArgs implements "/branches sisyphus p11".
func (bt *Bot) setBranchesFromArgs(ctx context.Context, a access, args string) error {
	bt.refreshBranchCatalogueNow(ctx)
	known, err := bt.store.BranchNames(ctx)
	if err != nil {
		return err
	}
	catalogue := toSet(known)

	var want, rejected []string
	for _, tok := range strings.FieldsFunc(args, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if _, ok := catalogue[tok]; !ok {
			rejected = append(rejected, tok)
			continue
		}
		want = append(want, tok)
	}
	if len(want) == 0 && len(rejected) == 0 {
		return errors.New("no branches given; try /branches sisyphus p11")
	}
	if err := bt.store.SetAllChatBranches(ctx, a.chatID, want); err != nil {
		return err
	}
	msg := "✅ Branches set to: " + notifier.EscapeHTML(strings.Join(want, ", ")) +
		"\nCounting from now; tasks already in the feed are not replayed."
	if len(rejected) > 0 {
		msg += "\n⚠️ Unknown to RDB: " + notifier.EscapeHTML(strings.Join(rejected, ", "))
	}
	bt.send(ctx, a.chatID, msg, nil)
	return nil
}

func (bt *Bot) cmdRecent(ctx context.Context, a access, args string) error {
	if _, err := bt.ensureChat(ctx, a); err != nil {
		return err
	}
	limit := 10
	if args != "" {
		n, err := strconv.Atoi(strings.TrimSpace(args))
		if err != nil || n < 1 {
			return fmt.Errorf("usage: /recent [count], got %q", args)
		}
		limit = min(n, 30)
	}
	sub, err := bt.store.LoadSubscription(ctx, a.chatID)
	if err != nil {
		return err
	}
	events, err := bt.store.RecentMatching(ctx, a.chatID, limit)
	if err != nil {
		return err
	}
	bt.send(ctx, a.chatID, renderRecent(events, sub, limit), nil)
	return nil
}

func (bt *Bot) cmdSetEnabled(ctx context.Context, a access, enabled bool) error {
	chat, err := bt.ensureChat(ctx, a)
	if err != nil {
		return err
	}
	if err := bt.store.SetChatEnabled(ctx, a.chatID, enabled, ""); err != nil {
		return err
	}
	if enabled {
		bt.send(ctx, a.chatID, fmt.Sprintf(
			"▶ Notifications resumed for %s.\nUse /settings to review the subscription.",
			notifier.EscapeHTML(joinList(subBranchNames(ctx, bt, a.chatID), 8))), nil)
		return nil
	}
	bt.send(ctx, a.chatID, fmt.Sprintf(
		"⏸ Notifications paused. Your settings are kept; send /resume to turn them back on.\n"+
			"(Was: %s)", notifier.EscapeHTML(summariseChat(chat))), nil)
	return nil
}

func (bt *Bot) cmdTest(ctx context.Context, a access) error {
	if _, err := bt.ensureChat(ctx, a); err != nil {
		return err
	}
	sub, err := bt.store.LoadSubscription(ctx, a.chatID)
	if err != nil {
		return err
	}
	owner := "grenka"
	if len(sub.Maintainers) > 0 {
		owner = sub.Maintainers[0]
	}
	branch := "sisyphus"
	if len(sub.Branches) > 0 {
		branch = sub.Branches[0].Branch
	}
	ev := notifier.SampleEvent(owner, branch, bt.clock.Now())

	markup := models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: fmt.Sprintf("Open task #%d", ev.TaskID), URL: notifier.TaskURL(ev.TaskID)},
	}}}

	if n := bt.notifier(); n != nil {
		if err := n.SendNow(ctx, a.chatID, notifier.Render(ev), markup); err != nil {
			return fmt.Errorf("telegram rejected the test message: %w", err)
		}
	} else {
		bt.send(ctx, a.chatID, notifier.Render(ev), markup)
	}
	if !sub.Complete() {
		bt.send(ctx, a.chatID, "⚠️ Your subscription is incomplete, so real notifications would not match anything yet:\n"+
			notifier.EscapeHTML(strings.Join(sub.Missing(), "\n")), nil)
	}
	return nil
}

func (bt *Bot) cmdTask(ctx context.Context, a access, args string) error {
	if _, err := bt.ensureChat(ctx, a); err != nil {
		return err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		return errors.New("usage: /task <numeric id>, for example /task 433219")
	}
	if bt.tasks == nil {
		return errors.New("task lookup is not wired up in this build")
	}

	lookupCtx, cancel := context.WithTimeout(ctx, bt.lookupTimeout())
	defer cancel()

	info, err := bt.tasks.TaskInfo(lookupCtx, id)
	if err != nil {
		if errors.Is(err, rdb.ErrNotFound) {
			bt.send(ctx, a.chatID, fmt.Sprintf("RDB does not know task #%d.", id), nil)
			return nil
		}
		return fmt.Errorf("RDB lookup failed: %w", err)
	}

	task, err := info.ToTask(info.TaskRepo)
	if err != nil {
		return fmt.Errorf("RDB returned an unusable record: %w", err)
	}
	ev := model.TaskEvent{
		TaskRepo: task.TaskRepo, TaskID: task.TaskID, State: task.State, Owner: task.Owner,
		Try: task.Try, Iter: task.Iter, Testonly: task.Testonly, Stage: task.Stage,
		Message: task.Message, Changed: task.Changed, Packages: task.Packages,
	}
	bt.send(ctx, a.chatID, renderTaskDetail(ev, rdb.ApprovalSummary(info.Approval, 8), len(info.Subtasks)), nil)
	return nil
}

func (bt *Bot) cmdCancel(ctx context.Context, a access) error {
	conv, err := bt.store.GetConversation(ctx, a.chatID)
	if err != nil {
		return err
	}
	if conv == nil || conv.Step == model.StepNone {
		bt.send(ctx, a.chatID, "Nothing to cancel.", nil)
		return nil
	}
	if err := bt.store.ClearConversation(ctx, a.chatID); err != nil {
		return err
	}
	bt.send(ctx, a.chatID, "Cancelled. Send /settings to continue.", nil)
	return nil
}

// ---------------------------------------------------------------------------
// multi-step maintainer input
// ---------------------------------------------------------------------------

// handleMaintainerInput consumes the free-text answer to "➕ Add".
func (bt *Bot) handleMaintainerInput(ctx context.Context, a access, text string) {
	if strings.TrimSpace(text) == "" {
		bt.send(ctx, a.chatID,
			"Send the nicknames you want to follow, separated by spaces or commas.\n"+
				"Example: <code>grenka rider @glebfm</code>\n/cancel aborts.", nil)
		return
	}
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		// A command while a prompt is open ends the prompt and runs normally.
		if err := bt.store.ClearConversation(ctx, a.chatID); err != nil {
			bt.log.Warn("clearing conversation failed", "chat_id", a.chatID, "error", err.Error())
		}
		if cmd, ok := parseCommand(text, bt.botUsername); ok {
			bt.runCommand(ctx, a, cmd)
			return
		}
	}

	valid, invalid := model.ParseNicknames(text)

	existing, err := bt.store.ListMaintainers(ctx, a.chatID)
	if err != nil {
		bt.send(ctx, a.chatID, "I could not read your maintainer list: "+notifier.EscapeHTML(err.Error()), nil)
		return
	}
	have := toSet(existing)

	var fresh, duplicates []string
	for _, n := range valid {
		if _, ok := have[n]; ok {
			duplicates = append(duplicates, n)
			continue
		}
		fresh = append(fresh, n)
	}

	added, err := bt.store.AddMaintainers(ctx, a.chatID, fresh)
	if err != nil {
		if strings.Contains(err.Error(), "at most") {
			bt.send(ctx, a.chatID, "❌ "+notifier.EscapeHTML(err.Error()), nil)
			return
		}
		bt.send(ctx, a.chatID, "Adding maintainers failed: "+notifier.EscapeHTML(err.Error()), nil)
		return
	}

	unknown := bt.unknownNicknames(ctx, a.chatID, added)

	if err := bt.store.ClearConversation(ctx, a.chatID); err != nil {
		bt.log.Warn("clearing conversation failed", "chat_id", a.chatID, "error", err.Error())
	}

	bt.send(ctx, a.chatID, renderMaintainerResult(added, duplicates, invalid, unknown), nil)

	// Offer corrections for anything we could not confirm.
	candidates := append([]string{}, invalid...)
	candidates = append(candidates, unknown...)
	if len(candidates) > 0 {
		if p, err := bt.suggestionsPanel(ctx, a.chatID, candidates); err != nil {
			bt.log.Warn("building suggestions failed", "chat_id", a.chatID, "error", err.Error())
		} else if p != nil {
			bt.send(ctx, a.chatID, p.text, p.markup)
		}
	}

	if p, err := bt.maintainersPanel(ctx, a.chatID, 0); err == nil {
		bt.send(ctx, a.chatID, p.text, p.markup)
	}
}

// unknownNicknames reports which of the freshly added nicknames are missing
// from the cached directory of the chat's own branches. It is a warning, never
// a block: the directory is per branch and cached, and a packager can be absent
// from one branch while owning packages in another.
func (bt *Bot) unknownNicknames(ctx context.Context, chatID int64, added []string) []string {
	if len(added) == 0 {
		return nil
	}
	sub, err := bt.store.LoadSubscription(ctx, chatID)
	if err != nil {
		bt.log.Warn("loading subscription for validation failed", "chat_id", chatID, "error", err.Error())
		return nil
	}
	branches := sub.BranchNames()
	if len(branches) == 0 {
		// Nothing selected yet, so there is nothing to validate against.
		return nil
	}

	var unknown []string
	for _, nick := range added {
		found, _, _, err := bt.store.FindMaintainer(ctx, nick, branches)
		if err != nil {
			bt.log.Warn("maintainer lookup failed", "nickname", nick, "error", err.Error())
			continue
		}
		if !found {
			unknown = append(unknown, nick)
		}
	}
	return unknown
}

// summariseChat is a one-line description used in confirmations.
func summariseChat(chat *model.Chat) string {
	if chat == nil {
		return "unknown chat"
	}
	if chat.FirstName != "" {
		return chat.FirstName
	}
	if chat.Username != "" {
		return "@" + chat.Username
	}
	return fmt.Sprintf("chat %d", chat.ID)
}

// subBranchNames is a small helper for confirmation messages.
func subBranchNames(ctx context.Context, bt *Bot, chatID int64) []string {
	sub, err := bt.store.LoadSubscription(ctx, chatID)
	if err != nil {
		return nil
	}
	return sub.BranchNames()
}
