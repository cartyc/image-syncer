package sync

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/cartyc/image-syncer/internal/config"
)

// newRegistry starts an in-memory OCI registry with the Referrers API enabled
// and returns its host:port (loopback, so go-containerregistry talks plain HTTP).
func newRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func descOf(t *testing.T, img v1.Image) v1.Descriptor {
	t.Helper()
	dig, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	mt, err := img.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	rm, err := img.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	return v1.Descriptor{MediaType: mt, Digest: dig, Size: int64(len(rm))}
}

// TestMirrorArtifacts covers both ways cosign artifacts attach to an image: the
// legacy "sha256-<hex>.sig" tag scheme and an OCI Referrers-API artifact (an
// attestation whose `subject` is the image). The referrer case is the gap that
// left mirrored Chainguard images without their SBOM attestation.
func TestMirrorArtifacts_TagSchemeAndReferrers(t *testing.T) {
	ctx := context.Background()
	srcRepo := newRegistry(t) + "/python"
	dstRepo := newRegistry(t) + "/python"

	// Base image at :latest.
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, srcRepo+":latest"); err != nil {
		t.Fatal(err)
	}
	imgDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	// Legacy tag-scheme artifact: <sha256-hex>.sig
	sig, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	sigTag := strings.Replace(imgDigest.String(), ":", "-", 1) + ".sig"
	if err := crane.Push(sig, srcRepo+":"+sigTag); err != nil {
		t.Fatal(err)
	}

	// Referrers-API artifact: an attestation that refers to the image.
	attBase, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	att, ok := mutate.Subject(attBase, descOf(t, img)).(v1.Image)
	if !ok {
		t.Fatal("mutate.Subject did not return a v1.Image")
	}
	attDigest, err := att.Digest()
	if err != nil {
		t.Fatal(err)
	}
	srcName, err := name.NewRepository(srcRepo)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(srcName.Digest(attDigest.String()), att, remote.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}

	// Mirror.
	s := newSyncer(ctx, Options{MirrorSignatures: true, Logf: t.Logf})
	var res Result
	if err := s.syncTag(srcRepo, dstRepo, "latest", config.Verify{}, &res); err != nil {
		t.Fatalf("syncTag: %v", err)
	}

	// Image mirrored at the same digest.
	if got, err := crane.Digest(dstRepo + ":latest"); err != nil {
		t.Fatalf("dst image digest: %v", err)
	} else if got != imgDigest.String() {
		t.Errorf("dst image digest = %s, want %s", got, imgDigest)
	}

	// Tag-scheme .sig mirrored.
	if _, err := crane.Digest(dstRepo + ":" + sigTag); err != nil {
		t.Errorf("dst .sig tag missing: %v", err)
	}

	// Referrer mirrored AND re-indexed by the destination's referrers API.
	dstName, err := name.NewRepository(dstRepo)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := remote.Referrers(dstName.Digest(imgDigest.String()), remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("dst referrers: %v", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range im.Manifests {
		if d.Digest.String() == attDigest.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("attestation referrer %s not discoverable on dst (%d referrers found)", attDigest, len(im.Manifests))
	}

	// One tag-scheme artifact + one referrer.
	if res.Signatures != 2 {
		t.Errorf("res.Signatures = %d, want 2", res.Signatures)
	}

	// Idempotent: a second run copies nothing.
	var res2 Result
	if err := s.syncTag(srcRepo, dstRepo, "latest", config.Verify{}, &res2); err != nil {
		t.Fatalf("syncTag rerun: %v", err)
	}
	if res2.Copied != 0 || res2.Signatures != 0 {
		t.Errorf("rerun not idempotent: copied=%d signatures=%d, want 0/0", res2.Copied, res2.Signatures)
	}
}

func TestHeartbeat(t *testing.T) {
	var mu sync.Mutex
	lines := 0
	s := newSyncer(context.Background(), Options{
		Logf: func(string, ...any) { mu.Lock(); lines++; mu.Unlock() },
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return lines }

	// Stopped before the first tick: no output, and stop() returns promptly.
	stop := s.heartbeat("x", time.Hour)
	stop()
	if n := count(); n != 0 {
		t.Errorf("heartbeat logged %d lines before the first tick, want 0", n)
	}

	// With a short interval it emits at least once; stop() is still clean.
	stop = s.heartbeat("x", 2*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	stop()
	if n := count(); n == 0 {
		t.Error("heartbeat logged 0 lines with a short interval, want >=1")
	}
}
