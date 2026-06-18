# Build and run cgr-sync on free, public Chainguard images.
#
#   build  : cgr.dev/chainguard/go      (Go toolchain)
#   cosign : cgr.dev/chainguard/cosign  (for verify-before-mirror)
#   runtime: cgr.dev/chainguard/static  (distroless, nonroot, CA certs)
#
# Build:  docker build -t cgr-sync --build-arg VERSION=$(git describe --tags --always) .
# Run:    docker run --rm -v "$PWD/cgr-sync.yaml:/cgr-sync.yaml:ro" cgr-sync sync -config /cgr-sync.yaml

FROM cgr.dev/chainguard/go:latest AS build
USER root
WORKDIR /work
# Resolve modules first so the layer caches across source-only changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /work/cgr-sync ./cmd/cgr-sync

# cosign is only needed for the verify-before-mirror feature. It's a free
# Chainguard image; drop this stage and the COPY below for a smaller image if
# you never enable `verify` in your config.
FROM cgr.dev/chainguard/cosign:latest AS cosign

FROM cgr.dev/chainguard/static:latest
# cosign keyless verification caches Sigstore/TUF roots under $HOME.
ENV HOME=/tmp
COPY --from=build /work/cgr-sync /usr/bin/cgr-sync
COPY --from=cosign /usr/bin/cosign /usr/bin/cosign
# `serve` (the CloudEvents webhook) listens here by default; ignored by `sync`.
EXPOSE 8080
ENTRYPOINT ["/usr/bin/cgr-sync"]
