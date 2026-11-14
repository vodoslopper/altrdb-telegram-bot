package notifier

import (
	"strings"
	"testing"
)

func TestClampFallbackRemainsSafeForHTMLParseMode(t *testing.T) {
	text := "<b>&lt;package&gt; &amp; " + strings.Repeat("x", MaxTextLen) + "</b>"
	got := ClampText(text)
	if strings.Contains(got, "<package>") || !strings.HasPrefix(got, "&lt;package&gt; &amp;") {
		t.Fatalf("unsafe fallback: %.80s", got)
	}
	if len([]rune(stripTags(got))) > MaxTextLen {
		t.Fatal("fallback exceeds parsed message limit")
	}
	if twice := ClampText(got); twice != got {
		t.Fatal("clamping again changed escaped fallback")
	}
}
