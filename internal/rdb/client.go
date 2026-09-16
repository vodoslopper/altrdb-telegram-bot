// Package rdb is the read-only client for the ALT RDB (ALTRepo) API.
//
// The public API exposes no task-event webhook, so ingestion is polite HTTP
// polling. This package is where "polite" is enforced: explicit connect, TLS,
// response-header and overall timeouts; a descriptive User-Agent; bounded
// exponential backoff with jitter; Retry-After honoured; a minimum spacing
// between requests; and a response size cap.
package rdb

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"altrdb-telegram-bot/internal/clock"
	"altrdb-telegram-bot/internal/model"
)

// Endpoint paths, relative to the base URL.
const (
	PathAllBranches = "/task/progress/all_tasks_branches"
	PathLastTasks   = "/task/progress/last_tasks"
	PathTaskInfo    = "/task/progress/task_info"
	PathFindTasks   = "/task/progress/find_tasks"
	PathMaintainers = "/site/all_maintainers_with_nicknames"
)

// Default limits.
const (
	DefaultMaxRetries        = 3
	DefaultBackoffBase       = 500 * time.Millisecond
	DefaultBackoffMax        = 30 * time.Second
	DefaultMinRequestSpacing = 200 * time.Millisecond
	DefaultMaxResponseBytes  = 32 << 20 // 32 MiB, the feed is ~1 MiB for 200 tasks
)

// APIError is a non-2xx response, or a transport failure classified the same
// way so the caller can treat both uniformly.
type APIError struct {
	StatusCode int
	Path       string
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *APIError) Error() string {
	parts := []string{"rdb " + e.Path}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("status %d", e.StatusCode))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

// Unwrap exposes the underlying transport error.
func (e *APIError) Unwrap() error { return e.Err }

// Retryable reports whether another attempt could plausibly succeed.
func (e *APIError) Retryable() bool {
	if e.Err != nil && e.StatusCode == 0 {
		// Network level failure: DNS, connect, TLS, timeout, reset.
		return !errors.Is(e.Err, context.Canceled)
	}
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// IsRateLimit reports a 429.
func (e *APIError) IsRateLimit() bool { return e.StatusCode == http.StatusTooManyRequests }

// IsNotFound reports a 404, which for task_info simply means "no such task".
func (e *APIError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// ErrNotFound is returned by TaskInfo for a task the API does not know.
var ErrNotFound = errors.New("rdb: not found")

// Stats is a snapshot of client-side counters, exported to /metrics.
type Stats struct {
	Requests    uint64
	Successes   uint64
	Failures    uint64
	Retries     uint64
	RateLimited uint64
	LastStatus  int
	LastLatency time.Duration
	LastError   string
}

// Options configures a Client.
type Options struct {
	BaseURL               string
	UserAgent             string
	Timeout               time.Duration // overall, per attempt
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	MaxRetries            int
	BackoffBase           time.Duration
	BackoffMax            time.Duration
	MinRequestSpacing     time.Duration
	MaxResponseBytes      int64
	// HTTPClient overrides the constructed client. Tests point it at an
	// httptest.Server; production leaves it nil.
	HTTPClient *http.Client
	Logger     *slog.Logger
	Clock      clock.Clock
	// Jitter returns a float in [0,1). Injectable so backoff is testable.
	Jitter func() float64
}

// Client talks to the ALT RDB API.
type Client struct {
	base   string
	ua     string
	http   *http.Client
	log    *slog.Logger
	clock  clock.Clock
	jitter func() float64

	maxRetries    int
	backoffBase   time.Duration
	backoffMax    time.Duration
	maxBytes      int64
	headerTimeout time.Duration

	gateMu  sync.Mutex
	gateAt  time.Time
	spacing time.Duration

	requests    atomic.Uint64
	successes   atomic.Uint64
	failures    atomic.Uint64
	retries     atomic.Uint64
	rateLimited atomic.Uint64
	lastStatus  atomic.Int64
	lastLatency atomic.Int64
	lastError   atomic.Pointer[string]
}

// NewClient builds a client. Zero-valued options fall back to the defaults.
func NewClient(opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = clock.New()
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = DefaultMaxRetries
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = DefaultBackoffBase
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = DefaultBackoffMax
	}
	if opts.MinRequestSpacing < 0 {
		opts.MinRequestSpacing = 0
	} else if opts.MinRequestSpacing == 0 {
		opts.MinRequestSpacing = DefaultMinRequestSpacing
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if opts.Jitter == nil {
		opts.Jitter = rand.Float64
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	if opts.TLSHandshakeTimeout <= 0 {
		opts.TLSHandshakeTimeout = 10 * time.Second
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = 30 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 45 * time.Second
	}

	c := &Client{
		base:          strings.TrimRight(opts.BaseURL, "/"),
		ua:            opts.UserAgent,
		log:           opts.Logger,
		clock:         opts.Clock,
		jitter:        opts.Jitter,
		maxRetries:    opts.MaxRetries,
		backoffBase:   opts.BackoffBase,
		backoffMax:    opts.BackoffMax,
		maxBytes:      opts.MaxResponseBytes,
		headerTimeout: opts.ResponseHeaderTimeout,
		spacing:       opts.MinRequestSpacing,
	}
	if c.base == "" {
		c.base = "https://rdb.altlinux.org/api"
	}
	if c.ua == "" {
		c.ua = "altrdb-telegram-bot/dev"
	}
	if opts.HTTPClient != nil {
		c.http = opts.HTTPClient
	} else {
		c.http = &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   opts.ConnectTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   opts.TLSHandshakeTimeout,
				ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				MaxIdleConns:          8,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		}
	}
	return c
}

// BaseURL is the resolved API root.
func (c *Client) BaseURL() string { return c.base }

// Snapshot returns the current counters.
func (c *Client) Snapshot() Stats {
	st := Stats{
		Requests:    c.requests.Load(),
		Successes:   c.successes.Load(),
		Failures:    c.failures.Load(),
		Retries:     c.retries.Load(),
		RateLimited: c.rateLimited.Load(),
		LastStatus:  int(c.lastStatus.Load()),
		LastLatency: time.Duration(c.lastLatency.Load()),
	}
	if p := c.lastError.Load(); p != nil {
		st.LastError = *p
	}
	return st
}

// Branches returns the branch list, as reported by the live task feed.
func (c *Client) Branches(ctx context.Context) ([]string, error) {
	var resp BranchesResponse
	if err := c.get(ctx, PathAllBranches, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Branches))
	for _, b := range resp.Branches {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rdb: %s returned no branches", PathAllBranches)
	}
	return out, nil
}

// LastTasks fetches the newest records for one branch. It returns the raw
// response (for request_args and length) and the converted domain tasks.
// Records that fail validation are skipped and counted, not fatal: one bad row
// must not blind the whole branch.
func (c *Client) LastTasks(ctx context.Context, branch string, limit int) (*TasksListResponse, []model.Task, int, error) {
	if branch == "" {
		return nil, nil, 0, errors.New("rdb: empty branch")
	}
	q := url.Values{}
	q.Set("branch", branch)
	if limit > 0 {
		q.Set("tasks_limit", strconv.Itoa(limit))
	}

	var resp TasksListResponse
	if err := c.get(ctx, PathLastTasks, q, &resp); err != nil {
		return nil, nil, 0, err
	}

	tasks := make([]model.Task, 0, len(resp.Tasks))
	skipped := 0
	for _, el := range resp.Tasks {
		t, err := el.ToTask(branch)
		if err != nil {
			skipped++
			c.log.Warn("skipping malformed task record",
				"branch", branch, "task_id", el.TaskID, "error", err.Error())
			continue
		}
		tasks = append(tasks, t)
	}
	return &resp, tasks, skipped, nil
}

// TaskInfo fetches the full record of one task. Used for on-demand diagnostics,
// never in the polling path.
func (c *Client) TaskInfo(ctx context.Context, taskID int64) (*TaskInfoResponse, error) {
	var resp TaskInfoResponse
	p := PathTaskInfo + "/" + strconv.FormatInt(taskID, 10)
	if err := c.get(ctx, p, nil, &resp); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &resp, nil
}

// FindTasks queries the search endpoint. It exists for /recent-style
// diagnostics; the polling path never uses it because `input` is mandatory and
// a per-user search does not scale to many subscribers.
func (c *Client) FindTasks(ctx context.Context, branch, owner string, states []string, limit int) ([]model.Task, error) {
	q := url.Values{}
	if len(states) > 0 {
		q.Set("input", strings.Join(states, " "))
		for _, st := range states {
			q.Add("state", st)
		}
	} else {
		q.Set("input", branch)
	}
	if branch != "" {
		q.Set("branch", branch)
	}
	if owner != "" {
		q.Set("owner", owner)
	}
	if limit > 0 {
		q.Set("tasks_limit", strconv.Itoa(limit))
	}

	var resp TasksListResponse
	if err := c.get(ctx, PathFindTasks, q, &resp); err != nil {
		return nil, err
	}
	tasks := make([]model.Task, 0, len(resp.Tasks))
	for _, el := range resp.Tasks {
		if t, err := el.ToTask(branch); err == nil {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// Maintainers fetches the packager directory of one branch.
func (c *Client) Maintainers(ctx context.Context, branch string) ([]model.Maintainer, error) {
	if branch == "" {
		return nil, errors.New("rdb: empty branch")
	}
	q := url.Values{}
	q.Set("branch", branch)

	var resp MaintainersResponse
	if err := c.get(ctx, PathMaintainers, q, &resp); err != nil {
		return nil, err
	}
	now := c.clock.Now()
	out := make([]model.Maintainer, 0, len(resp.Maintainers))
	for _, m := range resp.Maintainers {
		nick := strings.ToLower(strings.TrimSpace(m.PackagerNickname))
		if nick == "" {
			continue
		}
		out = append(out, model.Maintainer{
			Branch:         branch,
			Nickname:       nick,
			PackagerName:   strings.TrimSpace(m.PackagerName),
			CountSourcePkg: m.CountSourcePkg,
			FetchedAt:      now,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rdb: %s returned no maintainers for branch %s", PathMaintainers, branch)
	}
	return out, nil
}

// get performs one request with retries and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, p string, q url.Values, out any) error {
	full := c.base + p
	if len(q) > 0 {
		full += "?" + q.Encode()
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			c.retries.Add(1)
		}
		if err := c.wait(ctx); err != nil {
			return err
		}

		err := c.doOnce(ctx, p, full, out)
		if err == nil {
			c.successes.Add(1)
			return nil
		}
		lastErr = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() {
			c.failures.Add(1)
			return err
		}
		if attempt == c.maxRetries {
			break
		}

		delay := c.backoff(attempt, apiErr.RetryAfter)
		c.log.Warn("rdb request failed, retrying",
			"path", p, "attempt", attempt+1, "max_attempts", c.maxRetries+1,
			"status", apiErr.StatusCode, "retry_in", delay.String(), "error", err.Error())

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			c.failures.Add(1)
			return ctx.Err()
		case <-timer.C:
		}
	}

	c.failures.Add(1)
	if lastErr == nil {
		lastErr = fmt.Errorf("rdb %s: request failed", p)
	}
	return lastErr
}

// backoff returns how long to wait before the next attempt. An explicit
// Retry-After always wins; otherwise the delay is exponential with equal
// jitter, capped.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, c.backoffMax*4)
	}
	exp := float64(c.backoffBase) * math.Pow(2, float64(attempt))
	if exp > float64(c.backoffMax) {
		exp = float64(c.backoffMax)
	}
	// Equal jitter: half deterministic, half random. Keeps a fleet of
	// processes from re-hitting RDB in lockstep while still bounding the wait.
	half := exp / 2
	return time.Duration(half + half*c.jitter())
}

// doOnce performs a single attempt.
func (c *Client) doOnce(ctx context.Context, p, full string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return &APIError{Path: p, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")

	c.requests.Add(1)
	start := c.clock.Now()
	resp, err := c.http.Do(req)
	latency := time.Since(start)
	c.lastLatency.Store(int64(latency))
	if err != nil {
		c.lastStatus.Store(0)
		c.setLastError(err.Error())
		return &APIError{Path: p, Err: err}
	}
	defer resp.Body.Close()

	c.lastStatus.Store(int64(resp.StatusCode))

	if resp.StatusCode == http.StatusTooManyRequests {
		c.rateLimited.Add(1)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Path:       p,
			Message:    strings.TrimSpace(errorMessage(body, resp.StatusCode)),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.clock.Now()),
		}
		c.setLastError(apiErr.Error())
		return apiErr
	}

	limited := io.LimitReader(resp.Body, c.maxBytes+1)
	dec := json.NewDecoder(limited)
	if err := dec.Decode(out); err != nil {
		c.setLastError(err.Error())
		return &APIError{
			StatusCode: resp.StatusCode,
			Path:       p,
			Err:        fmt.Errorf("decode response: %w", err),
		}
	}
	// Reject a body that was cut off by the size cap rather than silently
	// acting on a truncated feed.
	if dec.More() {
		err := fmt.Errorf("response larger than %d bytes", c.maxBytes)
		c.setLastError(err.Error())
		return &APIError{StatusCode: resp.StatusCode, Path: p, Err: err}
	}
	return nil
}

// wait enforces the minimum spacing between requests.
func (c *Client) wait(ctx context.Context) error {
	if c.spacing <= 0 {
		return nil
	}
	c.gateMu.Lock()
	now := time.Now()
	if now.Before(c.gateAt) {
		delay := c.gateAt.Sub(now)
		c.gateAt = c.gateAt.Add(c.spacing)
		c.gateMu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	c.gateAt = now.Add(c.spacing)
	c.gateMu.Unlock()
	return nil
}

func (c *Client) setLastError(s string) {
	if len(s) > 256 {
		s = s[:256]
	}
	c.lastError.Store(&s)
}

// parseRetryAfter understands both the delta-seconds and HTTP-date forms.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// errorMessage extracts the API's own message when there is one, so logs say
// "no such task" instead of "status 404".
func errorMessage(body []byte, status int) string {
	if len(body) == 0 {
		return http.StatusText(status)
	}
	var er ErrorResponse
	if err := json.Unmarshal(body, &er); err == nil && er.Message != "" {
		return er.Message
	}
	return strings.TrimSpace(string(body))
}
