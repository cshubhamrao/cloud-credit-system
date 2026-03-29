# PoC Scope vs. Full Design

What's implemented in this PoC, and what remains from the full design (`docs/Design_wip.md`).

---

## Implemented in PoC

- [x] ConnectRPC gateway with bidi streaming heartbeats (`HeartbeatStream`)
- [x] TigerBeetle double-entry ledger — 3 resource types: `cpu_hours`, `memory_gb_hours`, `active_nodes`
- [x] `TenantAccountingWorkflow` — long-running, one per tenant, batched TB writes
- [x] `TenantProvisioningWorkflow` + `RegisterClusterWorkflow`
- [x] Hard limit enforcement via `debits_must_not_exceed_credits` on tenant quota accounts
- [x] Soft limit warnings (`STATUS_QUOTA_WARNING` in heartbeat response)
- [x] Deterministic BLAKE3 transfer IDs for heartbeat idempotency
- [x] Three-layer dedup: gateway `DedupCache` → workflow `processedSeqs` → TB transfer ID
- [x] PostgreSQL quota snapshots (projection, updated by Temporal after each TB flush)
- [x] Surge pack / manual credit top-up (`IssueTenantCredit` admin RPC → Temporal Update)
- [x] Gauge resource handling — `active_nodes` with void+create pending transfers per flush
- [x] Adaptive flush interval (5s base, doubles to 60s when idle, immediate when batch ≥ 20)
- [x] Web dashboard — runtime stats, quota bars, tenant/cluster cards, simulator controls
- [x] TUI simulator — 7-step scripted demo, 2 tenants, 5 clusters, real gRPC streams
- [x] Unit tests across all layers (domain, ledger, gateway dedup, workflow dedup, workflow invariants)
- [x] Integration tests (TigerBeetle and Temporal with real services)
- [x] Valkey/Redis shared dedup cache (with in-memory fallback if Valkey unavailable)
- [x] zstd compression for ConnectRPC (server + simulator)
- [x] Admin RPCs: delete tenant, deregister cluster, issue credit, list quotas
- [x] Embedded SQL migrations — server self-bootstraps, no `psql` required
- [x] Debug HTTP server on `:6061` (`/debug/stats`, `/debug/pprof/*`)

---

## Not Yet Implemented

### Security & Auth
- [ ] mTLS + JWT authentication for workload clusters
- [ ] Bootstrap tokens (one-time use, `bootstrap_tokens` table, `cert_deny_list`)
- [ ] Certificate rotation (agent auto-renews at 50% TTL)
- [ ] Signed commands (HMAC with expiry — prevents replay of enforcement commands)
- [ ] Row-Level Security (RLS) for multi-tenant isolation in PostgreSQL

### Billing Lifecycle
- [ ] `BillingResetWorkflow` — monthly drain+re-credit as linked atomic TB batch, void pending gauges
- [ ] `billing_snapshots` table — end-of-period usage records for invoice generation
- [ ] Invoice generation (`invoices` table, downstream billing integration)
- [ ] Four-eyes approval for large credit adjustments (approval threshold in `credit_adjustments`)
- [ ] Usage corrections (transfer code 201) — positive/negative, linked to original transfer ID

### Workflow Robustness
- [ ] `ContinueAsNew` — workflow history compaction for long-running per-tenant workflows
- [ ] Missed-heartbeat timers — `degraded` → `unreachable` → `suspended` escalation in workflow
- [ ] Agent graceful shutdown — `shutting_down` flag in heartbeat suppresses false-positive escalation
- [ ] Reconnection semantics — exponential backoff + jitter, unACKed command re-delivery on reconnect

### Feature Completeness
- [ ] Additional resource types: `storage_gb`, `network_egress_gb`, `active_users`, `gpu_hours` (4 more ledgers)
- [ ] Surge pack catalog (`surge_packs` + `surge_purchases` tables, purchasable mid-period)
- [ ] Plan management (`base_plans` CRUD, `plan_change_log`, propagation to active tenant wallets)
- [ ] Unary heartbeat fallback (`Heartbeat` RPC — currently only bidi streaming is implemented)
- [ ] Command execution model — agent-side persist-before-execute, idempotent re-delivery
- [ ] Agent versioning — minimum version policy, fleet version tracking via `agent_version` field
- [ ] `pending_commands` table — durable command persistence across gateway restarts

### Observability & Scale
- [ ] Structured metrics export (Prometheus — goroutines, TB latency, quota utilization)
- [ ] Distributed tracing (OpenTelemetry spans across gateway → Temporal → TB)
- [ ] Multi-region — TigerBeetle cross-datacenter replication, cross-region quota consistency

---

## What Was Deliberately Simplified in PoC vs. Original Plan

| Full Design | PoC Approach |
|-------------|--------------|
| mTLS + JWT auth | No auth |
| `BillingResetWorkflow` | Skipped |
| Four-eyes approval | Skipped |
| Missed-heartbeat timers | Skipped |
| Gauge pending transfers (void+create on each flush) | **Implemented** — `PendingGaugeIDs` tracked in workflow state |
| Adaptive flush interval | **Implemented** — 5s base, doubles to 60s idle, immediate at ≥ 20 |
| Soft limit alerts | Logged to event log; no webhook/email |
| `ContinueAsNew` | Skipped (workflows accumulate history indefinitely in PoC) |
