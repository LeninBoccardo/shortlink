# Multi-stage build shared by every ShortLink binary (SPEC §17 M8).
#
# Build a specific binary:
#   docker build --build-arg BINARY=worker -t shortlink-worker:dev .
#
# Valid BINARY values: api, worker, observer, loadtest, migrate, keygen.
# CGO is off so the runtime stage is a static-distroless image and the
# build cache is shared across all binaries via the same go mod download.

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Cache the module graph independently of source -- changing handlers.go
# should not re-download deps.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BINARY=api
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=linux

# -trimpath strips local paths from the binary; -s -w drops the symbol/debug
# tables for a smaller image. Tag the version onto a runtime var if main.go
# declares one in a future milestone.
RUN go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/app \
    ./cmd/${BINARY}

# ---------------------------------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot

# Distroless static has CA roots; no shell, no package manager. Binary runs as
# uid 65532 (nonroot) so PodSecurity restricted is automatic.
COPY --from=builder /out/app /app

USER nonroot:nonroot
ENTRYPOINT ["/app"]
