# cgr-sync

A one-shot CLI that mirrors [Chainguard](https://www.chainguard.dev/) images
from `cgr.dev` into a **private OCI registry** — Google Artifact Registry,
JFrog Artifactory, Cloudsmith, or any registry that speaks the OCI distribution
API. Built to run as a CI/CD step: read a config, copy what's missing, exit with
a status code.

- **Diff-based & idempotent** — lists source tags, compares digests against the
  destination, and copies only what's missing or changed. Re-running is cheap.
- **Multi-arch aware** — copies image indexes whole (all platforms) via
  [`go-containerregistry`](https://github.com/google/go-containerregistry).
- **Supply-chain preserving** — also mirrors cosign signatures, attestations,
  and SBOMs (the `sha256-<digest>.sig`/`.att`/`.sbom` tag scheme), so the mirror
  stays verifiable.
- **Any OCI registry** — auth comes from the standard Docker keychain
  (`~/.docker/config.json`), ambient cloud credentials, and cred helpers; no
  per-vendor code required.

## Install / build

```sh
go build -o cgr-sync ./cmd/cgr-sync
```

## Usage

```sh
cgr-sync -config cgr-sync.yaml            # sync per config
cgr-sync -config cgr-sync.yaml -dry-run   # plan only, copy nothing
```

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `cgr-sync.yaml` | Path to the config file. |
| `-dry-run` | `false` | Plan and print the work; copy nothing. |
| `-continue-on-error` | `false` | Keep going after a failure instead of exiting on the first. |
| `-no-signatures` | `false` | Mirror images only; skip cosign artifacts. |
| `-version` | | Print version and exit. |

Exit code is `0` on success, `1` if any image failed, `2` on a config error.

## Configuration

See [`examples/cgr-sync.yaml`](examples/cgr-sync.yaml). Per-repo settings inherit
from `defaults`:

```yaml
defaults:
  source: cgr.dev/chriscarty.com
  destination: us-docker.pkg.dev/my-project/cgr-mirror
  tags:
    list: ["latest"]

repositories:
  - name: python
    tags:
      list: ["latest", "3.12", "3.13"]
  - name: node
    tags:
      all: true
      include: '^[0-9]+$'    # major-version tags only
      exclude: '-dev$'
  - name: nginx
    destination: cloudsmith.io/my-org/cgr-mirror/nginx   # per-repo override
    tags: { list: ["latest"] }
```

A repository's `destination` may be a registry+namespace prefix (the repo name
is appended) or a full repository path (used as-is).

## Authentication

cgr-sync uses your existing registry credentials — log in with each registry's
normal tooling before running:

- **Source (`cgr.dev`):** `chainctl auth login` (interactive) or, for CI, a
  non-interactive identity/pull token written into the Docker config. Avoid the
  interactive browser flow in pipelines.
- **Google Artifact Registry:** ambient credentials (workload identity / service
  account) are picked up automatically, or `gcloud auth configure-docker`.
- **Artifactory / Cloudsmith / other:** `docker login <registry>` (token/API
  key), which cgr-sync reads from `~/.docker/config.json`.

## CI/CD

Run it as a job step. Sketch (GitLab CI):

```yaml
mirror-images:
  image: golang:1.26
  script:
    - go build -o cgr-sync ./cmd/cgr-sync
    - ./cgr-sync -config cgr-sync.yaml -continue-on-error
```

Provide non-interactive credentials via the job environment / Docker config so
no browser login is triggered.

## Status / roadmap

MVP — generic OCI mirroring with signature/attestation copy. Planned next:
referrers-API artifact mirroring, semver tag selection, concurrency, and
per-vendor auth conveniences.
