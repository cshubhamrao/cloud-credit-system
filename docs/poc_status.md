# PoC Implementation Status

## Summary

All planned PoC phases are complete. The simulator runs a full 7-step demo end-to-end with two
tenants, five clusters, real TigerBeetle enforcement, and live server observability.

---

## Bug Fixes

| Bug | Root Cause | Fix |
|-----|-----------|-----|
| BUG-1 (critical) | `heartbeat_handler.go` hardcoded `poc-tenant` — every signal went to a non-existent workflow | `resolveTenantID` method: PG lookup + `sync.Map` cache on `HeartbeatHandler` |
| BUG-2 (high) | `queryWorkflowState` returned nil quotas — TUI bars never updated from server | PG snapshot lookup after ack query; returned as `QuotaInfo` slice on each heartbeat response |
| BUG-3 (high) | PG `quota_snapshots` always zero — `buildSnapshotData` returned empty struct | Added `LookupAccountBalances` TB activity; called in `flushBatch` and `flushAdjustment` after every TB write |
| BUG-4 (medium) | Provisioning race — `RegisterCluster` could arrive before accounting workflow started | `workflowRun.Get(ctx, nil)` in provisioning handler blocks until child workflow is live |
| BUG-5 (medium) | Surge pack didn't update PG snapshot until next flush window | `flushAdjustment` now calls `LookupAccountBalances` → `UpdateQuotaSnapshots` after TB write |
| BUG-6 (low) | Dead code — unused `ackCh`, stale variables | Removed |

---

## Unit Tests

All pass: `go test ./internal/... -timeout 90s`

| Package | Key Tests | Invariant |
|---------|-----------|-----------|
| `internal/domain` | `TestLedgerID`, `TestAllResources`, `TestIsGauge` | baseline |
| `internal/ledger` | `TestDeriveTransferID_Deterministic`, `TestDeriveTransferID_Unique`, `TestDeriveTransferID_DifferentLedger` | **I-2** layer 3 — TB-level idempotency via deterministic transfer IDs |
| `internal/gateway` | `TestDedupCache_SameSeq`, `TestDedupCache_HigherSeq`, `TestDedupCache_DifferentClusters` | **I-2** layer 1 — gateway dedup before Temporal |
| `internal/accounting` | `TestTenantAccountingWorkflow_FlushOnTimer` | **I-1** — all TB writes via workflow |
| `internal/accounting` | `TestTenantAccountingWorkflow_Dedup` | **I-2** layer 2 — `processedSeqs` monotonic per cluster |
| `internal/accounting` | `TestTenantAccountingWorkflow_NoAppSideQuotaCheck` | **I-3** — workflow submits blindly to TB, no balance pre-check |
| `internal/accounting` | `TestTenantAccountingWorkflow_PGSnapshotFailure_NonFatal` | **I-4** — PG failure does not affect TB enforcement |
| `internal/accounting` | `TestTenantAccountingWorkflow_QuotaAdjustment_ImmediateFlush` | **I-1** — surge pack goes through workflow Update, not direct TB |
| `internal/accounting` | `TestTenantProvisioningWorkflow_HappyPath`, `TestRegisterClusterWorkflow_HappyPath` | **I-1** — provisioning creates single-writer workflow |

---

## Simulator

### Multi-tenant, multi-cluster demo

- **Acme Corp** (pro): 3 clusters (A: 1 hb/s, B: 1 hb/2s, C: 1 hb/4s) — us-east-1, eu-west-1, ap-south-1; 1.5M CPU / 1M MEM / 20 nodes credits
- **Globex Inc** (starter): 2 clusters (D: 1 hb/s, E: 1 hb/3s) — us-west-2, ca-central-1; 500K CPU / 500K MEM / 10 nodes credits
- TAB switches between tenant views; each has independent quota bars and event log

### Key behaviors demonstrated

| Feature | Implementation |
|---------|---------------|
| Hard-limit kill | On `STATUS_QUOTA_EXCEEDED`, cluster zeroes that resource (simulates k8s kill); revived on surge pack |
| [SPACE] pause | `atomic.Bool` on `ScenarioDriver` synced from TUI key handler to all cluster goroutines |
| Idempotency replay | Step 5 sends 3 real historical sequence numbers on live stream; `processedSeqs` dedupes them |
| Surge pack ack | Temporal Update (`UpdateIssueCredit`) blocks RPC until TigerBeetle confirms; response includes live balances |
| Workflow reuse | Re-provisioning same tenant handles `ChildWorkflowExecutionAlreadyStartedError` gracefully |

### Server Stats tab (Tab 2)

Debug HTTP server on `:6061`:
- `GET /debug/stats` — JSON: goroutines, heap alloc/sys MB, GC runs, last GC pause (µs), uptime, active stream count
- `GET /debug/pprof/*` — standard Go pprof endpoints

Simulator polls every 2s; third TUI tab shows Go runtime metrics + aggregated TB stats across all tenants.

---

## Design Invariants — Verified

| # | Invariant | Verification |
|---|-----------|-------------|
| **I-1** | Single TigerBeetle writer per tenant — all TB writes via `TenantAccountingWorkflow` | Temporal Update handler for surge pack still runs inside workflow. Unit tests: all TB activities called only from workflow activities, never from gateway. |
| **I-2** | One heartbeat seq processed at most once | Three-layer defence: `DedupCache` (gateway) → `processedSeqs` (workflow state) → deterministic BLAKE3 transfer ID (TigerBeetle). Each layer has a dedicated unit test. |
| **I-3** | Hard limit correctness from TigerBeetle only | `debits_must_not_exceed_credits` flag set on tenant quota accounts. `NoAppSideQuotaCheck` test explicitly submits a batch that would exceed credits and verifies no pre-check blocks it. |
| **I-4** | PG snapshots are projections, may lag, never authoritative | `PGSnapshotFailure_NonFatal` test: PG activity fails for all 5 retry attempts, TB batch still submitted, workflow survives. PG is never read in the enforcement path. |

---

## What Was Simplified (vs. full design)

| Full Design | PoC Approach |
|------------|-------------|
| mTLS + JWT auth | No auth |
| BillingResetWorkflow | Skipped |
| Adaptive flush interval | Fixed 5s |
| Four-eyes approval for credits | Skipped |
| Missed-heartbeat timers | Skipped |
| Gauge resources (two-phase pending) | `active_nodes` tracked as simple transfer |
| Soft limit alerts | Logged to event log; no webhook |

---

## Running the Demo

```bash
make docker-up     # PG 16 + Temporal + TigerBeetle 0.16
make build
make run           # server :8080 + debug :6061
make simulate      # TUI (TAB = switch tenant/server tab, SPACE = pause)
```

Temporal UI: http://localhost:8088
pprof: http://localhost:6061/debug/pprof/
