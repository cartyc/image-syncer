package sync

import (
	"io"
	"net"
	"net/http"
	"time"
)

// newTransport returns an http.RoundTripper with per-request timeouts and
// retry-with-backoff on transient failures (network errors, 429, 5xx). Registry
// operations go through it (via crane.WithTransport), so a slow registry fails
// fast instead of hanging, and a transient blip (e.g. a token endpoint 500) is
// retried rather than failing the whole run.
func newTransport(timeout time.Duration, retries int) http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if timeout > 0 {
		base.DialContext = (&net.Dialer{Timeout: timeout}).DialContext
		base.TLSHandshakeTimeout = timeout
		base.ResponseHeaderTimeout = timeout
		base.ExpectContinueTimeout = timeout
	}
	return &retryTransport{base: base, retries: retries, backoff: time.Second}
}

type retryTransport struct {
	base    http.RoundTripper
	retries int
	backoff time.Duration
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	wait := rt.backoff
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		// Rewind the body for retried requests that carry one.
		if attempt > 0 && req.GetBody != nil {
			b, e := req.GetBody()
			if e != nil {
				return resp, err
			}
			req.Body = b
		}
		resp, err = rt.base.RoundTrip(req)
		if attempt >= rt.retries || !retryable(req, resp, err) {
			return resp, err
		}
		// Discard the failed response body so the connection can be reused.
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		select {
		case <-time.After(wait):
		case <-req.Context().Done():
			return rt.base.RoundTrip(req)
		}
		wait *= 2
	}
}

// retryable reports whether a request should be retried. Only idempotent
// requests (GET/HEAD, or any request whose body can be rewound) are retried, on
// network errors, 429, or 5xx (except 501 Not Implemented).
func retryable(req *http.Request, resp *http.Response, err error) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead && req.GetBody == nil {
		return false
	}
	if err != nil {
		return true
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented
}
