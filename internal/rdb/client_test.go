package rdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/model"
)

// testClient builds a client pointed at an httptest server with fast,
// deterministic backoff.
func testClient(t *testing.T, srv *httptest.Server, mutate func(*Options)) *Client {
	t.Helper()
	opts := Options{
		BaseURL:           srv.URL,
		UserAgent:         "altrdb-telegram-bot/test",
		HTTPClient:        srv.Client(),
		MaxRetries:        3,
		BackoffBase:       time.Millisecond,
		BackoffMax:        4 * time.Millisecond,
		MinRequestSpacing: 0,
		Jitter:            func() float64 { return 0.5 },
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:             clock.NewFake(time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return NewClient(opts)
}

func jsonHandler(t *testing.T, status int, body any) http.HandlerFunc {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(encoded)
	}
}

func sampleTasksResponse(branch string, ids ...int64) TasksListResponse {
	tasks := make([]TaskElement, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, TaskElement{
			TaskID:      id,
			TaskRepo:    branch,
			TaskState:   "DONE",
			TaskOwner:   "glebfm",
			TaskTry:     1,
			TaskIter:    1,
			TaskChanged: "2026-09-16T13:56:00+00:00",
			TaskStage:   "task-save-repo",
			Subtasks: []Subtask{{
				SubtaskSrpmName: "postfix",
				SubtaskSrpmEvr:  "3.8.21-alt1",
			}},
		})
	}
	return TasksListResponse{
		RequestArgs: map[string]any{"branch": branch},
		Length:      len(tasks),
		Tasks:       tasks,
	}
}

func TestBranches(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK,
		BranchesResponse{Length: 3, Branches: []string{"sisyphus", "p11", " p10 "}}))
	defer srv.Close()

	c := testClient(t, srv, nil)
	got, err := c.Branches(context.Background())
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if strings.Join(got, ",") != "sisyphus,p11,p10" {
		t.Errorf("branches = %v (blanks must be trimmed and dropped)", got)
	}
}

func TestBranchesRejectsAnEmptyList(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, BranchesResponse{Length: 0}))
	defer srv.Close()

	if _, err := testClient(t, srv, nil).Branches(context.Background()); err == nil {
		t.Fatal("an empty branch list must be an error, not a silently emptied selector")
	}
}

func TestBranchesMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"length": "many", "branches": `)
	}))
	defer srv.Close()

	_, err := testClient(t, srv, nil).Branches(context.Background())
	if err == nil {
		t.Fatal("truncated JSON must produce an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("error = %T, want *APIError", err)
	}
}

func TestBranchesTolerantOfExtraFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"length":1,"branches":["sisyphus"],
			"brand_new_field":{"nested":[1,2,3]},"another":null}`)
	}))
	defer srv.Close()

	got, err := testClient(t, srv, nil).Branches(context.Background())
	if err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}
	if len(got) != 1 || got[0] != "sisyphus" {
		t.Errorf("branches = %v", got)
	}
}

func TestLastTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, PathLastTasks; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		q := r.URL.Query()
		if q.Get("branch") != "sisyphus" {
			t.Errorf("branch = %q", q.Get("branch"))
		}
		if q.Get("tasks_limit") != "200" {
			t.Errorf("tasks_limit = %q", q.Get("tasks_limit"))
		}
		jsonHandler(t, http.StatusOK, sampleTasksResponse("sisyphus", 433219, 433220))(w, r)
	}))
	defer srv.Close()

	c := testClient(t, srv, nil)
	resp, tasks, skipped, err := c.LastTasks(context.Background(), "sisyphus", 200)
	if err != nil {
		t.Fatalf("LastTasks: %v", err)
	}
	if resp.Length != 2 || len(tasks) != 2 || skipped != 0 {
		t.Fatalf("resp.Length=%d tasks=%d skipped=%d", resp.Length, len(tasks), skipped)
	}
	got := tasks[0]
	if got.TaskID != 433219 || got.TaskRepo != "sisyphus" || got.State != "DONE" {
		t.Errorf("task = %+v", got)
	}
	if got.Owner != "glebfm" {
		t.Errorf("owner = %q", got.Owner)
	}
	if !got.Changed.Equal(time.Date(2026, 9, 16, 13, 56, 0, 0, time.UTC)) {
		t.Errorf("changed = %s", got.Changed)
	}
	if len(got.Packages) != 1 || got.Packages[0] != "postfix 3.8.21-alt1" {
		t.Errorf("packages = %v", got.Packages)
	}
	if got.ChangedAt != "2026-09-16T13:56:00+00:00" {
		t.Errorf("raw timestamp = %q", got.ChangedAt)
	}
}

func TestLastTasksSkipsMalformedRecords(t *testing.T) {
	resp := sampleTasksResponse("sisyphus", 1)
	resp.Tasks = append(resp.Tasks,
		// Unparsable timestamp: the watermark comparison depends on it.
		TaskElement{TaskID: 2, TaskRepo: "sisyphus", TaskState: "DONE", TaskChanged: "yesterday"},
		// Non-positive id.
		TaskElement{TaskID: 0, TaskRepo: "sisyphus", TaskState: "DONE", TaskChanged: "2026-09-16T13:56:00+00:00"},
		// Missing repo, but a branch was requested, so it is recoverable.
		TaskElement{TaskID: 3, TaskState: "DONE", TaskChanged: "2026-09-16T13:56:00+00:00"},
	)
	resp.Length = len(resp.Tasks)

	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, resp))
	defer srv.Close()

	_, tasks, skipped, err := testClient(t, srv, nil).LastTasks(context.Background(), "sisyphus", 10)
	if err != nil {
		t.Fatalf("LastTasks: %v", err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 (bad timestamp and bad id)", skipped)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	for _, task := range tasks {
		if task.TaskRepo != "sisyphus" {
			t.Errorf("task %d repo = %q, want the requested branch as a fallback", task.TaskID, task.TaskRepo)
		}
	}
}

func TestLastTasksKeepsUnknownStates(t *testing.T) {
	resp := sampleTasksResponse("sisyphus", 1)
	resp.Tasks[0].TaskState = "COMMITTING" // a state the binary was not built with
	resp.Tasks = append(resp.Tasks, TaskElement{
		TaskID: 2, TaskRepo: "sisyphus", TaskState: "   ",
		TaskChanged: "2026-09-16T13:56:00+00:00",
	})
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, resp))
	defer srv.Close()

	_, tasks, _, err := testClient(t, srv, nil).LastTasks(context.Background(), "sisyphus", 10)
	if err != nil {
		t.Fatalf("LastTasks: %v", err)
	}
	if tasks[0].State != "COMMITTING" {
		t.Errorf("unknown state was mangled: %q", tasks[0].State)
	}
	if tasks[1].State != "UNKNOWN" {
		t.Errorf("blank state = %q, want UNKNOWN", tasks[1].State)
	}
	if got := StatesOf(&resp); len(got) != 1 || got[0] != "COMMITTING" {
		t.Errorf("StatesOf = %v, want only the non-empty state", got)
	}
}

func TestLastTasksRejectsAnEmptyBranch(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, TasksListResponse{}))
	defer srv.Close()
	if _, _, _, err := testClient(t, srv, nil).LastTasks(context.Background(), "", 10); err == nil {
		t.Fatal("an empty branch must be rejected before any request is made")
	}
}

func TestRetriesOnServerErrorThenSucceeds(t *testing.T) {
	var attemptCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts := int(attemptCount.Add(1))
		if attempts < 3 {
			http.Error(w, `{"message":"boom"}`, http.StatusServiceUnavailable)
			return
		}
		jsonHandler(t, http.StatusOK, BranchesResponse{Length: 1, Branches: []string{"sisyphus"}})(w, r)
	}))
	defer srv.Close()

	c := testClient(t, srv, nil)
	got, err := c.Branches(context.Background())
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("branches = %v", got)
	}
	if got := attemptCount.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if st := c.Snapshot(); st.Retries != 2 || st.Successes != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var attemptCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attemptCount.Add(1)
		http.Error(w, `{"message":"still broken"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := testClient(t, srv, func(o *Options) { o.MaxRetries = 2 })
	_, err := c.Branches(context.Background())
	if err == nil {
		t.Fatal("expected a failure after exhausting retries")
	}
	if got := attemptCount.Load(); got != 3 {
		t.Errorf("attempts = %d, want 1 initial + 2 retries", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("error = %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "still broken") {
		t.Errorf("the API's own message should surface, got %q", err.Error())
	}
}

func TestRateLimitHonoursRetryAfter(t *testing.T) {
	var attemptCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts := int(attemptCount.Add(1))
		if attempts == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		jsonHandler(t, http.StatusOK, BranchesResponse{Length: 1, Branches: []string{"p11"}})(w, r)
	}))
	defer srv.Close()

	// backoffMax also caps how far an explicit Retry-After is honoured (4x),
	// so it has to be larger than the delay under test.
	c := testClient(t, srv, func(o *Options) { o.BackoffMax = 2 * time.Second })
	start := time.Now()
	got, err := c.Branches(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(got) != 1 || got[0] != "p11" {
		t.Errorf("branches = %v", got)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("waited %s; Retry-After: 1 must be honoured", elapsed)
	}
	if st := c.Snapshot(); st.RateLimited != 1 {
		t.Errorf("rate limited counter = %d", st.RateLimited)
	}
}

func TestBackoffPrefersRetryAfterAndCapsExponential(t *testing.T) {
	c := &Client{
		backoffBase: time.Second,
		backoffMax:  10 * time.Second,
		jitter:      func() float64 { return 0 }, // deterministic: always the lower half
	}

	if got := c.backoff(0, 3*time.Second); got != 3*time.Second {
		t.Errorf("explicit Retry-After must win, got %s", got)
	}
	// Equal jitter: half deterministic plus half random. With jitter pinned to
	// 0 the delay is exactly half the exponential value.
	if got := c.backoff(0, 0); got != 500*time.Millisecond {
		t.Errorf("attempt 0 = %s, want 500ms", got)
	}
	if got := c.backoff(1, 0); got != time.Second {
		t.Errorf("attempt 1 = %s, want 1s", got)
	}
	if got := c.backoff(20, 0); got != 5*time.Second {
		t.Errorf("attempt 20 = %s, want half of the 10s cap", got)
	}
	// A Retry-After far beyond the cap is still bounded.
	if got := c.backoff(0, time.Hour); got != 40*time.Second {
		t.Errorf("an hour of Retry-After = %s, want the 4x cap of 40s", got)
	}

	// With jitter at 1.0 the delay reaches the full exponential value.
	c.jitter = func() float64 { return 1 }
	if got := c.backoff(0, 0); got != time.Second {
		t.Errorf("full jitter at attempt 0 = %s, want 1s", got)
	}
	if got := c.backoff(20, 0); got != 10*time.Second {
		t.Errorf("full jitter at attempt 20 = %s, want the 10s cap", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"nonsense", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
	}
	for _, tc := range tests {
		if got := parseRetryAfter(tc.in, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	c := testClient(t, srv, func(o *Options) {
		o.HTTPClient = &http.Client{Timeout: 40 * time.Millisecond}
		o.MaxRetries = 1
	})
	_, err := c.Branches(context.Background())
	if err == nil {
		t.Fatal("a hung server must produce a timeout error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T, want *APIError", err)
	}
	if apiErr.StatusCode != 0 {
		t.Errorf("status = %d, want 0 for a transport failure", apiErr.StatusCode)
	}
	if !apiErr.Retryable() {
		t.Error("a timeout must be retryable")
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	var attemptCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attemptCount.Add(1)
		http.Error(w, `{}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := testClient(t, srv, func(o *Options) {
		o.MaxRetries = 5
		o.BackoffBase = 50 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := c.Branches(ctx); err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if got := attemptCount.Load(); got >= 6 {
		t.Errorf("attempts = %d; cancellation must stop the retry loop", got)
	}
}

func TestSendsUserAgentAndAccept(t *testing.T) {
	var ua, accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		accept = r.Header.Get("Accept")
		jsonHandler(t, http.StatusOK, BranchesResponse{Length: 1, Branches: []string{"p11"}})(w, r)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv, nil).Branches(context.Background()); err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if ua != "altrdb-telegram-bot/test" {
		t.Errorf("User-Agent = %q", ua)
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q", accept)
	}
}

func TestResponseSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"length":1,"branches":["`+strings.Repeat("x", 4096)+`"]}`)
	}))
	defer srv.Close()

	c := testClient(t, srv, func(o *Options) { o.MaxResponseBytes = 128 })
	_, err := c.Branches(context.Background())
	if err == nil {
		t.Fatal("an oversized body must be rejected instead of decoded")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T, want *APIError", err)
	}
}

func TestTrailingGarbageAfterACompleteDocument(t *testing.T) {
	// A complete JSON value followed by more bytes means the body was not what
	// the server claimed; refusing it is cheaper than acting on half a feed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"length":1,"branches":["p11"]}{"length":2`)
	}))
	defer srv.Close()

	_, err := testClient(t, srv, nil).Branches(context.Background())
	if err == nil {
		t.Fatal("trailing bytes after a complete document must be rejected")
	}
	if !strings.Contains(err.Error(), "larger than") && !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error = %v", err)
	}
}

func TestTaskInfoNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathTaskInfo+"/433219" {
			t.Errorf("path = %q", r.URL.Path)
		}
		http.Error(w, `{"message":"Requested data not found in database"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := testClient(t, srv, nil).TaskInfo(context.Background(), 433219)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Retryable() {
		t.Error("a 404 must not be retried")
	}
}

func TestTaskInfoSuccess(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, TaskInfoResponse{
		TaskID: 433219, TaskRepo: "sisyphus", TaskState: "EPERM", TaskOwner: "glebfm",
		TaskTry: 2, TaskIter: 1, TaskChanged: "2026-09-16T13:56:00+00:00",
		Approval: []Approval{{Type: "approve", Nickname: "rider"}, {Type: "approve", Nickname: ""}},
		Subtasks: []Subtask{{SubtaskSrpmName: "postfix", SubtaskSrpmEvr: "3.8.21-alt1"}},
	}))
	defer srv.Close()

	info, err := testClient(t, srv, nil).TaskInfo(context.Background(), 433219)
	if err != nil {
		t.Fatalf("TaskInfo: %v", err)
	}
	if info.TaskState != "EPERM" {
		t.Errorf("state = %q", info.TaskState)
	}
	got := ApprovalSummary(info.Approval, 8)
	if len(got) != 1 || got[0] != "approve: rider" {
		t.Errorf("approvals = %v", got)
	}
}

func TestMaintainers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathMaintainers {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("branch") != "sisyphus" {
			t.Errorf("branch = %q", r.URL.Query().Get("branch"))
		}
		jsonHandler(t, http.StatusOK, MaintainersResponse{
			Length: 3,
			Maintainers: []MaintainerEntry{
				{PackagerName: "Georgy Gagarin", PackagerNickname: "Grenka", CountSourcePkg: 42},
				{PackagerName: "Empty Nick", PackagerNickname: "  ", CountSourcePkg: 1},
				{PackagerName: "Gleb Fotyev", PackagerNickname: "glebfm", CountSourcePkg: 300},
			},
		})(w, r)
	}))
	defer srv.Close()

	got, err := testClient(t, srv, nil).Maintainers(context.Background(), "sisyphus")
	if err != nil {
		t.Fatalf("Maintainers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("maintainers = %d, want the blank nickname dropped", len(got))
	}
	if got[0].Nickname != "grenka" || got[0].PackagerName != "Georgy Gagarin" || got[0].CountSourcePkg != 42 {
		t.Errorf("maintainer = %+v", got[0])
	}
	if got[0].Branch != "sisyphus" {
		t.Errorf("branch = %q", got[0].Branch)
	}
	if got[0].FetchedAt.IsZero() {
		t.Error("fetched_at must be stamped")
	}
}

func TestMaintainersRejectsAnEmptyDirectory(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, MaintainersResponse{Length: 0}))
	defer srv.Close()
	if _, err := testClient(t, srv, nil).Maintainers(context.Background(), "p11"); err == nil {
		t.Fatal("an empty directory must be an error so the cache is not wiped")
	}
}

func TestFindTasksSendsFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("input") == "" {
			t.Error("input is required by the API and must always be sent")
		}
		if q.Get("owner") != "glebfm" || q.Get("branch") != "sisyphus" {
			t.Errorf("filters = owner=%q branch=%q", q.Get("owner"), q.Get("branch"))
		}
		if got := q["state"]; len(got) != 2 || got[0] != "DONE" || got[1] != "FAILED" {
			t.Errorf("state = %v", got)
		}
		jsonHandler(t, http.StatusOK, sampleTasksResponse("sisyphus", 7))(w, r)
	}))
	defer srv.Close()

	tasks, err := testClient(t, srv, nil).FindTasks(context.Background(),
		"sisyphus", "glebfm", []string{"DONE", "FAILED"}, 10)
	if err != nil {
		t.Fatalf("FindTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].TaskID != 7 {
		t.Errorf("tasks = %+v", tasks)
	}
}

func TestMinRequestSpacing(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK,
		BranchesResponse{Length: 1, Branches: []string{"p11"}}))
	defer srv.Close()

	c := testClient(t, srv, func(o *Options) {
		o.MinRequestSpacing = 60 * time.Millisecond
		o.MaxRetries = 0
	})
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.Branches(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 110*time.Millisecond {
		t.Errorf("three calls with a 60ms spacing took %s", elapsed)
	}
}

func TestStatsSnapshot(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK,
		BranchesResponse{Length: 1, Branches: []string{"p11"}}))
	defer srv.Close()

	c := testClient(t, srv, nil)
	if _, err := c.Branches(context.Background()); err != nil {
		t.Fatalf("Branches: %v", err)
	}
	st := c.Snapshot()
	if st.Requests != 1 || st.Successes != 1 || st.Failures != 0 {
		t.Errorf("stats = %+v", st)
	}
	if st.LastStatus != http.StatusOK {
		t.Errorf("last status = %d", st.LastStatus)
	}
	if st.LastError != "" {
		t.Errorf("last error = %q", st.LastError)
	}
}

// ---------------------------------------------------------------------------
// pure conversion helpers
// ---------------------------------------------------------------------------

func TestPackageLines(t *testing.T) {
	tests := []struct {
		name     string
		subtasks []Subtask
		want     []string
	}{
		{
			name:     "srpm name and evr win",
			subtasks: []Subtask{{SubtaskSrpmName: "postfix", SubtaskSrpmEvr: "3.8.21-alt1", SubtaskSrpm: "postfix-3.8.21-alt1.src.rpm"}},
			want:     []string{"postfix 3.8.21-alt1"},
		},
		{
			name:     "srpm file name is stripped",
			subtasks: []Subtask{{SubtaskSrpm: "malcontent-0.14.0-alt1.src.rpm"}},
			want:     []string{"malcontent-0.14.0-alt1"},
		},
		{
			name:     "gear directory plus tag",
			subtasks: []Subtask{{SubtaskDir: "/people/fidel/packages/rpm-macros-cuda-toolkit.git", SubtaskTagName: "0.1-alt1"}},
			want:     []string{"rpm-macros-cuda-toolkit 0.1-alt1"},
		},
		{
			name:     "delete subtask names a binary package",
			subtasks: []Subtask{{SubtaskPackage: "nvidia_cuda_toolkit_12.9.1_aarch64", SubtaskType: "delete"}},
			want:     []string{"nvidia_cuda_toolkit_12.9.1_aarch64"},
		},
		{
			name:     "src_pkg_name from task_info",
			subtasks: []Subtask{{SrcPkgName: "nvidia-nccl", SubtaskTagName: "2.31.2-alt1"}},
			want:     []string{"nvidia-nccl 2.31.2-alt1"},
		},
		{
			name:     "duplicates collapse",
			subtasks: []Subtask{{SubtaskSrpmName: "a", SubtaskSrpmEvr: "1"}, {SubtaskSrpmName: "a", SubtaskSrpmEvr: "1"}},
			want:     []string{"a 1"},
		},
		{
			name:     "empty subtasks are skipped",
			subtasks: []Subtask{{}, {SubtaskSrpmName: "b"}},
			want:     []string{"b"},
		},
		{name: "no subtasks", subtasks: nil, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PackageLines(tc.subtasks, MaxPackagesPerEvent)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPackageLinesCap(t *testing.T) {
	subs := make([]Subtask, 0, 20)
	for i := range 20 {
		subs = append(subs, Subtask{SubtaskSrpmName: fmt.Sprintf("pkg%02d", i), SubtaskSrpmEvr: "1"})
	}
	if got := PackageLines(subs, 5); len(got) != 5 {
		t.Errorf("cap not applied: %d lines", len(got))
	}
	if got := PackageLines(subs, 0); len(got) != 20 {
		t.Errorf("a non-positive cap must fall back to the default, got %d", len(got))
	}
}

func TestToTaskValidation(t *testing.T) {
	tests := []struct {
		name    string
		el      TaskElement
		branch  string
		wantErr string
	}{
		{
			name:    "zero id",
			el:      TaskElement{TaskRepo: "sisyphus", TaskChanged: "2026-09-16T13:56:00+00:00"},
			branch:  "sisyphus",
			wantErr: "non-positive task_id",
		},
		{
			name:    "unparsable timestamp",
			el:      TaskElement{TaskID: 1, TaskRepo: "sisyphus", TaskChanged: "later"},
			branch:  "sisyphus",
			wantErr: "task 1",
		},
		{
			name:    "no repo and no requested branch",
			el:      TaskElement{TaskID: 1, TaskChanged: "2026-09-16T13:56:00+00:00"},
			branch:  "",
			wantErr: "no task_repo",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.el.ToTask(tc.branch)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestToTaskNormalisesOwnerCase(t *testing.T) {
	el := TaskElement{TaskID: 1, TaskRepo: "sisyphus", TaskOwner: "  GlebFM ", TaskState: " DONE ",
		TaskChanged: "2026-09-16T13:56:00+00:00"}
	got, err := el.ToTask("sisyphus")
	if err != nil {
		t.Fatalf("ToTask: %v", err)
	}
	if got.Owner != "glebfm" {
		t.Errorf("owner = %q, want lower-cased and trimmed", got.Owner)
	}
	if got.State != "DONE" {
		t.Errorf("state = %q, want trimmed", got.State)
	}
}

func TestOldestChanged(t *testing.T) {
	t1 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	if _, ok := OldestChanged(nil); ok {
		t.Error("an empty list has no oldest record")
	}
	got, ok := OldestChanged([]model.Task{{Changed: t2}, {Changed: t1}})
	if !ok || !got.Equal(t1) {
		t.Errorf("oldest = %s (%t), want %s", got, ok, t1)
	}
}

func TestRepoBaseName(t *testing.T) {
	tests := map[string]string{
		"/gears/n/nvidia-nccl.git":               "nvidia-nccl",
		"/people/fidel/packages/rpm-macros.git":  "rpm-macros",
		"/people/fidel/packages/rpm-macros.git/": "rpm-macros",
		"kernel-modules-yt6801-rockchip64":       "kernel-modules-yt6801-rockchip64",
	}
	for in, want := range tests {
		if got := repoBaseName(in); got != want {
			t.Errorf("repoBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApprovalSummary(t *testing.T) {
	in := []Approval{
		{Type: "approve", Nickname: "rider"},
		{Type: "", Nickname: "armatik"},
		{Type: "revoke", Nickname: ""},
	}
	got := ApprovalSummary(in, 8)
	if len(got) != 2 || got[0] != "approve: rider" || got[1] != "approve: armatik" {
		t.Errorf("summary = %v", got)
	}
	if capped := ApprovalSummary(in, 1); len(capped) != 1 {
		t.Errorf("cap not applied: %v", capped)
	}
}

func TestDefaultBaseURLAndUserAgent(t *testing.T) {
	c := NewClient(Options{})
	if c.BaseURL() != "https://rdb.altlinux.org/api" {
		t.Errorf("base = %q", c.BaseURL())
	}
	if c.ua == "" {
		t.Error("a descriptive User-Agent must always be set")
	}
	if c.maxRetries != DefaultMaxRetries || c.spacing != DefaultMinRequestSpacing {
		t.Errorf("defaults not applied: retries=%d spacing=%s", c.maxRetries, c.spacing)
	}
}
