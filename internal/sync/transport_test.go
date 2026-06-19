package sync

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fast retry transport for tests (no real backoff waits).
func testRT(base http.RoundTripper, retries int) *retryTransport {
	return &retryTransport{base: base, retries: retries, backoff: time.Millisecond}
}

func TestRetry_RecoversFromTransient500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError) // two transient failures
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := testRT(http.DefaultTransport, 3)
	resp, err := rt.RoundTrip(mustReq(t, http.MethodGet, srv.URL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (2 retries)", got)
	}
}

func TestRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	rt := testRT(http.DefaultTransport, 2)
	resp, err := rt.RoundTrip(mustReq(t, http.MethodGet, srv.URL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 { // 1 initial + 2 retries
		t.Errorf("server calls = %d, want 3", got)
	}
}

func TestRetry_DoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rt := testRT(http.DefaultTransport, 3)
	if _, err := rt.RoundTrip(mustReq(t, http.MethodGet, srv.URL)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (404 is not retried)", got)
	}
}

func mustReq(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
