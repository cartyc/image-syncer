// Package events implements a webhook listener for Chainguard registry
// CloudEvents. It validates the event's OIDC token, filters to registry "push"
// events, and invokes a callback to mirror the affected image — letting
// cgr-sync react to new/updated images in near-real-time alongside the cron.
//
// Subscribe your deployed listener with:
//
//	chainctl events subscriptions create https://<your-webhook-url>/events
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	// PushType is the ce-type header for registry push events (image added/updated).
	PushType = "dev.chainguard.registry.push.v1"
	// Issuer is the Chainguard OIDC issuer that signs event tokens.
	Issuer = "https://issuer.enforce.dev"
	// defaultSource is the registry host prepended to the event's repository.
	defaultSource = "cgr.dev"
)

// SyncFunc mirrors a single image identified by its fully-qualified source repo
// (e.g. "cgr.dev/example.com/python"), tag, and digest.
type SyncFunc func(ctx context.Context, sourceRepo, tag, digest string) error

// Validator authenticates the bearer token on an incoming event.
type Validator interface {
	Validate(ctx context.Context, bearerToken string) error
}

// Handler validates Chainguard registry CloudEvents and triggers OnPush for
// push events. It implements http.Handler.
type Handler struct {
	// Source is the registry host prepended to the event repository
	// (default "cgr.dev").
	Source string
	// Validate authenticates the event token (use OIDCValidator in production).
	Validate Validator
	// OnPush mirrors the affected image.
	OnPush SyncFunc
	// Logf receives progress/diagnostic lines (optional).
	Logf func(format string, args ...any)
}

func (h *Handler) logf(f string, a ...any) {
	if h.Logf != nil {
		h.Logf(f, a...)
	}
}

func (h *Handler) source() string {
	if h.Source != "" {
		return h.Source
	}
	return defaultSource
}

// registryEvent is the subset of the CloudEvent data we consume.
type registryEvent struct {
	Body struct {
		Repository string `json:"repository"` // "<org>/<repo>", no registry host
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
	} `json:"body"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Acknowledge non-push events with 200 so Chainguard doesn't retry them.
	if t := r.Header.Get("ce-type"); t != PushType {
		w.WriteHeader(http.StatusOK)
		return
	}

	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err := h.Validate.Validate(r.Context(), bearer); err != nil {
		h.logf("event rejected: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var ev registryEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if ev.Body.Repository == "" || ev.Body.Tag == "" {
		w.WriteHeader(http.StatusOK) // nothing actionable
		return
	}

	sourceRepo := h.source() + "/" + strings.TrimPrefix(ev.Body.Repository, "/")
	h.logf("push event: %s:%s (%s)", sourceRepo, ev.Body.Tag, ev.Body.Digest)
	if err := h.OnPush(r.Context(), sourceRepo, ev.Body.Tag, ev.Body.Digest); err != nil {
		// 500 → Chainguard will retry, which is what we want for transient errors.
		h.logf("sync failed for %s:%s: %v", sourceRepo, ev.Body.Tag, err)
		http.Error(w, "sync failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// OIDCValidator verifies the event's OIDC ID token against the Chainguard
// issuer, the expected audience (your public webhook URL), and — optionally —
// the expected subject identity (e.g. "webhook:<UIDP>").
type OIDCValidator struct {
	verifier *oidc.IDTokenVerifier
	identity string
}

// NewOIDCValidator builds a validator. audience must equal the public URL you
// registered with `chainctl events subscriptions create`. identity, when set,
// is the expected token subject.
func NewOIDCValidator(ctx context.Context, audience, identity string) (*OIDCValidator, error) {
	provider, err := oidc.NewProvider(ctx, Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc provider %s: %w", Issuer, err)
	}
	return &OIDCValidator{
		verifier: provider.Verifier(&oidc.Config{ClientID: audience}),
		identity: identity,
	}, nil
}

// Validate checks the token signature, issuer, expiry, audience and subject.
func (v *OIDCValidator) Validate(ctx context.Context, bearer string) error {
	if bearer == "" {
		return fmt.Errorf("missing bearer token")
	}
	tok, err := v.verifier.Verify(ctx, bearer)
	if err != nil {
		return fmt.Errorf("verify token: %w", err)
	}
	if v.identity != "" && tok.Subject != v.identity {
		return fmt.Errorf("unexpected subject %q (want %q)", tok.Subject, v.identity)
	}
	return nil
}

// InsecureNoValidator accepts every event without verifying the token. For
// local testing only — never expose this on a public endpoint.
type InsecureNoValidator struct{}

func (InsecureNoValidator) Validate(context.Context, string) error { return nil }
