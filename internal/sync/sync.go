// Package sync mirrors container images (and their cosign signatures /
// attestations) from a source registry to a destination registry. It lists the
// source tags, diffs them against the destination by digest, and copies only
// what is missing or changed — so re-running is cheap and idempotent. Cosign
// artifacts are carried both via the legacy "sha256-<hex>.{sig,att,sbom}" tag
// scheme and via the OCI Referrers API, so attestations attached either way
// survive the mirror.
package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/cartyc/image-syncer/internal/config"
	"github.com/cartyc/image-syncer/internal/verify"
)

// defaultRetries is the number of retry attempts for transient registry errors.
const defaultRetries = 3

// heartbeatInterval is how often a long-running copy logs a liveness line.
const heartbeatInterval = 15 * time.Second

// Verifier checks an image's signature against a policy before it is mirrored.
type Verifier interface {
	Verify(ctx context.Context, ref string, p verify.Policy) error
}

// Options control a sync run.
type Options struct {
	// DryRun plans the work and logs it, but copies nothing.
	DryRun bool
	// MirrorSignatures also copies cosign .sig/.att/.sbom artifacts.
	MirrorSignatures bool
	// ContinueOnError keeps going after a per-image failure (default: stop).
	ContinueOnError bool
	// Verify, when set, gates copying on cosign verification for repositories
	// whose policy is enabled. Required if any repo enables verification.
	Verify Verifier
	// Timeout bounds each registry HTTP operation (0 = no timeout).
	Timeout time.Duration
	// Logf receives human-readable progress lines (newline added by caller).
	Logf func(format string, args ...any)
}

// Result summarises a sync run.
type Result struct {
	Copied     int // images copied (or, in dry-run, would be copied)
	Skipped    int // images already in sync
	Signatures int // signature/attestation artifacts copied
	Failed     int // images that errored
}

type syncer struct {
	ctx    context.Context
	opts   Options
	crane  []crane.Option
	remote []remote.Option // same auth/transport as crane, for the Referrers API
}

// newSyncer wires the auth keychain and crane options shared by Run/SyncImage.
// DefaultKeychain reads ~/.docker/config.json (where `docker login` /
// `chainctl auth` store cgr.dev creds); google.Keychain adds ambient GAR/GCR
// auth. Together they cover the generic "any OCI registry" case.
func newSyncer(ctx context.Context, opts Options) *syncer {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	kc := authn.NewMultiKeychain(google.Keychain, authn.DefaultKeychain)
	rt := newTransport(opts.Timeout, defaultRetries)
	return &syncer{
		ctx:  ctx,
		opts: opts,
		crane: []crane.Option{
			crane.WithContext(ctx),
			crane.WithAuthFromKeychain(kc),
			crane.WithTransport(rt),
		},
		remote: []remote.Option{
			remote.WithContext(ctx),
			remote.WithAuthFromKeychain(kc),
			remote.WithTransport(rt),
		},
	}
}

// Run mirrors every repository in cfg. It returns a non-nil error when any
// operation failed (the Result still reports what succeeded).
func Run(ctx context.Context, cfg *config.Config, opts Options) (Result, error) {
	s := newSyncer(ctx, opts)
	var res Result
	for _, repo := range cfg.Repositories {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := s.syncRepo(repo, &res); err != nil {
			res.Failed++
			if !s.opts.ContinueOnError {
				return res, err
			}
			s.opts.Logf("ERROR repo %s: %v", repo.Name, err)
		}
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("%d operation(s) failed", res.Failed)
	}
	return res, nil
}

// SyncImage mirrors a single image on demand — used by the event listener when
// a push event arrives. sourceRepo is the fully-qualified source path (e.g.
// "cgr.dev/example.com/python"). It acts only when sourceRepo matches a
// configured repository and tag passes that repo's selector; matched reports
// whether a configured repo was found.
func SyncImage(ctx context.Context, cfg *config.Config, opts Options, sourceRepo, tag string) (res Result, matched bool, err error) {
	var repo *config.Repository
	for i := range cfg.Repositories {
		if cfg.Repositories[i].SourceRepo() == sourceRepo {
			repo = &cfg.Repositories[i]
			break
		}
	}
	if repo == nil {
		return res, false, nil
	}
	selected, _, serr := repo.Tags.Select([]string{tag})
	if serr != nil {
		return res, true, serr
	}
	if len(selected) == 0 {
		return res, true, nil // configured repo, but this tag isn't selected
	}
	s := newSyncer(ctx, opts)
	if err := s.syncTag(repo.SourceRepo(), repo.DestRepo(), tag, repo.Verify, &res); err != nil {
		res.Failed++
		return res, true, err
	}
	return res, true, nil
}

func (s *syncer) syncRepo(repo config.Repository, res *Result) error {
	src, dst := repo.SourceRepo(), repo.DestRepo()
	s.opts.Logf("repo %s -> %s", src, dst)

	tags, err := crane.ListTags(src, s.crane...)
	if err != nil {
		return fmt.Errorf("list tags for %s: %w", src, err)
	}
	selected, missing, err := repo.Tags.Select(tags)
	if err != nil {
		return err
	}
	for _, m := range missing {
		s.opts.Logf("  WARN requested tag %q not found in %s", m, src)
	}
	if len(selected) == 0 {
		s.opts.Logf("  nothing to mirror")
		return nil
	}
	for _, tag := range selected {
		if err := s.syncTag(src, dst, tag, repo.Verify, res); err != nil {
			res.Failed++
			if !s.opts.ContinueOnError {
				return err
			}
			s.opts.Logf("  ERROR %s:%s: %v", src, tag, err)
		}
	}
	return nil
}

func (s *syncer) syncTag(src, dst, tag string, pol config.Verify, res *Result) error {
	srcRef, dstRef := src+":"+tag, dst+":"+tag

	srcDigest, err := crane.Digest(srcRef, s.crane...)
	if err != nil {
		return fmt.Errorf("digest %s: %w", srcRef, err)
	}

	dstDigest, err := crane.Digest(dstRef, s.crane...)
	switch {
	case err != nil && !isNotFound(err):
		return fmt.Errorf("digest %s: %w", dstRef, err)
	case err == nil && dstDigest == srcDigest:
		s.opts.Logf("  = %s:%s in sync (%s)", dst, tag, short(srcDigest))
		res.Skipped++
		return s.mirrorArtifacts(src, dst, srcDigest, res)
	}

	if s.opts.DryRun {
		verifyNote := ""
		if pol.Enabled {
			verifyNote = " [would verify]"
		}
		s.opts.Logf("  + would copy %s:%s (%s)%s", dst, tag, short(srcDigest), verifyNote)
		res.Copied++
		return nil
	}

	// Gate the copy on cosign verification of the exact digest we're about to
	// mirror (verifying by digest avoids a tag-vs-content race).
	if pol.Enabled {
		if s.opts.Verify == nil {
			return fmt.Errorf("verification required for %s but no verifier configured", srcRef)
		}
		vref := src + "@" + srcDigest
		s.opts.Logf("  verifying %s:%s (%s)", src, tag, short(srcDigest))
		if err := s.opts.Verify.Verify(s.ctx, vref, toPolicy(pol)); err != nil {
			return fmt.Errorf("verify %s: %w", vref, err)
		}
	}

	s.opts.Logf("  + copy %s:%s (%s)", dst, tag, short(srcDigest))
	// A large multi-arch copy can run for minutes with no output; emit a
	// liveness line periodically so it doesn't look hung.
	stop := s.heartbeat(fmt.Sprintf("%s:%s", dst, tag), heartbeatInterval)
	err = crane.Copy(srcRef, dstRef, s.crane...)
	stop()
	if err != nil {
		return fmt.Errorf("copy %s -> %s: %w", srcRef, dstRef, err)
	}
	res.Copied++
	return s.mirrorArtifacts(src, dst, srcDigest, res)
}

// toPolicy maps the config verification block onto the verify package's policy.
func toPolicy(v config.Verify) verify.Policy {
	return verify.Policy{
		Identity:       v.CertificateIdentity,
		IdentityRegexp: v.CertificateIdentityRegexp,
		Issuer:         v.CertificateOIDCIssuer,
		IssuerRegexp:   v.CertificateOIDCIssuerRegexp,
	}
}

// mirrorArtifacts copies an image's cosign signatures/attestations to the
// destination, covering both ways they can be attached:
//
//   - the legacy "sha256-<hex>.{sig,att,sbom}" tag scheme, and
//   - the OCI Referrers API (subject -> referring artifact), which the tag
//     scheme misses. apko-built Chainguard images attach their SBOM attestation
//     as a referrer, so without this the mirror carries the image but not its
//     attestation, and a downstream `cosign verify-attestation` fails.
//
// Both are best-effort and idempotent: artifacts already present (by digest) on
// the destination are skipped, and a source that has neither is a no-op.
func (s *syncer) mirrorArtifacts(src, dst, digest string, res *Result) error {
	if !s.opts.MirrorSignatures || s.opts.DryRun {
		return nil
	}
	if err := s.mirrorSignatureTags(src, dst, digest, res); err != nil {
		return err
	}
	return s.mirrorReferrers(src, dst, digest, res)
}

// mirrorSignatureTags copies cosign artifacts that follow the tag scheme
// "sha256-<hex>.{sig,att,sbom}" derived from an image's digest. Absent
// artifacts are skipped silently.
func (s *syncer) mirrorSignatureTags(src, dst, digest string, res *Result) error {
	base := strings.Replace(digest, ":", "-", 1) // sha256:abc -> sha256-abc
	for _, suffix := range []string{".sig", ".att", ".sbom"} {
		tag := base + suffix
		srcRef, dstRef := src+":"+tag, dst+":"+tag

		srcDigest, err := crane.Digest(srcRef, s.crane...)
		if err != nil {
			if isNotFound(err) {
				continue // no such artifact on the source
			}
			return fmt.Errorf("check %s: %w", srcRef, err)
		}

		// Diff the artifact by digest too, so an in-sync run does no writes.
		if dstDigest, err := crane.Digest(dstRef, s.crane...); err == nil && dstDigest == srcDigest {
			continue
		} else if err != nil && !isNotFound(err) {
			return fmt.Errorf("check %s: %w", dstRef, err)
		}

		s.opts.Logf("    ~ signature %s", tag)
		if err := crane.Copy(srcRef, dstRef, s.crane...); err != nil {
			return fmt.Errorf("copy signature %s: %w", srcRef, err)
		}
		res.Signatures++
	}
	return nil
}

// mirrorReferrers copies every artifact that refers to the image digest via the
// OCI Referrers API. Copying the referrer manifest by digest preserves its
// `subject` field, so a referrers-aware destination (e.g. Artifact Registry)
// re-indexes it against the mirrored image. Registries without referrers
// support, or images with no referrers, are a silent no-op.
func (s *syncer) mirrorReferrers(src, dst, digest string, res *Result) error {
	srcRepo, err := name.NewRepository(src)
	if err != nil {
		return fmt.Errorf("parse repo %s: %w", src, err)
	}
	idx, err := remote.Referrers(srcRepo.Digest(digest), s.remote...)
	if err != nil {
		if isNotFound(err) {
			return nil // registry doesn't implement the referrers API
		}
		return fmt.Errorf("list referrers for %s@%s: %w", src, short(digest), err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("referrers index for %s@%s: %w", src, short(digest), err)
	}
	for _, desc := range im.Manifests {
		refDigest := desc.Digest.String()
		srcRef, dstRef := src+"@"+refDigest, dst+"@"+refDigest

		// Idempotent: skip referrers already on the destination.
		if _, err := crane.Digest(dstRef, s.crane...); err == nil {
			continue
		} else if !isNotFound(err) {
			return fmt.Errorf("check %s: %w", dstRef, err)
		}

		s.opts.Logf("    ~ referrer %s (%s)", short(refDigest), desc.ArtifactType)
		if err := crane.Copy(srcRef, dstRef, s.crane...); err != nil {
			return fmt.Errorf("copy referrer %s: %w", srcRef, err)
		}
		res.Signatures++
	}
	return nil
}

// heartbeat logs "still copying …" every `every` until the returned stop func is
// called, so a long copy doesn't look hung. stop blocks until the goroutine has
// exited, so no heartbeat line interleaves with later output.
func (s *syncer) heartbeat(what string, every time.Duration) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		var elapsed time.Duration
		for {
			select {
			case <-done:
				return
			case <-t.C:
				elapsed += every
				s.opts.Logf("    … still copying %s (%s elapsed)", what, elapsed)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// isNotFound reports whether err is a registry 404 (manifest/name unknown).
func isNotFound(err error) bool {
	var te *transport.Error
	if errors.As(err, &te) {
		return te.StatusCode == http.StatusNotFound
	}
	return false
}

// short trims a digest to "sha256:" + 12 hex chars for logging.
func short(d string) string {
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) >= i+13 {
		return d[:i+13]
	}
	return d
}
