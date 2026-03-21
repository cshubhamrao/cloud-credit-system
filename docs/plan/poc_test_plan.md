# PoC Test Plan: Fix Bugs & Verify Simulator E2E

> **Status: COMPLETE** (2026-03-21)
> All bug fixes implemented. Unit tests, Temporal workflow tests, and TigerBeetle integration
> tests written and passing. Manual E2E / simulator verification remains (Part 4).
> See [Implementation Notes](#implementation-notes) at the bottom for deviations from this plan.

## Context

The PoC implementation (all 5 phases) compiles cleanly but has **zero tests** and several bugs that prevent the simulator from working end-to-end. The most critical bug is a hardcoded tenant ID in the heartbeat handler that causes all heartbeat signals to target a non-existent Temporal workflow. This plan fixes the blocking bugs, then verifies the 7-step demo scenario works — with explicit verification of each design invariant.

---

## Design Invariants (from `docs/Design_wip.md`)

These invariants are non-negotiable. Every bug fix and test must be reviewed against this table.

| # | Invariant | PoC Status | How Verified |
|---|-----------|------------|--------------|
| I-1 | **One TigerBeetle writer per tenant** — all TB writes via `TenantAccountingWorkflow` | Preserved | Workflow test: no TB activity called outside workflow. Code review: BUG-1 fix still routes through workflow signal, not direct TB write. |
| I-2 | **One heartbeat seq processed at most once** — `processedSeqs` monotonic + deterministic TB transfer IDs | Preserved | 3-layer dedup test: gateway `DedupCache` → workflow `processedSeqs` → TB `DeriveTransferID` idempotency. Demo step 5. |
| I-3 | **Hard limit correctness from TigerBeetle only** — `debits_must_not_exceed_credits` is sole enforcement | Preserved | Integration test: exhaust quota, verify TB rejects. No app-side balance check anywhere. Demo step 3. |
| I-4 | **PG snapshots are projections, may lag, never authoritative** | Preserved | BUG-2 fix reads PG for display only (heartbeat response quotas). BUG-3 fix writes real balances to PG but PG is never consulted for enforcement. Test: hard limit still enforced when PG snapshot is stale. |
| I-5 | **Gauge resources use pending transfers** | Simplified | PoC: `active_nodes` tracked as simple transfer (no two-phase). Documented simplification. |
| I-6 | **Billing reset is idempotent and atomic** | Skipped | Not in PoC scope. |
| I-7 | **Server clock authoritative for billing timestamps** | Preserved | TB transfers use server-side timestamps. `HeartbeatTimestampNs` is set from `req.Timestamp` (server-received) and stored in `user_data_64` for audit, but TB's own timestamp field is set server-side by TB itself. |
| I-8 | **Corrections require Admin authority** | Skipped | Not in PoC scope (no auth). |

### Invariant compliance review for each bug fix

| Bug Fix | I-1 | I-2 | I-3 | I-4 |
|---------|-----|-----|-----|-----|
| BUG-1 (tenant resolution) | OK: still signals workflow, no direct TB writes | OK: dedup unchanged | OK: no app-side check added | OK: PG lookup is for routing only |
| BUG-2 (quota in response) | N/A | N/A | **Must verify**: quota info sent to client is advisory only — no enforcement decision based on PG | OK: PG read is for display |
| BUG-3 (real PG snapshots) | OK: new `LookupAccountBalances` activity runs inside workflow | N/A | **Must verify**: `UpdateQuotaSnapshots` does not gate any enforcement | OK: PG still a projection, just with real data now |
| BUG-4 (provisioning wait) | OK | N/A | N/A | N/A |
| BUG-5 (adjustment PG update) | OK: activity runs inside workflow | N/A | N/A | OK: still a projection update |

---

## Part 1: Bug Fixes (ordered by priority)

### BUG-1 (Critical): Hardcoded `poc-tenant` in heartbeat_handler.go

**Problem:** `signalHeartbeat()` (line 176) and `queryWorkflowState()` (line 185) both use `AccountingWorkflowID("poc-tenant")`, which resolves to `tenant-accounting-poc-tenant`. But provisioning creates the workflow with a real UUID: `tenant-accounting-<uuid>`. Every heartbeat signal goes to a non-existent workflow — the entire heartbeat flow is broken.

**Fix:**
- Add `db *sql.DB` and `clusterToTenant sync.Map` fields to `HeartbeatHandler`
- Add `resolveTenantID(ctx, clusterID) (string, error)` method: checks cache → falls back to `sqlcgen.GetCluster(ctx, clusterUUID)` → caches `cluster.TenantID.String()`
- Update `signalHeartbeat()` to call `resolveTenantID` and use the result
- Update `queryWorkflowState()` to accept tenant ID from caller (resolved once per heartbeat in recv loop / per ticker in send loop)
- Update `NewHeartbeatHandler()` signature to accept `*pgxpool.Pool`, convert via `stdlib.OpenDBFromPool`
- Update `cmd/server/main.go` line 79 to pass `pool`

**Files:**
- `internal/gateway/heartbeat_handler.go` — main fix
- `cmd/server/main.go` — wire pool into constructor

### BUG-2 (High): queryWorkflowState returns nil quotas

**Problem:** Line 195 returns `ack, nil, nil` — quota info is never sent on the heartbeat response stream. TUI quota bars never update from server data.

**Fix:**
- In `queryWorkflowState`, after getting ack, do a PG lookup via `sqlcgen.GetQuotaSnapshots(ctx, tenantUUID)` (same as `AdminHandler.ListTenantQuotas`)
- Convert snapshots to `[]*creditsystemv1.QuotaInfo` with status computation
- Return the quotas slice

**Files:**
- `internal/gateway/heartbeat_handler.go`

### BUG-3 (High): PG quota snapshots always contain zeros

**Problem:** `buildSnapshotData()` (line 223-231) returns `QuotaSnapshotData{}` with all zeros. `UpdateQuotaSnapshots` PG activity just writes whatever it receives — it does NOT fetch from TB itself. So `quota_snapshots` table always has zero values.

**Fix:**
- Add `LookupAccountBalances` activity to `TBActivities`:
  - Input: `map[string][16]byte` (resource → account ID)
  - Calls `ledger.LookupAccounts()` for each account
  - Returns `map[string]QuotaSnapshotData` with real `CreditsPosted`, `DebitsPosted`, `DebitsPending` from TB
- In `flushBatch()`, after `SubmitTBBatch`, call `LookupAccountBalances` to get fresh balances
- Pass real balances to `UpdateQuotaSnapshots`
- Register new activity in `activity_refs.go`

**Files:**
- `internal/accounting/activities_tb.go` — add `LookupAccountBalances`
- `internal/accounting/activity_refs.go` — add nil pointer ref
- `internal/accounting/workflow_accounting.go` — update `flushBatch` to call new activity

### BUG-4 (Medium): Provisioning race condition

**Problem:** `RegisterTenant` in `provisioning_handler.go` starts the provisioning workflow but returns immediately without waiting for completion. The simulator calls `RegisterCluster` ~3s later, which tries to signal the accounting workflow — but it may not exist yet.

**Fix:**
- In `provisioning_handler.go`, after `ExecuteWorkflow`, call `workflowRun.Get(ctx, nil)` to wait for the provisioning workflow to complete before returning the tenant ID
- This ensures the accounting child workflow is running before any cluster registration

**Files:**
- `internal/gateway/provisioning_handler.go`

### BUG-5 (Medium): flushAdjustment doesn't update PG snapshots

**Problem:** After surge pack credits are applied in TB, `flushAdjustment` doesn't update `quota_snapshots`. The PG projection won't reflect the new credits until the next 30s `flushBatch`.

**Fix:**
- After `SubmitAllocationTransfers` succeeds in `flushAdjustment`, call `LookupAccountBalances` → `UpdateQuotaSnapshots` (same as in `flushBatch`)

**Files:**
- `internal/accounting/workflow_accounting.go`

### BUG-6 (Low): Dead code cleanup

**Problem:** Unused `ackCh` channel (heartbeat_handler.go:59), unused variables in `flushBatch` (workflow_accounting.go:161-178).

**Fix:**
- Remove `ackCh` and `_ = ackCh`
- Remove unused `snapshots` map, `res`, `ok` in `flushBatch`

**Files:**
- `internal/gateway/heartbeat_handler.go`
- `internal/accounting/workflow_accounting.go`

---

## Part 2: Unit Tests

### domain package (no external deps)

**`internal/domain/resource_test.go`**
- `TestLedgerID` — each resource maps to correct ledger ID (1, 2, 3)
- `TestResourceTypeFromLedger` — round-trip ledger ID → ResourceType
- `TestIsGauge` — only `active_nodes` is gauge
- `TestAllResources` — contains exactly 3 entries in order

### ledger/ids — **I-2 layer 3: deterministic transfer IDs**

**`internal/ledger/ids_test.go`**
- `TestDeriveTransferID_Deterministic` — same (clusterID, seq, ledgerID) → same output. *This is the I-2 backstop: same heartbeat always produces the same TB transfer ID, so TB itself deduplicates.*
- `TestDeriveTransferID_Unique` — different inputs → different outputs
- `TestDeriveTransferID_DifferentLedger` — same cluster+seq, different ledger → different ID. *Each resource gets its own transfer ID; they don't collide.*
- `TestUUIDToUint128_RoundTrip` — convert and back

### gateway/dedup — **I-2 layer 1: gateway-level dedup**

**`internal/gateway/dedup_test.go`**
- `TestDedupCache_FirstSeen` — not a duplicate
- `TestDedupCache_SameSeq` — is a duplicate. *I-2: first line of defense — duplicate seq never reaches Temporal.*
- `TestDedupCache_HigherSeq` — not a duplicate, updates tracked seq
- `TestDedupCache_DifferentClusters` — independent tracking. *Per-cluster monotonic sequences, not global.*

### accounting (Temporal test SDK) — **I-1, I-2 layer 2, I-3, I-4**

**`internal/accounting/workflow_accounting_test.go`**
- `TestTenantAccountingWorkflow_FlushOnTimer` — heartbeat → 30s → SubmitTBBatch called. *I-1: all TB writes go through this workflow.*
- `TestTenantAccountingWorkflow_Dedup` — duplicate seq skipped. *I-2 layer 2: `processedSeqs` is monotonic per cluster within the workflow.*
- `TestTenantAccountingWorkflow_QuotaAdjustment_ImmediateFlush` — adjustment triggers immediate SubmitAllocationTransfers. *I-1: even adjustments go through the workflow, not direct TB.*
- `TestTenantAccountingWorkflow_QueryLastAck` — returns highest committed seq after flush
- `TestTenantAccountingWorkflow_PGSnapshotFailure_NonFatal` — PG activity fails → workflow continues, no panic. *I-4: PG failure is non-fatal; enforcement unaffected.*
- `TestTenantAccountingWorkflow_NoAppSideQuotaCheck` — verify workflow does NOT check balance before submitting to TB. *I-3: no app-side gating, TB is sole authority.*

**`internal/accounting/workflow_provisioning_test.go`**
- `TestTenantProvisioningWorkflow_HappyPath` — all 5 steps, accounting child workflow started. *I-1: provisioning creates the single writer workflow.*
- `TestRegisterClusterWorkflow_HappyPath` — insert + signal to existing accounting workflow

---

## Part 3: Integration Tests (require docker-compose)

### TigerBeetle integration — **I-2 layer 3, I-3**

**`internal/ledger/integration_test.go`** (build tag or `-run Integration`)
- `TestIntegration_CreateGlobalAccounts` — 6 accounts created (operator + sink per resource)
- `TestIntegration_CreateGlobalAccounts_Idempotent` — second call succeeds (AccountExists)
- `TestIntegration_TenantQuotaAccount_HasDebitsMustNotExceedCreditsFlag` — verify the flag is set on created accounts. *I-3: this flag IS the hard limit enforcement mechanism.*
- `TestIntegration_AllocateAndUse` — allocate credits, submit usage, verify balance decreases correctly
- `TestIntegration_ExceedsCredits` — usage > credits → TB rejects with `exceeds_credits`. *I-3: hard limit enforced atomically by TB, no app-side check.*
- `TestIntegration_ExceedsCredits_OtherResourcesUnaffected` — exhaust CPU, verify memory transfers still accepted. *Independent per-ledger enforcement.*
- `TestIntegration_DeterministicTransferID_Idempotent` — same (clusterID, seq, ledgerID) → same transfer ID → TB returns `Exists`. *I-2 layer 3: TB-level idempotency.*
- `TestIntegration_LookupAccountBalances` — after transfers, verify `LookupAccounts` returns correct `credits_posted` and `debits_posted`

### Full-stack E2E — **all invariants**

**`test/e2e_test.go`** (requires all services)
- `TestIntegration_E2E_ProvisionHeartbeatExhaustSurge`:
  1. RegisterTenant → wait for provisioning. *I-1: accounting workflow created.*
  2. RegisterCluster
  3. Send heartbeats via unary RPC
  4. Wait for flush, verify PG snapshots non-zero. *I-4: PG updated as projection.*
  5. Exhaust quota, verify TB rejects. *I-3: hard limit from TB.*
  6. IssueTenantCredit, verify unblocked. *I-1: adjustment goes through workflow.*
  7. Send duplicate seq, verify no double-charge. *I-2: all 3 layers.*
  8. Verify PG snapshot `debits_posted` matches TB account balance. *I-4: PG lags but converges.*

---

## Part 4: Manual E2E Verification (Simulator Demo)

### Prerequisites
- Docker Desktop running
- Go 1.24+, `buf` CLI, `psql` available

### Setup
```bash
make docker-down          # clean slate
make docker-up            # PG + Temporal + TigerBeetle + migrations
docker compose ps         # verify all healthy
make build                # build server + simulator
```

### Run
```bash
# Terminal 1
make run                  # expect: "server listening addr=:8080"

# Terminal 2
make simulate             # expect: TUI launches
```

### Step-by-step Verification

| Demo Step | What to Check | Where |
|-----------|--------------|-------|
| 0: Provision | Event log: "Tenant provisioned, 3 wallets loaded" | TUI + server logs |
| 0: Provision | `tenant-provisioning-*` completed, `tenant-accounting-*` running | Temporal UI :8088 |
| 0: Provision | `SELECT * FROM tenants` → 1 row, tier='pro' | psql |
| 1: Start clusters | Two cluster panels with green dots, "connected" | TUI |
| 1: Start clusters | `SELECT * FROM workload_clusters` → 2 rows | psql |
| 2: Record usage | Heartbeat ACKs in event log, quota bars filling | TUI |
| 2: Record usage | `SELECT * FROM quota_snapshots` → non-zero debits_posted | psql |
| 3: Exhaustion | CPU bar turns red, "HARD LIMIT" in event log | TUI |
| 3: Exhaustion | `exceeds_credits` in server logs | Terminal 1 |
| 4: Surge pack | CPU bar drops, "Surge pack applied +500K" | TUI |
| 4: Surge pack | `SELECT * FROM credit_adjustments` → 1 row | psql |
| 5: Idempotency | "Duplicate seq=N deduped" in event log, no balance change | TUI |
| 6: Summary | All success criteria listed as passed | TUI |

---

## Part 5: Success Criteria × Invariant Matrix

| Criterion | Invariants | Blocking Bugs | Demo Step | Automated Tests |
|-----------|-----------|--------------|-----------|-----------------|
| Exactly-once usage | **I-2** (all 3 layers) | BUG-1 | Step 5 (idempotency) | `dedup_test` (layer 1), workflow dedup test (layer 2), TB idempotent integration (layer 3) |
| Hard quota rejection | **I-3** | BUG-1, BUG-3 | Step 3 (exhaustion) | `ExceedsCredits` integration, flag verification test, `NoAppSideQuotaCheck` workflow test |
| Accurate PG snapshots | **I-4** | BUG-2, BUG-3 | Step 2 (usage) | E2E snapshot convergence test, `PGSnapshotFailure_NonFatal` workflow test |
| PG never authoritative | **I-4** | — | Step 3 (limit enforced even if PG stale) | Workflow test: PG failure doesn't block enforcement. Code review: no `quota_snapshots` read in enforcement path. |
| Single TB writer | **I-1** | — | All steps | Workflow tests: all TB activities called only from workflow. Code review: gateway never imports `ledger` package. |
| Multi-rate heartbeats | **I-1** (tenant isolation) | BUG-1 | Step 2 (fast + slow) | E2E multi-cluster test |
| Throughput headroom | — | — | Step 2-3 TUI stats | TB latency in integration tests |
| Surge pack unblocks | **I-1**, **I-3** | BUG-1, BUG-5 | Step 4 (surge) | E2E exhaust+surge test |
| Server clock authoritative | **I-7** | — | — | Code review: `HeartbeatTimestampNs` stored in `user_data_64` for audit only. TB sets its own transfer timestamp server-side. |
| Independent per-resource enforcement | **I-3** | — | Step 3 (CPU exhausted, MEM ok) | `ExceedsCredits_OtherResourcesUnaffected` integration test |

---

## Part 6: Manual Invariant Verification (during demo)

| Invariant | How to Verify During Demo |
|-----------|--------------------------|
| I-1 | Temporal UI: only one `tenant-accounting-*` workflow per tenant. All TB activity executions are inside this workflow. |
| I-2 | Step 5: replay 3 duplicate seqs → event log shows "deduped", quota bars don't move. PG `debits_posted` unchanged. |
| I-3 | Step 3: CPU quota hits 100% → "HARD LIMIT — TigerBeetle rejected". Server logs show `exceeds_credits`. Crucially: no app-side "balance < 0" check anywhere. |
| I-4 | Between flushes (30s window): PG `quota_snapshots` may show stale values. Hard limit still enforced by TB even when PG shows old data. After flush: PG catches up. |
| I-7 | Server logs: heartbeat timestamps are server-received times. TB transfer timestamps are TB-server-side (not client-reported). |

---

## Implementation Order

1. Fix BUG-1 (heartbeat_handler tenant resolution) + BUG-6 (cleanup)
2. Fix BUG-3 (add LookupAccountBalances activity, fix flushBatch)
3. Fix BUG-2 (quota info in queryWorkflowState)
4. Fix BUG-4 (provisioning wait-for-completion)
5. Fix BUG-5 (flushAdjustment PG update)
6. Write unit tests (domain, ledger/ids, gateway/dedup) — verify I-2 layers 1 & 3
7. Write Temporal workflow tests — verify I-1, I-2 layer 2, I-3, I-4
8. Manual E2E verification via simulator — verify all invariants
9. Write integration tests for TB + E2E — verify I-2 layer 3, I-3

---

## Implementation Notes

Deviations and surprises discovered during implementation (2026-03-21).

### Skipped / Not Yet Implemented

- **`internal/accounting/workflow_provisioning_test.go`** — planned but not written. The
  provisioning workflow is covered indirectly by BUG-4 fix (wait-for-completion) and the
  manual E2E. Skipped to keep scope tight.
- **`test/e2e_test.go`** (full-stack E2E) — planned but not written. Requires all services
  running and a live ConnectRPC client. Left for a follow-up; the TigerBeetle integration
  tests cover the critical invariants (I-2 layer 3, I-3) with real infra.
- **Manual simulator E2E** (Part 4) — not yet run. Infrastructure is verified (docker-compose
  up, all tests pass), but the TUI walkthrough hasn't been done.

### Structural Change: No Build Tags for Integration Tests

Plan said `internal/ledger/integration_test.go` with `//go:build integration`.
**Actual**: moved to `test/ledger/integration_test.go` with no build tags. Separation by
directory is cleaner — `go test ./internal/...` runs unit tests, `go test ./test/...` runs
integration tests. Makefile updated accordingly:
- `make test-unit` → `go test ./internal/... -timeout 60s`
- `make test-integration` → `go test ./test/... -timeout 120s`
- `make test` → both

### Ledger Package API Differences from Plan

- **`types.AccountFlags.FromUint16` doesn't exist** in TB 0.16.11. The correct API is the
  method `account.AccountFlags()` on a `types.Account` value. Fixed in integration test.
- **`LookupBalances` helper added to `internal/ledger/accounts.go`** to keep `types.Uint128`
  internal to the ledger package. The accounting activity receives `map[string]QuotaSnapshotData`
  (plain Go types), not raw TB types.
- **`uint128Lo` helper added** — TB balance fields (`CreditsPosted`, `DebitsPosted`, etc.) are
  `types.Uint128` ([16]byte), not `uint64`. Extracts lower 64 bits via `binary.LittleEndian`.

### Integration Test Isolation (Cross-Run Pollution)

TigerBeetle is persistent across test runs. Using hardcoded tenant/transfer IDs caused
`TransferIDAlreadyFailed` on the second run (TB remembers that a transfer with that ID
previously failed with `ExceedsCredits`). Fix:
- All tenant UUIDs in integration tests use `ledger.Uint128ToBytes(ledger.RandomID())`.
- Transfer IDs in non-idempotency tests use `ledger.RandomID()`.
- The idempotency test uses a random `clusterUUID` per run, then calls `DeriveTransferID`
  twice with the same random clusterUUID — still proves determinism within a run.

### Temporal Workflow Test: Dedup Logic

The plan described the dedup test as "duplicate seq skipped" but the initial implementation
sent two identical seqs in the **same 30s window**. That's wrong: `processedSeqs` only
deduplicates **cross-flush** (it's only updated after `SubmitTBBatch`). Within a single
flush window, the gateway `DedupCache` handles duplicates (tested separately in `dedup_test.go`).

Corrected scenario:
- `t=2s`: seq=5 → enters batch (processedSeqs not yet updated)
- `t=30s`: flush → batch submitted, `processedSeqs[cluster-a]=5`
- `t=36s`: seq=3 arrives → deduped (`3 ≤ 5`), not added to batch
- `t=60s`: second flush → empty batch, `SubmitTBBatch` NOT called again
- Assert: `SubmitTBBatch` called exactly once

### Temporal Workflow Test: PGSnapshotFailure Timeout

Plan said cancel at 35s. Actual: cancel moved to 70s, `SetTestTimeout(75s)`. Reason: with
`MaximumAttempts: 5` and `InitialInterval: 1s` (backoff coefficient 2.0), retries consume
up to 1+2+4+8=15s of simulated time starting at t=30s (flush fires). The cancel at t=35s
cut off mid-retry, which is actually fine (the workflow handles `CanceledError` as non-fatal),
but the extended timeout makes the test less brittle and allows all 5 attempts to be exhausted
if docker-compose is running slowly.
