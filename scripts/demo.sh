#!/usr/bin/env bash
set -euo pipefail

echo "============================================================"
echo "  BYOC Cloud Credit System — Demo Setup"
echo "============================================================"

# 1. Ensure services are up
echo ""
echo "[1/4] Starting infrastructure services..."
docker compose up -d
sleep 8

# 2. Run migrations
echo ""
echo "[2/4] Running database migrations..."
PGPASSWORD=postgres psql -h localhost -U postgres -d creditdb \
  -f sql/migrations/001_initial.sql 2>/dev/null || echo "  (migrations may already be applied)"

# 3. Start server in background
echo ""
echo "[3/4] Starting server..."
./bin/server &
SERVER_PID=$!
trap "kill $SERVER_PID 2>/dev/null || true" EXIT
sleep 3

# 4. Start simulator
echo ""
echo "[4/4] Launching simulator (TUI)..."
./bin/simulator

echo ""
echo "Demo complete. Shutting down server."
