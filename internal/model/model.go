// Package model holds the domain types shared by the store, the poller, the
// notifier and the Telegram layer. It has no dependencies on SQLite, HTTP or
// Telegram, which keeps it trivially testable.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TimeLayout is the single timestamp format used in the database and in
// fingerprints: RFC 3339, always UTC, always second precision or better.
const TimeLayout = time.RFC3339

// FormatTime renders t as an RFC 3339 UTC string.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime reads an RFC 3339 timestamp. Values produced by the RDB API carry
// a "+00:00" offset; everything is normalised to UTC.
func ParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		// RDB has been observed to emit space-separated SQL style values.
		if t2, err2 := time.Parse("2006-01-02 15:04:05", s); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// NotifyMode selects how much of a task's life a chat wants to hear about.
type NotifyMode string

const (
	// NotifyAll delivers every matching transition.
	NotifyAll NotifyMode = "all"
	// NotifyTerminal delivers only states that end a task's life.
	NotifyTerminal NotifyMode = "terminal"
)

// Valid reports whether m is a stored mode.
func (m NotifyMode) Valid() bool { return m == NotifyAll || m == NotifyTerminal }

// DefaultTerminalStates are the states treated as final in "terminal" notify
// mode. Everything else in the Girar lifecycle is transient: TESTED still
// becomes DONE, EPERM waits for an approval, POSTPONED is re-queued.
// Override with TERMINAL_STATES.
var DefaultTerminalStates = []string{"DONE", "FAILED"}

// KnownStates are the task states seen in the live feed while this program was
// designed. They seed the selector UI in lifecycle order. The list is not
// authoritative: any other non-empty state the API returns is persisted and
// offered too.
var KnownStates = []string{
	"NEW",
	"BUILDING",
	"AWAITING",
	"POSTPONED",
	"EPERM",
	"TESTED",
	"FAILING",
	"FAILED",
	"DONE",
}

// StateRank returns the lifecycle position of a known state (10, 20, ...).
// Unknown states get 1000 and are therefore sorted alphabetically after the
// known ones.
func StateRank(state string) int {
	for i, s := range KnownStates {
		if s == state {
			return (i + 1) * 10
		}
	}
	return 1000
}

// SortStates orders states by lifecycle position, unknown ones alphabetically
// at the end.
func SortStates(states []string) {
	sort.SliceStable(states, func(i, j int) bool {
		ri, rj := StateRank(states[i]), StateRank(states[j])
		if ri != rj {
			return ri < rj
		}
		return states[i] < states[j]
	})
}

// TerminalStates is the resolved set used by notify mode "terminal".
type TerminalStates map[string]struct{}

// NewTerminalStates builds the set from a list, ignoring blanks.
func NewTerminalStates(states []string) TerminalStates {
	out := make(TerminalStates, len(states))
	for _, s := range states {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

// Contains reports whether state is terminal.
func (t TerminalStates) Contains(state string) bool {
	_, ok := t[state]
	return ok
}

// Chat is one Telegram chat and its subscription switches.
type Chat struct {
	ID             int64
	Type           string // private | group | supergroup | channel
	Username       string
	FirstName      string
	Enabled        bool
	NotifyMode     NotifyMode
	DisabledReason string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// BranchSubscription is one checked branch plus its activation watermark.
//
// The watermark is what stops "I just ticked sisyphus" from replaying every
// task currently sitting in the feed: only records whose task_changed is newer
// than the watermark are notified.
type BranchSubscription struct {
	Branch          string
	ActiveWatermark time.Time
	CreatedAt       time.Time
}

// Subscription is the whole editable selection of one chat.
type Subscription struct {
	Maintainers []string
	States      []string
	Branches    []BranchSubscription
}

// HasBranch reports whether branch is checked.
func (s Subscription) HasBranch(branch string) bool {
	for _, b := range s.Branches {
		if b.Branch == branch {
			return true
		}
	}
	return false
}

// BranchNames lists the checked branches.
func (s Subscription) BranchNames() []string {
	out := make([]string, 0, len(s.Branches))
	for _, b := range s.Branches {
		out = append(out, b.Branch)
	}
	return out
}

// Complete reports whether the subscription can match anything at all.
func (s Subscription) Complete() bool {
	return len(s.Maintainers) > 0 && len(s.States) > 0 && len(s.Branches) > 0
}

// Missing returns the human readable list of what is still unset.
func (s Subscription) Missing() []string {
	var missing []string
	if len(s.Maintainers) == 0 {
		missing = append(missing, "maintainers (/maintainers)")
	}
	if len(s.States) == 0 {
		missing = append(missing, "states (/states)")
	}
	if len(s.Branches) == 0 {
		missing = append(missing, "branches (/branches)")
	}
	return missing
}

// Task is the normalised view of one RDB task record.
type Task struct {
	TaskID    int64
	TaskRepo  string
	State     string
	Owner     string
	Try       int
	Iter      int
	Testonly  int
	Stage     string
	Message   string
	Changed   time.Time
	Packages  []string
	ChangedAt string // raw timestamp as the API returned it
}

// Fingerprint is the deterministic identity of a task snapshot. Two records
// with the same fingerprint are the same event; a different fingerprint means
// something meaningful moved.
//
// The field list is part of the on-disk contract: changing it makes every
// stored snapshot look "changed" once, so bump migrations.MetaFingerprintVersion
// when you do.
func Fingerprint(t Task) string {
	parts := []string{
		strconv.FormatInt(t.TaskID, 10),
		t.TaskRepo,
		t.Owner,
		strconv.Itoa(t.Try),
		strconv.Itoa(t.Iter),
		t.State,
		t.Stage,
		FormatTime(t.Changed),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// FingerprintShort is a compact form for logs and callback payloads.
func FingerprintShort(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// Snapshot is the persisted latest state of a (task_repo, task_id) pair.
type Snapshot struct {
	TaskRepo    string
	TaskID      int64
	Fingerprint string
	State       string
	Owner       string
	Try         int
	Iter        int
	Testonly    int
	Stage       string
	Message     string
	Changed     time.Time
	Packages    []string
	FirstSeenAt time.Time
	UpdatedAt   time.Time
}

// TaskEvent is one notification: a task transition plus what it looked like
// before. It is serialised verbatim into the outbox payload column, so the
// delivery worker never has to re-read the task feed.
type TaskEvent struct {
	Fingerprint   string    `json:"fingerprint"`
	TaskRepo      string    `json:"task_repo"`
	TaskID        int64     `json:"task_id"`
	State         string    `json:"state"`
	PreviousState string    `json:"previous_state,omitempty"`
	Owner         string    `json:"owner"`
	Try           int       `json:"try"`
	Iter          int       `json:"iter"`
	Testonly      int       `json:"testonly,omitempty"`
	Stage         string    `json:"stage,omitempty"`
	Message       string    `json:"message,omitempty"`
	Changed       time.Time `json:"changed"`
	Packages      []string  `json:"packages,omitempty"`
	FirstObserved bool      `json:"first_observed,omitempty"`
}

// TaskURL is the human link shown as an inline button.
func (e TaskEvent) TaskURL() string {
	return fmt.Sprintf("https://git.altlinux.org/tasks/%d/", e.TaskID)
}

// OutboxStatus is the lifecycle of a notification row.
type OutboxStatus string

const (
	OutboxPending OutboxStatus = "pending"
	OutboxSending OutboxStatus = "sending"
	OutboxSent    OutboxStatus = "sent"
	OutboxDead    OutboxStatus = "dead"
)

// Notification is one outbox row.
type Notification struct {
	ID            int64
	ChatID        int64
	Fingerprint   string
	TaskRepo      string
	TaskID        int64
	Event         TaskEvent
	Status        OutboxStatus
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	SentAt        *time.Time
}

// ConversationStep names a persisted multi-step command state.
type ConversationStep string

const (
	// StepNone means no pending input.
	StepNone ConversationStep = ""
	// StepAwaitingMaintainers waits for a list of nicknames to add.
	StepAwaitingMaintainers ConversationStep = "awaiting_maintainers"
	// StepSuggestions holds the offer list behind a "did you mean" panel, so
	// its buttons can be resolved back to a nickname safely.
	StepSuggestions ConversationStep = "suggestions"
)

// Conversation is the persisted state of a chat mid-command.
type Conversation struct {
	ChatID    int64
	Step      ConversationStep
	Payload   string
	UpdatedAt time.Time
}

// Maintainer is one entry of the cached RDB maintainer directory.
type Maintainer struct {
	Branch         string
	Nickname       string
	PackagerName   string
	CountSourcePkg int
	FetchedAt      time.Time
}

// Branch is one entry of the cached RDB branch list.
type Branch struct {
	ID        int64
	Name      string
	SortOrder int
	FirstSeen time.Time
	LastSeen  time.Time
}

// State is one entry of the observed task state catalogue.
type State struct {
	ID        int64
	Name      string
	Known     bool
	SortRank  int
	FirstSeen time.Time
	LastSeen  time.Time
}

// PollState is the per-branch polling health record. It is embedded verbatim in
// the /healthz document, so the field names are part of the operator contract.
type PollState struct {
	Branch              string     `json:"branch"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	TasksSeen           int        `json:"tasks_seen"`
	OldestTaskChanged   *time.Time `json:"oldest_task_changed,omitempty"`
	WindowSaturated     bool       `json:"window_saturated"`
	Baselined           bool       `json:"baselined"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// Health is the payload of GET /healthz.
type Health struct {
	Status        string         `json:"status"` // ok | degraded
	Version       string         `json:"version"`
	Commit        string         `json:"commit,omitempty"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	Database      DatabaseHealth `json:"database"`
	Telegram      TelegramHealth `json:"telegram"`
	Poller        PollerHealth   `json:"poller"`
	Outbox        OutboxHealth   `json:"outbox"`
	Chats         ChatsHealth    `json:"chats"`
	Extra         map[string]any `json:"extra,omitempty"`
}

// DatabaseHealth reports store reachability and migration level.
type DatabaseHealth struct {
	OK            bool   `json:"ok"`
	Driver        string `json:"driver"`
	SchemaVersion int    `json:"schema_version"`
	Error         string `json:"error,omitempty"`
}

// TelegramHealth reports the update loop.
type TelegramHealth struct {
	Mode         string `json:"mode"` // polling | webhook
	Running      bool   `json:"running"`
	LastUpdateID int64  `json:"last_update_id"`
	LastUpdateAt string `json:"last_update_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	Error        string `json:"error,omitempty"`
}

// PollerHealth reports one entry per branch that has been polled.
type PollerHealth struct {
	IntervalSeconds int         `json:"interval_seconds"`
	TasksLimit      int         `json:"tasks_limit"`
	Running         bool        `json:"running"`
	Branches        []PollState `json:"branches"`
	WindowSaturated bool        `json:"window_saturated"`
}

// OutboxHealth reports delivery backlog.
type OutboxHealth struct {
	Pending   int   `json:"pending"`
	Sending   int   `json:"sending"`
	Dead      int   `json:"dead"`
	OldestAge int64 `json:"oldest_pending_age_seconds"`
}

// ChatsHealth reports subscription counts.
type ChatsHealth struct {
	Total    int `json:"total"`
	Enabled  int `json:"enabled"`
	Complete int `json:"complete"`
}
