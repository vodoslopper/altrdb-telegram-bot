package telegram

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type startupTransport func(*http.Request) (*http.Response, error)

func (f startupTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStartupProfileLookupHasDeadlineAndToleratesTimeout(t *testing.T) {
	h := newHarness(t, nil)
	cfg := *h.cfg
	cfg.HTTPTimeout = 20 * time.Millisecond
	calls := 0
	client := &http.Client{Transport: startupTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"id":123456,"is_bot":true,"first_name":"Bot","username":"test_bot"}}`))}, nil
		}
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("profile lookup has no context deadline")
			return nil, io.EOF
		}
		if remaining := time.Until(deadline); remaining > cfg.HTTPTimeout {
			t.Errorf("lookup deadline exceeds configured timeout: %s", remaining)
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	bt, err := New(Options{Config: &cfg, Store: h.store, Clock: h.clock, Logger: discardLogger(), HTTPClient: client})
	if err != nil {
		t.Fatalf("startup failed after optional lookup timeout: %v", err)
	}
	if calls != 2 || bt.botID != testBotID {
		t.Fatalf("calls: %d, bot ID: %d", calls, bt.botID)
	}
}
