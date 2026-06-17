package events

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubValidator struct{ err error }

func (s stubValidator) Validate(context.Context, string) error { return s.err }

func post(h *Handler, ceType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	if ceType != "" {
		req.Header.Set("ce-type", ceType)
	}
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const pushBody = `{"body":{"repository":"example.com/python","tag":"3.13","digest":"sha256:abc"}}`

func TestHandler_PushTriggersSync(t *testing.T) {
	var gotRepo, gotTag string
	h := &Handler{
		Validate: stubValidator{},
		OnPush: func(_ context.Context, repo, tag, _ string) error {
			gotRepo, gotTag = repo, tag
			return nil
		},
	}
	rec := post(h, PushType, pushBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotRepo != "cgr.dev/example.com/python" || gotTag != "3.13" {
		t.Errorf("OnPush got %s:%s", gotRepo, gotTag)
	}
}

func TestHandler_NonPushIgnored(t *testing.T) {
	called := false
	h := &Handler{
		Validate: stubValidator{},
		OnPush:   func(context.Context, string, string, string) error { called = true; return nil },
	}
	rec := post(h, "dev.chainguard.registry.pull.v1", pushBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if called {
		t.Error("OnPush should not run for non-push events")
	}
}

func TestHandler_RejectsBadToken(t *testing.T) {
	called := false
	h := &Handler{
		Validate: stubValidator{err: errors.New("bad token")},
		OnPush:   func(context.Context, string, string, string) error { called = true; return nil },
	}
	rec := post(h, PushType, pushBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("OnPush should not run when validation fails")
	}
}

func TestHandler_SyncErrorReturns500(t *testing.T) {
	h := &Handler{
		Validate: stubValidator{},
		OnPush:   func(context.Context, string, string, string) error { return errors.New("boom") },
	}
	rec := post(h, PushType, pushBody)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
