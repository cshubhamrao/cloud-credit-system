-- BYOC Cloud Credit System — Initial Schema
-- Run: psql -h localhost -U postgres -d creditdb -f sql/migrations/001_initial.sql

-- ─── Core Entities ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tenants (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name         TEXT NOT NULL,
  billing_tier TEXT NOT NULL CHECK (billing_tier IN ('free', 'starter', 'pro', 'enterprise')),
  status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'deregistered')),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  deleted_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS workload_clusters (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       UUID NOT NULL REFERENCES tenants(id),
  cloud_provider  TEXT NOT NULL CHECK (cloud_provider IN ('aws', 'gcp', 'azure')),
  region          TEXT NOT NULL,
  last_heartbeat  TIMESTAMPTZ,
  status          TEXT NOT NULL DEFAULT 'healthy'
                  CHECK (status IN ('healthy', 'degraded', 'unreachable', 'suspended', 'deregistered')),
  agent_version   TEXT,
  deregistered_at TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_clusters_tenant_id ON workload_clusters(tenant_id);

-- ─── Plans & Quotas ──────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS base_plans (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  resource_type TEXT NOT NULL,
  name          TEXT NOT NULL,
  credit_amount BIGINT NOT NULL,
  UNIQUE (resource_type, name)
);

-- Default PoC plans
INSERT INTO base_plans (resource_type, name, credit_amount)
VALUES
  ('cpu_hours',       'pro-1m',   1000000),
  ('memory_gb_hours', 'pro-1m',   1000000),
  ('active_nodes',    'pro-20',   20)
ON CONFLICT (resource_type, name) DO NOTHING;

CREATE TABLE IF NOT EXISTS quota_configs (
  tenant_id             UUID NOT NULL REFERENCES tenants(id),
  resource_type         TEXT NOT NULL,
  base_plan_id          UUID REFERENCES base_plans(id),
  hard_limit            BIGINT NOT NULL,
  soft_limit            BIGINT,
  burst_credits         BIGINT NOT NULL DEFAULT 0,
  billing_period        TEXT NOT NULL DEFAULT 'monthly'
                        CHECK (billing_period IN ('monthly', 'daily')),
  soft_limit_alert_sent BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY (tenant_id, resource_type)
);

-- ─── TigerBeetle Account Mapping ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tb_account_ids (
  tenant_id     UUID NOT NULL REFERENCES tenants(id),
  resource_type TEXT NOT NULL,
  account_type  TEXT NOT NULL CHECK (account_type IN ('operator', 'tenant_quota', 'sink')),
  tb_account_id BYTEA NOT NULL,  -- 16-byte (128-bit) TigerBeetle account ID
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (tenant_id, resource_type, account_type)
);

-- Global (non-tenant) TB accounts: operator and sink per resource.
-- These use a sentinel UUID (all zeros) for tenant_id.
CREATE TABLE IF NOT EXISTS tb_global_account_ids (
  resource_type TEXT NOT NULL,
  account_type  TEXT NOT NULL CHECK (account_type IN ('operator', 'sink')),
  tb_account_id BYTEA NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (resource_type, account_type)
);

-- ─── Quota Snapshots (near-real-time, updated by Temporal) ──────────────────

CREATE TABLE IF NOT EXISTS quota_snapshots (
  tenant_id      UUID NOT NULL REFERENCES tenants(id),
  resource_type  TEXT NOT NULL,
  credits_total  BIGINT NOT NULL DEFAULT 0,
  debits_posted  BIGINT NOT NULL DEFAULT 0,
  debits_pending BIGINT NOT NULL DEFAULT 0,
  available      BIGINT GENERATED ALWAYS AS (credits_total - debits_posted - debits_pending) STORED,
  snapshot_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (tenant_id, resource_type)
);

-- ─── Credit Adjustments Audit Trail ─────────────────────────────────────────

CREATE TABLE IF NOT EXISTS credit_adjustments (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      UUID NOT NULL REFERENCES tenants(id),
  resource_type  TEXT NOT NULL,
  amount         BIGINT NOT NULL,
  reason         TEXT,
  tb_transfer_id BYTEA,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
