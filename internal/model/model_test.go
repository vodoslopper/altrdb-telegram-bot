package model

import (
	"strings"
	"testing"
	"time"
)

func TestFingerprintIsDeterministicAndFieldSensitive(t *testing.T) {
	base := Task{
		TaskID:   433219,
		TaskRepo: "sisyphus",
		Owner:    "glebfm",
		Try:      2,
		Iter:     1,
		State:    "DONE",
		Stage:    "task-save-repo",
		Changed:  time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC),
	}

	first, second := Fingerprint(base), Fingerprint(base)
	if first != second {
		t.Fatalf("fingerprint is not deterministic: %q != %q", first, second)
	}
	if len(first) != 64 {
		t.Errorf("fingerprint length = %d, want a sha256 hex digest", len(first))
	}

	// Fields that must change the fingerprint.
	mutations := map[string]func(*Task){
		"task_id":   func(x *Task) { x.TaskID++ },
		"task_repo": func(x *Task) { x.TaskRepo = "p11" },
		"owner":     func(x *Task) { x.Owner = "rider" },
		"try":       func(x *Task) { x.Try++ },
		"iter":      func(x *Task) { x.Iter++ },
		"state":     func(x *Task) { x.State = "FAILED" },
		"stage":     func(x *Task) { x.Stage = "task-build" },
		"changed":   func(x *Task) { x.Changed = x.Changed.Add(time.Second) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			modified := base
			mutate(&modified)
			if Fingerprint(modified) == Fingerprint(base) {
				t.Errorf("changing %s did not change the fingerprint", name)
			}
		})
	}

	// Fields that must NOT change it: they are presentation details, and
	// including them would notify twice for the same transition.
	stable := map[string]func(*Task){
		"message":   func(x *Task) { x.Message = "something new" },
		"packages":  func(x *Task) { x.Packages = []string{"postfix 3.8.21-alt1"} },
		"testonly":  func(x *Task) { x.Testonly = 1 },
		"changedat": func(x *Task) { x.ChangedAt = "raw string" },
	}
	for name, mutate := range stable {
		t.Run("stable_"+name, func(t *testing.T) {
			modified := base
			mutate(&modified)
			if Fingerprint(modified) != Fingerprint(base) {
				t.Errorf("changing %s changed the fingerprint; it must not", name)
			}
		})
	}
}

func TestFingerprintNormalisesTimezone(t *testing.T) {
	utc := Task{TaskID: 1, TaskRepo: "sisyphus", Changed: time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC)}
	moscow := time.FixedZone("MSK", 3*3600)
	same := utc
	same.Changed = time.Date(2026, 9, 16, 16, 56, 0, 0, moscow)

	if Fingerprint(utc) != Fingerprint(same) {
		t.Error("the same instant in a different zone must produce the same fingerprint")
	}
}

func TestParseTimeFormats(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Time
		wantErr bool
	}{
		{"api offset form", "2026-09-16T13:56:00+00:00", time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC), false},
		{"zulu form", "2026-09-16T13:56:00Z", time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC), false},
		{"non-utc offset is normalised", "2026-09-16T16:56:00+03:00", time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC), false},
		{"space separated sql form", "2026-09-16 13:56:00", time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC), false},
		{"fractional seconds", "2026-09-16T13:56:00.5Z", time.Date(2026, 9, 16, 13, 56, 0, 500000000, time.UTC), false},
		{"empty", "", time.Time{}, true},
		{"garbage", "not a time", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTime(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTime(%q) succeeded, want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTime(%q): %v", tc.in, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("ParseTime(%q) = %s, want %s", tc.in, got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("ParseTime(%q) is not UTC: %v", tc.in, got.Location())
			}
		})
	}
}

func TestFormatTimeRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 16, 13, 56, 7, 0, time.UTC)
	s := FormatTime(now)
	if !strings.HasSuffix(s, "Z") {
		t.Errorf("FormatTime should emit UTC, got %q", s)
	}
	back, err := ParseTime(s)
	if err != nil {
		t.Fatalf("ParseTime: %v", err)
	}
	if !back.Equal(now) {
		t.Errorf("round trip = %s, want %s", back, now)
	}
}

func TestSortStatesLifecycleThenAlphabetical(t *testing.T) {
	in := []string{"ZZZ_NEW_STATE", "DONE", "abc", "NEW", "FAILED", "TESTED", "BUILDING", "COMMITTING"}
	SortStates(in)

	got := strings.Join(in, ",")
	// Known states first in lifecycle order, unknown ones alphabetically after.
	wantKnown := []string{"NEW", "BUILDING", "TESTED", "FAILED", "DONE"}
	pos := 0
	for _, w := range wantKnown {
		idx := indexOfString(in, w)
		if idx < pos {
			t.Errorf("%s appeared out of lifecycle order in %s", w, got)
		}
		pos = idx
	}
	// COMMITTING, ZZZ_NEW_STATE and abc are all unknown: alphabetical at the end.
	tail := in[len(in)-3:]
	if strings.Join(tail, ",") != "COMMITTING,ZZZ_NEW_STATE,abc" {
		t.Errorf("unknown states = %v, want alphabetical after the known ones", tail)
	}
}

func indexOfString(items []string, want string) int {
	for i, v := range items {
		if v == want {
			return i
		}
	}
	return -1
}

func TestStateRank(t *testing.T) {
	if StateRank("NEW") >= StateRank("DONE") {
		t.Error("NEW must rank before DONE")
	}
	if StateRank("SOMETHING_NEW") != 1000 {
		t.Errorf("unknown state rank = %d, want 1000", StateRank("SOMETHING_NEW"))
	}
}

func TestTerminalStates(t *testing.T) {
	ts := NewTerminalStates([]string{"done", " FAILED ", ""})
	if !ts.Contains("DONE") || !ts.Contains("FAILED") {
		t.Errorf("terminal set = %v", ts)
	}
	if ts.Contains("TESTED") {
		t.Error("TESTED must not be terminal by default")
	}
	if ts.Contains("") {
		t.Error("blank entries must be dropped")
	}
}

func TestNotifyModeValid(t *testing.T) {
	if !NotifyAll.Valid() || !NotifyTerminal.Valid() {
		t.Error("both documented modes must be valid")
	}
	if NotifyMode("sometimes").Valid() {
		t.Error("an invented mode must not validate")
	}
}

func TestSubscriptionCompleteness(t *testing.T) {
	sub := Subscription{}
	if sub.Complete() {
		t.Error("an empty subscription is not complete")
	}
	missing := sub.Missing()
	if len(missing) != 3 {
		t.Errorf("Missing() = %v, want three entries", missing)
	}

	sub.Maintainers = []string{"grenka"}
	sub.States = []string{"DONE"}
	sub.Branches = []BranchSubscription{{Branch: "sisyphus"}}
	if !sub.Complete() {
		t.Error("a fully populated subscription must be complete")
	}
	if len(sub.Missing()) != 0 {
		t.Errorf("Missing() = %v, want none", sub.Missing())
	}
	if !sub.HasBranch("sisyphus") || sub.HasBranch("p11") {
		t.Errorf("HasBranch is wrong for %v", sub.BranchNames())
	}
}

func TestTaskEventURL(t *testing.T) {
	ev := TaskEvent{TaskID: 433219}
	if got, want := ev.TaskURL(), "https://git.altlinux.org/tasks/433219/"; got != want {
		t.Errorf("TaskURL() = %q, want %q", got, want)
	}
}

func TestFingerprintShort(t *testing.T) {
	fp := Fingerprint(Task{TaskID: 1, TaskRepo: "sisyphus"})
	if got := FingerprintShort(fp); len(got) != 12 || !strings.HasPrefix(fp, got) {
		t.Errorf("FingerprintShort(%q) = %q", fp, got)
	}
	if got := FingerprintShort("abc"); got != "abc" {
		t.Errorf("short fingerprints must pass through, got %q", got)
	}
}
