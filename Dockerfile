# syntax=docker/dockerfile:1
#
# Build requires CGO_ENABLED=1 — TigerBeetle bundles a Zig/C native library.
# Runtime base must have glibc; distroless/base-debian12 provides it.

# ── Build ─────────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS builder

WORKDIR /src

# Download modules in a separate layer so source changes don't bust the cache.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download -x

# Compile. Bind-mount the source so it never lands in the layer store.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    CGO_ENABLED=1 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ── Runtime ───────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/base-debian12

COPY --from=builder /out/server /server

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/server"]
