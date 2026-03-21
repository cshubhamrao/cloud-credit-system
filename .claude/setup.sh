#!/usr/bin/env bash
# Setup script for Claude Code (web) — runs once when a session starts.
# Prepares the Go workspace so the project can be built and worked on.
set -euo pipefail

# Ensure we're in the project root (script lives in .claude/)
cd "$(dirname "$0")/.."

echo "==> Loading environment..."
if [ -f .env ]; then
  set -a; source .env; set +a
elif [ -f .env.example ]; then
  set -a; source .env.example; set +a
fi

echo "==> Downloading Go modules..."
go mod download all

echo "==> Building project..."
make build

echo "==> Setup complete. Run 'make docker-up' to start infrastructure (PostgreSQL, TigerBeetle, Temporal)."
