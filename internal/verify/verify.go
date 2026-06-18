// Package verify gates mirroring on cosign signature verification. It shells
// out to the `cosign` CLI rather than embedding the (very large) sigstore
// dependency tree — keeping cgr-sync small and matching the toolchain CI
// pipelines already install (sigstore/cosign-installer).
package verify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Policy is the set of cosign keyless-verification constraints for an image.
// At least one identity field and one issuer field must be set.
type Policy struct {
	Identity       string // --certificate-identity
	IdentityRegexp string // --certificate-identity-regexp
	Issuer         string // --certificate-oidc-issuer
	IssuerRegexp   string // --certificate-oidc-issuer-regexp
}

// Cosign verifies images by invoking the cosign binary found on PATH.
type Cosign struct {
	bin string
}

// NewCosign locates the cosign binary, returning a clear error if it is absent.
func NewCosign() (*Cosign, error) {
	bin, err := exec.LookPath("cosign")
	if err != nil {
		return nil, fmt.Errorf("cosign not found on PATH (required for signature verification): %w", err)
	}
	return &Cosign{bin: bin}, nil
}

// Verify checks the signature of ref (preferably a digest reference) against
// the policy. It returns nil only when cosign reports a valid signature.
func (c *Cosign) Verify(ctx context.Context, ref string, p Policy) error {
	args := []string{"verify", "--output", "json"}
	if p.Identity != "" {
		args = append(args, "--certificate-identity", p.Identity)
	}
	if p.IdentityRegexp != "" {
		args = append(args, "--certificate-identity-regexp", p.IdentityRegexp)
	}
	if p.Issuer != "" {
		args = append(args, "--certificate-oidc-issuer", p.Issuer)
	}
	if p.IssuerRegexp != "" {
		args = append(args, "--certificate-oidc-issuer-regexp", p.IssuerRegexp)
	}
	args = append(args, ref)

	cmd := exec.CommandContext(ctx, c.bin, args...)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("cosign verify: %s", msg)
	}
	return nil
}
