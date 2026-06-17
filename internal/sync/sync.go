// Package sync mirrors container images (and their cosign signatures /
// attestations) from a source registry to a destination registry. It lists the
// source tags, diffs them against the destination by digest, and copies only
// what is missing or changed — so re-running is cheap and idempotent.
package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/cartyc/image-syncer/internal/config"
)

// Options control a sync run.
type Options struct {
	// DryRun plans the work and logs it, but copies nothing.
	DryRun bool
	// MirrorSignatures also copies cosign .sig/.att/.sbom artifacts.
	MirrorSignatures bool
	// ContinueOnError keeps going after a per-image failure (default: stop).
	ContinueOnError bool
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
	opts  Options
	crane []crane.Option
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
	return &syncer{
		opts:  opts,
		crane: []crane.Option{crane.WithContext(ctx), crane.WithAuthFromKeychain(kc)},
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
	if err := s.syncTag(repo.SourceRepo(), repo.DestRepo(), tag, &res); err != nil {
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
		if err := s.syncTag(src, dst, tag, res); err != nil {
			res.Failed++
			if !s.opts.ContinueOnError {
				return err
			}
			s.opts.Logf("  ERROR %s:%s: %v", src, tag, err)
		}
	}
	return nil
}

func (s *syncer) syncTag(src, dst, tag string, res *Result) error {
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
		return s.mirrorSignatures(src, dst, srcDigest, res)
	}

	if s.opts.DryRun {
		s.opts.Logf("  + would copy %s:%s (%s)", dst, tag, short(srcDigest))
		res.Copied++
		return nil
	}
	s.opts.Logf("  + copy %s:%s (%s)", dst, tag, short(srcDigest))
	if err := crane.Copy(srcRef, dstRef, s.crane...); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", srcRef, dstRef, err)
	}
	res.Copied++
	return s.mirrorSignatures(src, dst, srcDigest, res)
}

// mirrorSignatures copies cosign artifacts that follow the tag scheme
// "sha256-<hex>.{sig,att,sbom}" derived from an image's digest. Absent
// artifacts are skipped silently. (Referrers-API artifacts are a follow-up.)
func (s *syncer) mirrorSignatures(src, dst, digest string, res *Result) error {
	if !s.opts.MirrorSignatures || s.opts.DryRun {
		return nil
	}
	base := strings.Replace(digest, ":", "-", 1) // sha256:abc -> sha256-abc
	for _, suffix := range []string{".sig", ".att", ".sbom"} {
		tag := base + suffix
		srcRef := src + ":" + tag
		if _, err := crane.Digest(srcRef, s.crane...); err != nil {
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("check %s: %w", srcRef, err)
		}
		s.opts.Logf("    ~ signature %s", tag)
		if err := crane.Copy(srcRef, dst+":"+tag, s.crane...); err != nil {
			return fmt.Errorf("copy signature %s: %w", srcRef, err)
		}
		res.Signatures++
	}
	return nil
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
