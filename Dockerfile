# syntax=docker/dockerfile:1
#
# Single image for all three process groups:
#   tb       — TigerBeetle ledger (PID 1: /tb-start.sh)
#   temporal — Temporal auto-setup server (PID 1: /temporal-start.sh)
#   app      — Go ConnectRPC gateway (PID 1: /server)
#
# Each group runs exactly one process as PID 1; Fly monitors and restarts it.

# ── Build Go binary ────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS builder

WORKDIR /src

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download -x

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    CGO_ENABLED=1 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ── TigerBeetle binary (static, arch-native) ───────────────────────────────
FROM ghcr.io/tigerbeetle/tigerbeetle:0.16.78 AS tb

# ── Temporal binaries and scripts (pure-Go, statically linked) ─────────────
FROM temporalio/auto-setup:1.29 AS temporal-src

# ── Runtime: Debian slim ───────────────────────────────────────────────────
# debian:bookworm-slim provides glibc + libgcc (needed by our CGO binary)
# and bash (needed by Temporal's shell entrypoint scripts).
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash ca-certificates netcat-openbsd \
    && rm -rf /var/lib/apt/lists/*

# TigerBeetle (static Zig binary)
COPY --from=tb /tigerbeetle /usr/local/bin/tigerbeetle

# Temporal: server binary, CLI tools, entrypoint/setup scripts, SQL schema
# All are pure-Go statically linked binaries — portable across libc variants.
COPY --from=temporal-src /usr/local/bin/temporal-server          /usr/local/bin/temporal-server
COPY --from=temporal-src /usr/local/bin/temporal-sql-tool        /usr/local/bin/temporal-sql-tool
COPY --from=temporal-src /usr/local/bin/temporal-cassandra-tool  /usr/local/bin/temporal-cassandra-tool
COPY --from=temporal-src /usr/local/bin/dockerize                /usr/local/bin/dockerize
COPY --from=temporal-src /usr/local/bin/tctl                     /usr/local/bin/tctl
COPY --from=temporal-src /usr/local/bin/temporal                 /usr/local/bin/temporal
COPY --from=temporal-src /etc/temporal/                          /etc/temporal/

# Go server (glibc CGO binary — runs natively on Debian)
COPY --from=builder /out/server /server

# Process group entrypoints
COPY --chmod=755 docker/tb-start.sh       /tb-start.sh
COPY --chmod=755 docker/temporal-start.sh  /temporal-start.sh

# Temporal dynamic config (overrides the bundled default)
COPY config/temporal/production-sql.yaml \
     /etc/temporal/config/dynamicconfig/production-sql.yaml

# Clear any USER from source images — all processes run as root on Fly.
USER root

# Set working dir to TEMPORAL_HOME so temporal-server finds config/docker.yaml.
# Other process groups (tb, app) use absolute paths and are unaffected.
WORKDIR /etc/temporal

# Clear ENTRYPOINT so Fly process group commands run directly.
ENTRYPOINT []

EXPOSE 3000 7233 8080
