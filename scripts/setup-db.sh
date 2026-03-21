#!/usr/bin/env bash
set -euo pipefail

PGHOST="${PGHOST:-localhost}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-postgres}"
PGPASSWORD="${PGPASSWORD:-postgres}"
PGDATABASE="${PGDATABASE:-creditdb}"

export PGPASSWORD

echo "==> Running migrations on ${PGHOST}:${PGPORT}/${PGDATABASE}"
psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -f sql/migrations/001_initial.sql
echo "==> Migrations complete."
