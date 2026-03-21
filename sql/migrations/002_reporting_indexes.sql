-- BYOC Cloud Credit System — Reporting Indexes
-- Adds indexes to support common reporting and dashboard queries.

-- ─── tenants ────────────────────────────────────────────────────────────────
-- Filter active vs suspended/deregistered tenants (most dashboards exclude non-active)
CREATE INDEX IF NOT EXISTS idx_tenants_status ON tenants(status);
-- Segment usage/cost reports by billing tier
CREATE INDEX IF NOT EXISTS idx_tenants_billing_tier ON tenants(billing_tier);
-- Cohort analysis and time-series tenant growth reports
CREATE INDEX IF NOT EXISTS idx_tenants_created_at ON tenants(created_at);

-- ─── workload_clusters ──────────────────────────────────────────────────────
-- Cluster health summary (count by status, find degraded/unreachable clusters)
CREATE INDEX IF NOT EXISTS idx_clusters_status ON workload_clusters(status);
-- Infrastructure distribution: "how many clusters per cloud/region?"
CREATE INDEX IF NOT EXISTS idx_clusters_cloud_provider_region ON workload_clusters(cloud_provider, region);
-- Stale cluster detection: find clusters whose heartbeat hasn't been seen recently
CREATE INDEX IF NOT EXISTS idx_clusters_last_heartbeat ON workload_clusters(last_heartbeat);

-- ─── quota_snapshots ────────────────────────────────────────────────────────
-- Cross-tenant resource utilization: "show all tenants' cpu_hours usage"
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_resource_type ON quota_snapshots(resource_type);
-- Near-exhaustion alerts: "which tenants are low on credits?" (ORDER BY available ASC)
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_available ON quota_snapshots(available);

-- ─── quota_configs ──────────────────────────────────────────────────────────
-- Resource-level limit overview: "what are the hard limits for all tenants on memory_gb_hours?"
CREATE INDEX IF NOT EXISTS idx_quota_configs_resource_type ON quota_configs(resource_type);

-- ─── credit_adjustments ─────────────────────────────────────────────────────
-- Per-tenant audit trail ordered by time (most recent first)
CREATE INDEX IF NOT EXISTS idx_credit_adjustments_tenant_created ON credit_adjustments(tenant_id, created_at DESC);
-- Resource-level adjustment history: "all cpu_hours adjustments this month"
CREATE INDEX IF NOT EXISTS idx_credit_adjustments_resource_created ON credit_adjustments(resource_type, created_at DESC);
