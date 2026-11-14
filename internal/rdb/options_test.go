package rdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestExplicitlyDisabledRetriesMakeOnlyOneRequest(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := testClient(t, srv, func(o *Options) { o.MaxRetries = -1; o.MinRequestSpacing = -1 })
	if _, err := c.Branches(context.Background()); err == nil {
		t.Fatal("expected API failure")
	}
	if requests.Load() != 1 || c.Snapshot().Retries != 0 {
		t.Fatalf("requests: %d, stats: %+v", requests.Load(), c.Snapshot())
	}
	if c.spacing != 0 {
		t.Fatal("explicitly disabled spacing retained a delay")
	}
}
