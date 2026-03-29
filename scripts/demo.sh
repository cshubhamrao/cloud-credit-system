#!/usr/bin/env bash
set -euo pipefail

echo "============================================================"
echo "  BYOC Cloud Credit System — Demo Setup"
echo "============================================================"

# 1. Ensure services are up
echo ""
echo "[1/3] Starting infrastructure services..."
docker compose up -d
sleep 8

# 2. Start server in background (migrations run automatically on startup)
echo ""
echo "[2/3] Starting server..."
./bin/server &
SERVER_PID=$!
trap "kill $SERVER_PID 2>/dev/null || true" EXIT
sleep 3

# 3. Start simulator
echo ""
echo "[3/3] Launching simulator (TUI)..."
./bin/simulator

echo ""
echo "Demo complete. Shutting down server."
