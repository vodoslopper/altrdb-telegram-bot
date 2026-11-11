package telegram

import (
	"fmt"
	"strings"
	"time"

	"altrdb-telegram-bot/internal/model"
	"altrdb-telegram-bot/internal/notifier"
)

// renderSettingsText summarises a chat's subscription. It is the body of both
// /settings and the settings panel, so the two never disagree.
func renderSettingsText(chat *model.Chat, sub *model.Subscription, terminal model.TerminalStates) string {
	var b strings.Builder
	b.WriteString("<b>ALT RDB build-task notifications</b>\n\n")

	if chat.Enabled {
		b.WriteString("Status: ▶ enabled\n")
	} else {
		fmt.Fprintf(&b, "Status: ⏸ paused")
		if chat.DisabledReason != "" {
			fmt.Fprintf(&b, " (%s)", notifier.EscapeHTML(chat.DisabledReason))
		}
		b.WriteString("\n")
	}

	if chat.NotifyMode == model.NotifyTerminal {
		fmt.Fprintf(&b, "Mode: terminal states only (%s)\n", notifier.EscapeHTML(strings.Join(terminalNames(terminal), ", ")))
	} else {
		b.WriteString("Mode: every matching change\n")
	}

	fmt.Fprintf(&b, "Maintainers (%d): %s\n", len(sub.Maintainers), notifier.EscapeHTML(joinList(sub.Maintainers, 12)))
	fmt.Fprintf(&b, "States (%d): %s\n", len(sub.States), notifier.EscapeHTML(joinList(sub.States, 12)))
	fmt.Fprintf(&b, "Branches (%d): %s\n", len(sub.Branches), notifier.EscapeHTML(joinBranches(sub.Branches)))

	if missing := sub.Missing(); len(missing) > 0 {
		fmt.Fprintf(&b, "\n⚠️ Not configured yet: %s", notifier.EscapeHTML(strings.Join(missing, ", ")))
	} else if !chat.Enabled {
		b.WriteString("\n⚠️ Paused: nothing will be delivered until /resume.")
	}
	return b.String()
}

func terminalNames(terminal model.TerminalStates) []string {
	out := make([]string, 0, len(terminal))
	for s := range terminal {
		out = append(out, s)
	}
	model.SortStates(out)
	return out
}

// joinBranches renders branches with the moment they started counting, which
// explains why ticking a branch did not immediately produce notifications.
func joinBranches(branches []model.BranchSubscription) string {
	if len(branches) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(branches))
	for _, b := range branches {
		if b.ActiveWatermark.IsZero() {
			parts = append(parts, b.Branch)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (since %s)", b.Branch, b.ActiveWatermark.UTC().Format("02 Jan 15:04 UTC")))
	}
	return strings.Join(parts, ", ")
}

// renderHelp is the concise command list.
func renderHelp(botUsername string) string {
	at := ""
	if botUsername != "" {
		at = "@" + botUsername
	}
	return `<b>ALT RDB build-task notifications</b>

I watch the ALT Girar task feed and tell you when a task from a maintainer you
follow changes into a state you selected, in a branch you selected.

<b>Commands</b>
/start — register this chat and show the summary
/settings — current subscription, one tap to each screen
/maintainers — add or remove packager nicknames
/states — checkbox list of task states
/branches — checkbox list of repository branches
/recent — matching tasks right now, without changing anything
/pause — stop deliveries (settings are kept)
/resume — start deliveries again
/test — choose a sample notification
/task &lt;id&gt; — fetch one task from RDB
/help — this message
/cancel — abandon a half-finished prompt

<b>Notes</b>
• Ticking a branch starts from now: tasks already in the feed are not replayed.
• Each event is delivered at most once, even across restarts.
• The feed is polled, not streamed, so a transition that happens and reverses
  inside one polling interval may never be observable.

Bot: ` + notifier.EscapeHTML(orUnknown(at))
}

// renderOnboarding is what /start answers with.
func renderOnboarding(chat *model.Chat, sub *model.Subscription, botUsername string) string {
	name := chat.FirstName
	if name == "" {
		name = chat.Username
	}
	var b strings.Builder
	if name != "" {
		fmt.Fprintf(&b, "Hello %s! ", notifier.EscapeHTML(name))
	}
	b.WriteString("I notify you about ALT Linux Girar build tasks.\n\n")

	if !sub.Complete() {
		b.WriteString("To get started, pick three things:\n" +
			"1. <b>/maintainers</b> — whose tasks you follow, e.g. <code>grenka</code>\n" +
			"2. <b>/states</b> — which states matter, e.g. <code>DONE</code> and <code>FAILED</code>\n" +
			"3. <b>/branches</b> — which repositories, e.g. <code>sisyphus</code>\n\n" +
			"Nothing is replayed: a branch starts counting from the moment you tick it.")
		return b.String()
	}
	b.WriteString("Your subscription is complete.\n\n")
	b.WriteString(renderSettingsText(chat, sub, model.NewTerminalStates(nil)))
	return b.String()
}

// renderRecent lists the currently matching tasks from the local snapshot
// table. It never touches the watermark and never queues anything.
func renderRecent(events []model.TaskEvent, sub *model.Subscription, limit int) string {
	if !sub.Complete() {
		return "Nothing to show yet: /recent lists tasks matching your maintainers, states and branches.\n" +
			"Configure them first with /maintainers, /states and /branches."
	}
	if len(events) == 0 {
		return "No task changed within the configured recent window matches your subscription.\n" +
			"The bot only keeps the most recent tasks per branch, so an idle maintainer shows nothing here."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>Recent matching tasks</b> · %d shown\n\n", len(events))
	for _, ev := range events {
		fmt.Fprintf(&b, "• <a href=\"%s\">#%d</a> <b>%s</b> · %s · @%s\n",
			notifier.TaskURL(ev.TaskID), ev.TaskID,
			notifier.EscapeHTML(ev.State),
			notifier.EscapeHTML(ev.TaskRepo),
			notifier.EscapeHTML(ev.Owner))
		if pkgs := strings.Join(ev.Packages, ", "); pkgs != "" {
			fmt.Fprintf(&b, "   📦 <b>%s</b>\n", notifier.EscapeHTML(notifier.TruncateRunes(pkgs, 160)))
		}
		fmt.Fprintf(&b, "   changed %s\n", notifier.EscapeHTML(formatWhen(ev.Changed)))
	}
	b.WriteString("\nRead-only view: it does not change what you will be notified about.")
	return b.String()
}

// renderMaintainerResult explains what an add attempt did. Unknown nicknames
// warn instead of blocking: a maintainer can be absent from the directory of
// one selected branch and present in another, and the directory is a cache.
func renderMaintainerResult(added, duplicates, invalid, unknown []string) string {
	var b strings.Builder
	switch {
	case len(added) == 0 && len(duplicates) == 0 && len(invalid) == 0:
		b.WriteString("Nothing to add.\n")
	default:
		if len(added) > 0 {
			fmt.Fprintf(&b, "✅ Added %d: %s\n", len(added), notifier.EscapeHTML(strings.Join(added, ", ")))
		}
		if len(duplicates) > 0 {
			fmt.Fprintf(&b, "Already followed: %s\n", notifier.EscapeHTML(strings.Join(duplicates, ", ")))
		}
		if len(invalid) > 0 {
			fmt.Fprintf(&b, "❌ Rejected (not a valid nickname): %s\n",
				notifier.EscapeHTML(strings.Join(invalid, ", ")))
			b.WriteString("   Expected <code>[a-z0-9][a-z0-9._+-]{0,63}</code>, e.g. <code>glebfm</code>.\n")
		}
	}
	if len(unknown) > 0 {
		fmt.Fprintf(&b, "\n⚠️ Saved but not found in the cached directory of your branches: %s\n",
			notifier.EscapeHTML(strings.Join(unknown, ", ")))
		b.WriteString("   A packager can be missing from one branch and present in another, " +
			"so these were kept. Tap ⟳ Refresh on /maintainers to re-fetch the directory.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTaskDetail shows one task fetched on demand from the task_info
// endpoint. It is a diagnostic view, not part of the notification path.
func renderTaskDetail(ev model.TaskEvent, approvals []string, subtasks int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Task #%d</b> · %s\n\n", ev.TaskID, notifier.EscapeHTML(ev.State))
	if len(ev.Packages) > 0 {
		fmt.Fprintf(&b, "📦 <b>%s</b>\n\n", notifier.EscapeHTML(notifier.TruncateRunes(strings.Join(ev.Packages, ", "), 400)))
	}
	fmt.Fprintf(&b, "Branch: <code>%s</code>\n", notifier.EscapeHTML(ev.TaskRepo))
	fmt.Fprintf(&b, "Owner: @%s\n", notifier.EscapeHTML(ev.Owner))
	fmt.Fprintf(&b, "Try/iteration: %d/%d\n", ev.Try, ev.Iter)
	if ev.Stage != "" {
		fmt.Fprintf(&b, "Stage: <code>%s</code>\n", notifier.EscapeHTML(ev.Stage))
	}
	fmt.Fprintf(&b, "Changed: %s\n", notifier.EscapeHTML(formatWhen(ev.Changed)))
	fmt.Fprintf(&b, "Subtasks: %d\n", subtasks)
	if len(approvals) > 0 {
		fmt.Fprintf(&b, "Approvals: %s\n", notifier.EscapeHTML(strings.Join(approvals, ", ")))
	}
	if msg := strings.TrimSpace(ev.Message); msg != "" {
		fmt.Fprintf(&b, "Message: <code>%s</code>\n", notifier.EscapeHTML(notifier.TruncateRunes(msg, 300)))
	}
	fmt.Fprintf(&b, "\n<a href=\"%s\">Open in Girar</a>", notifier.TaskURL(ev.TaskID))
	return b.String()
}

// formatWhen renders a timestamp for humans, with a relative hint.
func formatWhen(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	t = t.UTC()
	out := t.Format("2006-01-02 15:04 UTC")
	switch d := time.Since(t); {
	case d < time.Minute:
		out += " (just now)"
	case d < time.Hour:
		out += fmt.Sprintf(" (%dm ago)", int(d.Minutes()))
	case d < 48*time.Hour:
		out += fmt.Sprintf(" (%dh ago)", int(d.Hours()))
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
