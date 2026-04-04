#!/bin/sh
set -e

DATA=/data/0_0.tigerbeetle

if [ ! -f "$DATA" ]; then
  echo "Formatting TigerBeetle data file..."
  if ! tigerbeetle format --cluster=0 --replica=0 --replica-count=1 --development "$DATA"; then
    rm -f "$DATA"
    echo "Format failed, removed partial file; will retry on next start."
    exit 1
  fi
fi

# Bind on all interfaces (dual-stack) so both the Fly health check
# (127.0.0.1) and cross-machine gRPC (6PN IPv6) can reach port 3000.
# Fall back to 0.0.0.0 for local dev.
if [ -n "$FLY_PRIVATE_IP" ]; then
  ADDR="[::]:3000"
else
  ADDR="0.0.0.0:3000"
fi

# --development: reduces WAL/LSM/message-pool to dev defaults (~1.5GiB RSS vs 3.5GiB+ production).
# --cache-grid: explicit page cache cap.
# --limit-storage: caps FreeSet allocation (minimum valid value).
# --memory-lsm-manifest: reduces LSM manifest pool from 128MiB default.
exec tigerbeetle start \
  --addresses="$ADDR" \
  --development \
  --cache-grid=256MiB \
  "$DATA"
