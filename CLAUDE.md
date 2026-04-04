# Cloud Credit System — CLAUDE.md

BYOC (Bring-Your-Own-Cloud) resource accounting and quota enforcement PoC.

## Project Summary

A control plane that receives heartbeats from tenant workload clusters, records usage in
TigerBeetle (double-entry ledger), enforces hard quotas atomically, and projects snapshots
to PostgreSQL for reporting.

**Stack**: Go · ConnectRPC (bidi streaming) · Temporal (workflow engine) · TigerBeetle
(financial ledger) · PostgreSQL (metadata + reporting) · charmbracelet (TUI simulator)

**Module**: `github.com/cshubhamrao/cloud-credit-system`

---

## Architecture

```
Workload Clusters
    │ HeartbeatService (bidi gRPC stream)
    ▼
ConnectRPC Gateway (cmd/server)
    │ Signal(heartbeat)
    ▼
TenantAccountingWorkflow (Temporal, one per tenant)
    │ batch every 30s
    ├──► SubmitTBBatch ──► TigerBeetle  ◄── hard limit enforcement
    └──► UpdateQuotaSnapshots ──► PostgreSQL  ◄── reporting layer
```

### Key Invariants

| ID  | Invariant |
|-----|-----------|
| I-1 | Single TigerBeetle writer per tenant — all TB writes via `TenantAccountingWorkflow` |
| I-2 | One heartbeat sequence processed at most once — `processedSeqs` in workflow + `DedupCache` in gateway |
| I-3 | Hard limit correctness from TigerBeetle only — `debits_must_not_exceed_credits` flag |
| I-4 | PostgreSQL snapshots are projections, may lag, never authoritative for enforcement |
| I-5 | All TigerBeetle transfer IDs must be deterministic or pre-generated in workflow code — Temporal activities are at-least-once, so `RandomID()` inside an activity causes a duplicate transfer on retry |

---

## Directory Layout

```
proto/creditsystem/v1/    ← Protobuf definitions (source of truth for API)
gen/creditsystem/v1/      ← buf-generated Go types + ConnectRPC stubs (do not edit)
internal/
  config/                 ← Env-based config
  domain/                 ← Resource types, transfer codes, account codes (no deps)
  db/                     ← pgx pool helper
  db/sqlcgen/             ← sqlc-generated query functions (do not edit)
  ledger/                 ← TigerBeetle client wrapper, account/transfer helpers
  accounting/             ← Flat package: signals, activities, workflows, worker (no sub-packages)
    signals.go            ← HeartbeatSignal, QuotaAdjustmentSignal, signal/query name constants
    activities_tb.go      ← TBActivities: SubmitTBBatch, CreateTenantTBAccounts, SubmitAllocationTransfers
    activities_pg.go      ← PGActivities: InsertTenant, InsertCluster, InsertTBAccountMapping, etc.
    activity_refs.go      ← nil pointer stubs for type-safe workflow.ExecuteActivity references
    workflow_accounting.go    ← TenantAccountingWorkflow (long-running, one per tenant)
    workflow_provisioning.go  ← TenantProvisioningWorkflow, RegisterClusterWorkflow
    worker.go             ← NewClient, StartWorker
  compress/               ← zstd ConnectRPC codec adapter (faster than gzip for small protos)
  gateway/                ← ConnectRPC handlers, stream manager, dedup cache
  webui/                  ← Embedded web UI handler (page.html served by server)
cmd/server/               ← Server entrypoint (gateway + Temporal worker)
cmd/worker/               ← Standalone Temporal worker (no HTTP gateway)
cmd/simulator/            ← Bubbletea TUI demo simulator
sql/migrations/           ← PostgreSQL schema
sql/queries/              ← sqlc query files
```

---

## Quick Start

### Prerequisites

- Go 1.24+
- Docker + Docker Compose

Both `buf` and `sqlc` are registered as `go tool` dependencies — no separate install needed:
```bash
go tool buf generate     # proto → Go
go tool sqlc generate    # SQL → Go
```

### 1. Start infrastructure

```bash
make docker-up
```

Starts: PostgreSQL 16, Temporal (auto-setup, 8h retention), TigerBeetle 0.16.11, Valkey.
Temporal UI available at http://localhost:8088.

> ⚠️ `make docker-down` passes `-v` — **destroys all volumes** (Postgres + TigerBeetle data).

### 2. Generate code

```bash
make generate
```

Runs `buf generate` (proto → Go + ConnectRPC) and `sqlc generate` (SQL → Go).

### 3. Build

```bash
make build
```

### 4. Run server

```bash
make run          # gateway + embedded Temporal worker
# or
./bin/server

make run-worker   # standalone Temporal worker only (no HTTP gateway)
```

### 5. Run demo simulator

```bash
make simulate
# or
./bin/simulator
```

### Full demo (one command)

```bash
make demo
```

---

## Development Workflow

### Proto changes

1. Edit files in `proto/creditsystem/v1/`
2. `make proto-gen` — regenerates `gen/`
3. Update handler code to match new types

### SQL changes

1. Edit `sql/migrations/001_initial.sql` (add new migration file for changes)
2. Edit `sql/queries/*.sql`
3. `make sqlc-gen` — regenerates `internal/db/sqlcgen/`

### Adding a resource type

1. Add a constant in `internal/domain/resource.go`
2. Assign a new ledger ID (never reuse an old one)
3. Add a proto field in `heartbeat.proto`
4. Update `heartbeatAmount()` in `tb_activities.go`
5. Add default credits in `provisioning_handler.go` → `defaultCredits()`

### Running tests

```bash
make test            # all tests
make test-unit       # unit only (no external deps)
make test-integration # requires docker-compose services running
```

---

## Environment Variables

| Variable            | Default                                              | Description |
|---------------------|------------------------------------------------------|-------------|
| `POSTGRES_DSN`      | `postgres://postgres:postgres@localhost:5432/creditdb` | PostgreSQL connection string |
| `TIGERBEETLE_ADDR`  | `127.0.0.1:3000`                                     | TigerBeetle address |
| `TEMPORAL_HOST`     | `localhost:7233`                                     | Temporal gRPC address |
| `TEMPORAL_NAMESPACE`| `default`                                            | Temporal namespace |
| `TEMPORAL_API_KEY`  | `""`                                                 | Temporal Cloud API key (enables TLS + auth when set) |
| `LISTEN_ADDR`       | `:8080`                                              | Server listen address |
| `REDIS_ADDR`        | `localhost:6379`                                     | Valkey/Redis address for dedup cache (falls back to in-memory if unreachable) |
| `SERVER_URL`        | `http://localhost:8080`                              | Simulator target URL |

---

## TigerBeetle Design Decisions

- **Ledger per resource type** — prevents cross-resource transfers at DB level
- **Global operator + sink accounts** per ledger (shared across tenants)
- **Tenant quota accounts** have `DebitsMustNotExceedCredits | History` flags
- **Deterministic transfer IDs** for all TB transfers (invariant I-5) — IDs are derived via BLAKE3
  with domain separators so Temporal activity retries reuse the same ID and TB returns `TransferExists`
  instead of creating a duplicate:
  - Usage records: `blake3("" || clusterID || seq || ledgerID)` via `DeriveTransferID` (unchanged)
  - Allocation / adjustment: `blake3("alloc" || tenantUUID || FlushSeqNo || ledgerID)` via `DeriveAllocationTransferID`
  - Gauge pending: `blake3("gauge-pending" || tenantUUID || FlushSeqNo || clusterUUID || ledgerID)`
  - Gauge void: `blake3("gauge-void" || tenantUUID || FlushSeqNo || clusterUUID || ledgerID)`
  - Provisioning allocations use `PeriodStartNs` cast to `uint64` as the sequence (unique per tenant lifecycle)
  - `FlushSeqNo` is a monotonic counter in `TenantAccountingState`, incremented before each `flushBatch`
    or `flushAdjustment` call and carried across Continue-As-New
  - BLAKE3 chosen intentionally: IDs double as idempotency guards against double-spending, so
    tamper resistance matters. Per [use_fast_data_algorithms](https://jolynch.github.io/posts/use_fast_data_algorithms/):
    use xxHash for pure speed/bitrot detection; use BLAKE3 when adversarial resistance is needed.

### Account codes

| Code | Type |
|------|------|
| 1    | operator (global per ledger) |
| 2    | tenant_quota (per tenant per resource) |
| 3    | sink (global per ledger) |

### Transfer codes

| Code | Meaning |
|------|---------|
| 100  | QUOTA_ALLOCATION — initial credit at period start |
| 101  | QUOTA_ADJUSTMENT — manual credit from admin |
| 102  | SURGE_PACK_CREDIT — surge pack top-up |
| 200  | USAGE_RECORD — heartbeat usage debit |
| 300  | PERIOD_CLOSE — drain at billing period end |

---

## Temporal Workflow Design

### TenantAccountingWorkflow

Long-running, one instance per tenant. State:

- `ProcessedSeqs map[clusterID]uint64` — dedup tracker (invariant I-2)
- `CurBatch []HeartbeatSignal` — accumulates between flushes
- `LastAck uint64` — highest TB-committed sequence (returned by query handler)

Flush trigger: adaptive timer (2s base, doubles to 60s when idle) OR immediate when batch reaches 150 OR on `QuotaAdjustmentSignal`.

### Activity Registration Pattern

Activities are registered by struct — the SDK derives names from method names via reflection:

```go
w.RegisterActivity(tbActs)  // registers SubmitTBBatch, CreateTenantTBAccounts, SubmitAllocationTransfers
w.RegisterActivity(pgActs)  // registers InsertTenant, InsertCluster, InsertTBAccountMapping, etc.
```

Workflows reference activities via **nil pointer method values** (type-safe, no strings):

```go
// activity_refs.go — nil pointers used only for compile-time name resolution
var tbActivities *activities.TBActivities
var pgActivities *activities.PGActivities

// In workflow:
workflow.ExecuteActivity(ctx, tbActivities.SubmitTBBatch, input).Get(ctx, &result)
```

The SDK resolves `tbActivities.SubmitTBBatch` (bound method on nil) to `"SubmitTBBatch"` via
reflection, matching the registration. Renaming the method is a compile error at the call site.

### Activity Task Queue

`tenant-accounting` — single task queue for all workflows and activities.

---

## PoC Scope (What's Simplified)

| Full Design | PoC Approach |
|------------|-------------|
| mTLS + JWT auth | No auth |
| BillingResetWorkflow | Skipped |
| Four-eyes approval | Skipped |
| Missed-heartbeat timers | Skipped |
| Gauge pending transfers | **Implemented**: void+create pending in one TB batch per flush; PendingGaugeIDs tracked in workflow state |
| Adaptive flush interval | **Implemented**: 2s base, doubles to 60s when idle; immediate flush at batch size ≥ 150 |
| Idempotent TB writes (I-5) | **Implemented**: all transfer IDs derived deterministically in workflow before activity dispatch; `RandomID()` eliminated from Temporal activity paths |
| Gateway signal retry | **Implemented**: 3-attempt exponential backoff (100ms/500ms/2s) in signal dispatcher; 2s backpressure timeout on `sigCh` send; 3-attempt PG retry in `resolveTenantID` |
| Soft limit alerts | Logged to stdout |

---

## Known Build Requirements

- **CGo required** for TigerBeetle Go client (uses embedded Zig/C library)
- Run `go mod tidy` after adding dependencies
- `buf generate` must run before `go build` (gen/ is in .gitignore)
- `sqlc generate` must run before `go build` (internal/db/sqlcgen/ is in .gitignore)

## Key Implementation Notes

### pgx + sqlc bridge
`sqlcgen` uses `database/sql` interfaces (`ExecContext`, `QueryRowContext`, etc.) but the rest
of the codebase uses `pgx/v5/pgxpool.Pool`. Bridge them with:
```go
import "github.com/jackc/pgx/v5/stdlib"
db := stdlib.OpenDBFromPool(pool)   // *sql.DB compatible with sqlcgen.New(db)
```
Both `PGActivities` and `AdminHandler` hold a `*sql.DB` (not `*pgxpool.Pool`) for sqlc queries.

### TigerBeetle types
- `types.Uint128` is `[16]byte` — copy bytes directly, never treat as `[2]uint64`
- Account result: `types.AccountExists` (not `AccountExistsWithSameFlags`)
- Transfer result: `types.CreateTransferResult` (not `TransferResult`)
- Activity option type: `activity.RegisterOptions` from `go.temporal.io/sdk/activity` (not `worker.RegisterOptions`)
