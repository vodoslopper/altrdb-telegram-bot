package notifier

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"altrdb-telegram-bot/internal/model"
)

func sampleEvent() model.TaskEvent {
	return model.TaskEvent{
		Fingerprint: "fp",
		TaskRepo:    "sisyphus",
		TaskID:      433219,
		State:       "DONE",
		Owner:       "glebfm",
		Try:         2,
		Iter:        1,
		Stage:       "task-save-repo",
		Changed:     time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC),
		Packages:    []string{"postfix 3.8.21-alt1"},
	}
}

// TestRenderMatchesTheDocumentedFormat checks the exact layout the product
// specification shows, after stripping the HTML used for styling.
func TestRenderMatchesTheDocumentedFormat(t *testing.T) {
	got := stripTags(Render(sampleEvent()))
	want := strings.Join([]string{
		"✅ Task #433219: DONE",
		"📦 postfix 3.8.21-alt1",
		"",
		"Branch: sisyphus",
		"Owner: @glebfm",
		"Try/iteration: 2/1",
		"Stage: task-save-repo",
		"Changed: 2026-09-16 13:56 UTC",
	}, "\n")
	if got != want {
		t.Errorf("rendered:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRenderShowsTheTransition(t *testing.T) {
	ev := sampleEvent()
	ev.PreviousState = "BUILDING"
	got := Render(ev)
	if !strings.Contains(got, "BUILDING → DONE") {
		t.Errorf("the transition is missing from %q", got)
	}

	ev.PreviousState = "DONE" // unchanged state, something else moved
	got = Render(ev)
	if strings.Contains(got, "DONE → DONE") {
		t.Errorf("a self-transition should not be rendered: %q", got)
	}
}

func TestRenderMarksTestonlyAndFirstObservation(t *testing.T) {
	ev := sampleEvent()
	ev.Testonly = 1
	ev.FirstObserved = true
	ev.PreviousState = ""
	got := Render(ev)
	if !strings.Contains(got, "[testonly]") {
		t.Errorf("testonly tasks must be marked: %q", got)
	}
	if strings.Contains(strings.SplitN(got, "\n", 2)[0], "[testonly]") {
		t.Errorf("testonly flag must not appear in the heading: %q", got)
	}
	if !strings.Contains(got, "Try/iteration: 2/1\nFlags: <code>[testonly]</code>\nStage:") {
		t.Errorf("testonly flag must appear directly below try/iteration: %q", got)
	}
}

func TestRenderOmitsEmptyFields(t *testing.T) {
	ev := sampleEvent()
	ev.Stage = ""
	ev.Packages = nil
	got := Render(ev)
	if strings.Contains(got, "Stage:") {
		t.Errorf("an empty stage should not be rendered: %q", got)
	}
	if strings.Contains(got, "📦") {
		t.Errorf("an empty package list should not be rendered: %q", got)
	}
}

func TestRenderEscapesEverything(t *testing.T) {
	ev := sampleEvent()
	ev.Owner = "evil<owner>&co"
	ev.State = "<script>alert(1)</script>"
	ev.TaskRepo = "a&b"
	ev.Stage = "stage<x>"
	ev.Message = "msg & <tag>"
	ev.Packages = []string{`pkg "<quoted>" & 1.0`, "other <b>2.0</b>"}

	got := Render(ev)
	for _, bad := range []string{"<script>", "<owner>", "<tag>", "<quoted>", "<b>2.0</b>", "stage<x>"} {
		if strings.Contains(got, bad) {
			t.Errorf("unescaped %q survived in:\n%s", bad, got)
		}
	}
	// The only tags left must be the ones the renderer wrote itself.
	for _, tag := range []string{"<b>", "</b>", "<code>", "</code>"} {
		if !strings.Contains(got, tag) {
			t.Errorf("the renderer's own %s tag disappeared:\n%s", tag, got)
		}
	}
	// The state is upper-cased for display before it is escaped.
	if !strings.Contains(got, "&lt;SCRIPT&gt;ALERT(1)&lt;/SCRIPT&gt;") {
		t.Errorf("the state was not entity-escaped:\n%s", got)
	}
}

func TestRenderCollapsesLongPackageLists(t *testing.T) {
	ev := sampleEvent()
	ev.Packages = make([]string, 0, 40)
	for range 40 {
		ev.Packages = append(ev.Packages, "kernel-modules-something-long 6.18.52-alt1")
	}
	got := Render(ev)
	if !strings.Contains(got, "+32 more") {
		t.Errorf("40 packages should collapse to 8 plus a counter:\n%s", got)
	}
	if utf8.RuneCountInString(got) > MaxTextLen {
		t.Errorf("rendered %d runes, over the Telegram limit", utf8.RuneCountInString(got))
	}
}

func TestRenderTruncatesALongMessage(t *testing.T) {
	ev := sampleEvent()
	ev.Message = strings.Repeat("very_long_girar_message_", 200)
	got := Render(ev)
	if utf8.RuneCountInString(got) > MaxTextLen {
		t.Errorf("rendered %d runes", utf8.RuneCountInString(got))
	}
	if !strings.Contains(got, "…") {
		t.Error("a truncated message must end with an ellipsis")
	}
	// Girar encodes spaces as underscores; the renderer makes them readable.
	if strings.Contains(got, "very_long_girar_message") && !strings.Contains(got, "very long girar message") {
		t.Errorf("underscores were not humanised:\n%s", got)
	}
}

func TestRenderNeverExceedsTheTelegramLimit(t *testing.T) {
	ev := sampleEvent()
	ev.Owner = strings.Repeat("o", 500)
	ev.TaskRepo = strings.Repeat("r", 500)
	ev.Stage = strings.Repeat("s", 500)
	ev.Message = strings.Repeat("m", 20000)
	ev.Packages = make([]string, 0, 200)
	for range 200 {
		ev.Packages = append(ev.Packages, strings.Repeat("p", 200)+" 1.0-alt1")
	}
	got := Render(ev)
	if n := utf8.RuneCountInString(got); n > MaxTextLen {
		t.Errorf("rendered %d runes, want at most %d", n, MaxTextLen)
	}
	if strings.Contains(got, "<") && !strings.Contains(got, "<b>") && !strings.Contains(got, "<code>") {
		t.Errorf("a hard truncation must not leave a stray tag:\n%.200s", got)
	}
}

func TestClampTextStripsTagsWhenTruncating(t *testing.T) {
	short := "<b>hello</b>"
	if got := ClampText(short); got != short {
		t.Errorf("a short string must pass through unchanged, got %q", got)
	}
	long := "<b>" + strings.Repeat("x", MaxTextLen+100) + "</b>"
	got := ClampText(long)
	if utf8.RuneCountInString(got) > MaxTextLen {
		t.Errorf("clamped length = %d", utf8.RuneCountInString(got))
	}
	if strings.Contains(got, "<b>") {
		t.Error("a truncated message must not contain markup, since the cut could land inside a tag")
	}
}

func TestEscapeHTML(t *testing.T) {
	tests := map[string]string{
		"":            "",
		"plain":       "plain",
		"a & b":       "a &amp; b",
		"<tag>":       "&lt;tag&gt;",
		"<<&>>":       "&lt;&lt;&amp;&gt;&gt;",
		"already&amp": "already&amp;amp",
		"emoji ✅":     "emoji ✅",
	}
	for in, want := range tests {
		if got := EscapeHTML(in); got != want {
			t.Errorf("EscapeHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than the cap", "hello", 10, "hello"},
		{"exactly the cap", "hello", 5, "hello"},
		{"over the cap", "hello world", 6, "hello…"},
		{"cap of one", "hello", 1, "…"},
		{"cap of zero keeps everything", "hello", 0, "hello"},
		{"counts runes not bytes", "✅✅✅✅", 3, "✅✅…"},
		{"cyrillic", "привет мир", 7, "привет…"},
		{"empty", "", 5, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateRunes(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if tc.max > 0 && utf8.RuneCountInString(got) > tc.max {
				t.Errorf("result is %d runes, over the cap of %d", utf8.RuneCountInString(got), tc.max)
			}
		})
	}
}

func TestStripTagsRestoresEntities(t *testing.T) {
	got := stripTags("<b>a &amp; b</b> &lt;c&gt;")
	if got != "a & b <c>" {
		t.Errorf("stripTags = %q", got)
	}
	if got := stripTags("half <b> tag"); got != "half  tag" {
		t.Errorf("an unterminated tag = %q", got)
	}
}

func TestStateEmojiCoversKnownStatesAndHasAFallback(t *testing.T) {
	for _, s := range model.KnownStates {
		if stateEmoji(s) == "" {
			t.Errorf("no emoji for the seeded state %s", s)
		}
	}
	if stateEmoji("SOMETHING_ENTIRELY_NEW") != "ℹ️" {
		t.Errorf("unknown state emoji = %q", stateEmoji("SOMETHING_ENTIRELY_NEW"))
	}
	if stateEmoji("done") != stateEmoji("DONE") {
		t.Error("the emoji lookup must be case-insensitive")
	}
}

func TestHumaniseMessage(t *testing.T) {
	tests := map[string]string{
		"yt6801_driver_has_become_in-tree_since_kernel": "yt6801 driver has become in-tree since kernel",
		"already has spaces":                            "already has spaces",
		"":                                              "",
		"single":                                        "single",
	}
	for in, want := range tests {
		if got := humaniseMessage(in); got != want {
			t.Errorf("humaniseMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatChanged(t *testing.T) {
	if got := formatChanged(time.Time{}); got != "unknown" {
		t.Errorf("a zero time renders as %q", got)
	}
	got := formatChanged(time.Date(2026, 9, 16, 13, 56, 30, 0, time.UTC))
	if got != "2026-09-16 13:56 UTC" {
		t.Errorf("formatChanged = %q", got)
	}
	// A non-UTC input must still render in UTC.
	moscow := time.FixedZone("MSK", 3*3600)
	got = formatChanged(time.Date(2026, 9, 16, 16, 56, 0, 0, moscow))
	if got != "2026-09-16 13:56 UTC" {
		t.Errorf("formatChanged with an offset = %q", got)
	}
}

func TestTaskURL(t *testing.T) {
	if got := TaskURL(433219); got != "https://git.altlinux.org/tasks/433219/" {
		t.Errorf("TaskURL = %q", got)
	}
}

func TestSampleEventUsesTheSavedSettings(t *testing.T) {
	ev := SampleEvent("rider", "p11", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if ev.Owner != "rider" || ev.TaskRepo != "p11" {
		t.Errorf("sample = %+v", ev)
	}
	if !ev.Changed.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("sample changed = %s", ev.Changed)
	}
	fallback := SampleEvent("", "", time.Now())
	if fallback.Owner == "" || fallback.TaskRepo == "" {
		t.Errorf("a sample with no settings must still render: %+v", fallback)
	}
	if Render(ev) == "" {
		t.Error("the sample must render")
	}
}

func TestPreviewOffIsShared(t *testing.T) {
	if PreviewOff() == nil || PreviewOff().IsDisabled == nil || !*PreviewOff().IsDisabled {
		t.Error("link previews must be disabled")
	}
	first, second := PreviewOff(), PreviewOff()
	if first != second {
		t.Error("the options value should be shared, not allocated per call")
	}
}
