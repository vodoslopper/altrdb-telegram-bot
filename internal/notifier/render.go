package notifier

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"altrdb-telegram-bot/internal/model"
)

// Telegram limits.
const (
	// MaxTextLen is the hard cap on message text Telegram accepts.
	MaxTextLen      = 4096
	maxPackageLines = 8
	maxPackageRunes = 400
	maxMessageRunes = 220
	maxStageRunes   = 80
)

// EscapeHTML escapes the three characters that matter in Telegram's HTML parse
// mode. Everything the API or a user supplies passes through it, so a task
// message containing "<3" cannot break the message or inject markup.
func EscapeHTML(s string) string {
	if s == "" {
		return ""
	}
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// TruncateRunes shortens s to at most max runes, appending an ellipsis when it
// actually cut something. It operates on plain text only: never call it on a
// string that already contains markup, or it can slice a tag in half.
//
// The count is in runes, not bytes, so a budget of 400 really means 400
// characters whatever the alphabet.
func TruncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	out := make([]rune, 0, max)
	for _, r := range s {
		if len(out) >= max-1 {
			break
		}
		out = append(out, r)
	}
	return string(append(out, '…'))
}

// stateEmoji maps a task state to the icon that leads the notification.
// Unknown states get a neutral marker rather than being dropped.
func stateEmoji(state string) string {
	switch strings.ToUpper(state) {
	case "DONE":
		return "✅"
	case "FAILED":
		return "❌"
	case "FAILING":
		return "⚠️"
	case "TESTED":
		return "🟢"
	case "EPERM":
		return "🔒"
	case "POSTPONED":
		return "⏸"
	case "AWAITING":
		return "⏳"
	case "BUILDING":
		return "🔨"
	case "COMMITTING":
		return "📦"
	case "NEW":
		return "🆕"
	default:
		return "ℹ️"
	}
}

// Render builds the notification text in Telegram HTML parse mode.
//
// The layout follows the format the product specifies:
//
//	✅ Task #433219: DONE
//	📦 postfix 3.8.21-alt1
//
//	Branch: sisyphus
//	Owner: @glebfm
//	Try/iteration: 2/1
//	Stage: task-save-repo
//	Changed: 2026-09-16 13:56 UTC
func Render(ev model.TaskEvent) string {
	head := fmt.Sprintf("%s <b>Task #%d: %s</b>",
		stateEmoji(ev.State), ev.TaskID, EscapeHTML(displayState(ev)))

	lines := []string{head}
	if pkg := renderPackages(ev.Packages); pkg != "" {
		lines = append(lines, "📦 <b>"+pkg+"</b>", "")
	}
	lines = append(lines, "Branch: <code>"+EscapeHTML(ev.TaskRepo)+"</code>")
	lines = append(lines, "Owner: "+EscapeHTML("@"+ev.Owner))
	lines = append(lines, fmt.Sprintf("Try/iteration: %d/%d", ev.Try, ev.Iter))
	if ev.Testonly != 0 {
		lines = append(lines, "Flags: <code>[testonly]</code>")
	}
	if ev.Stage != "" {
		lines = append(lines, "Stage: <code>"+EscapeHTML(TruncateRunes(ev.Stage, maxStageRunes))+"</code>")
	}
	if msg := strings.TrimSpace(ev.Message); msg != "" {
		lines = append(lines, "Message: <code>"+EscapeHTML(TruncateRunes(humaniseMessage(msg), maxMessageRunes))+"</code>")
	}
	lines = append(lines, "Changed: "+EscapeHTML(formatChanged(ev.Changed)))

	return clampHTML(strings.Join(lines, "\n"), MaxTextLen)
}

// displayState renders "FROM → TO" when the previous state is known, which is
// the useful part of a transition notification.
func displayState(ev model.TaskEvent) string {
	cur := strings.ToUpper(strings.TrimSpace(ev.State))
	if cur == "" {
		cur = "UNKNOWN"
	}
	prev := strings.ToUpper(strings.TrimSpace(ev.PreviousState))
	if prev == "" || prev == cur {
		return cur
	}
	return prev + " → " + cur
}

// renderPackages joins package lines with a budget, collapsing the tail into
// "+N more" instead of letting a 40-subtask kernel task eat the message.
func renderPackages(pkgs []string) string {
	if len(pkgs) == 0 {
		return ""
	}
	shown := pkgs
	rest := 0
	if len(shown) > maxPackageLines {
		rest = len(shown) - maxPackageLines
		shown = shown[:maxPackageLines]
	}

	clean := make([]string, 0, len(shown))
	for _, p := range shown {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	// Truncate the plain text first, escape afterwards: escaping first would
	// let the cut land inside an "&amp;" and produce a stray entity.
	out := strings.Join(clean, ", ")
	if rest > 0 {
		out += fmt.Sprintf(" … +%d more", rest)
	}
	return EscapeHTML(TruncateRunes(out, maxPackageRunes))
}

// humaniseMessage turns Girar's underscore-encoded task messages into something
// readable: "yt6801_driver_has_become_in-tree_since_x" -> "yt6801 driver has
// become in-tree since x". A message that already has spaces is left alone.
func humaniseMessage(msg string) string {
	if strings.Contains(msg, " ") {
		return msg
	}
	return strings.ReplaceAll(msg, "_", " ")
}

// formatChanged renders the timestamp the way the product spec shows it.
func formatChanged(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// clampHTML is the last line of defence: if the assembled message somehow
// exceeds the Telegram limit, drop the markup and hard-truncate plain text.
// Stripping tags first guarantees the result cannot contain a half-written tag.
func clampHTML(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	plain := stripTags(s)
	return TruncateRunes(plain, max)
}

func stripTags(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	// Entities were escaped for HTML; undo the three we produce so the plain
	// fallback stays readable.
	out := b.String()
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&amp;", "&")
	return out
}

// ClampText enforces Telegram's hard message limit. It is exported because the
// command layer builds HTML too and must never exceed the cap.
func ClampText(s string) string { return clampHTML(s, MaxTextLen) }

// TaskURL is the inline button target for a notification.
func TaskURL(taskID int64) string {
	return fmt.Sprintf("https://git.altlinux.org/tasks/%d/", taskID)
}

// SampleEvent builds a representative event for /test. It is clearly marked as
// a test so nobody mistakes it for a real transition.
func SampleEvent(owner string, branch string, now time.Time) model.TaskEvent {
	if owner == "" {
		owner = "grenka"
	}
	if branch == "" {
		branch = "sisyphus"
	}
	return model.TaskEvent{
		Fingerprint: "test",
		TaskRepo:    branch,
		TaskID:      433219,
		State:       "DONE",
		Owner:       owner,
		Try:         2,
		Iter:        1,
		Stage:       "task-save-repo",
		Message:     "test notification from /test",
		Changed:     now,
		Packages:    []string{"postfix 3.8.21-alt1"},
	}
}
