# cloud-credit-system

Ledger-backed quota enforcement for BYOC (Bring-Your-Own-Cloud) deployments.

A control plane that receives heartbeats from tenant workload clusters, records usage in
[TigerBeetle](https://tigerbeetle.com) (double-entry ledger), enforces hard quotas atomically,
and projects snapshots to PostgreSQL for reporting. [Temporal](https://temporal.io) serializes
per-tenant accounting, providing durable workflow state without Kafka.

This is a fully working PoC — it runs a scripted 7-step demo end-to-end with two tenants, five
simulated clusters, and real TigerBeetle enforcement.

---

## Architecture

```
Workload Clusters
    │  HeartbeatService (bidi gRPC stream — ConnectRPC)
    ▼
ConnectRPC Gateway  ──►  DedupCache (Valkey / in-memory)
    │  Signal(heartbeat)
    ▼
TenantAccountingWorkflow (Temporal — one per tenant, long-running)
    │  batch every 5–60s (adaptive)
    ├──►  SubmitTBBatch  ──►  TigerBeetle   ◄── hard limit enforcement
    └──►  UpdateQuotaSnapshots  ──►  PostgreSQL  ◄── reporting / dashboard
```

**Stack**: Go 1.26 · ConnectRPC (bidi streaming gRPC) · Temporal · TigerBeetle 0.16 · PostgreSQL 16 · Valkey · charmbracelet TUI

---

## The Thesis

This PoC proves a specific architectural claim: **TigerBeetle can be the atomic enforcement engine
for a SaaS quota system**, not just passive storage.

The surrounding architecture is designed to let TigerBeetle shine:

- **One sequential accounting lane per tenant** — `TenantAccountingWorkflow` gives each tenant an
  ordered, durable processing path. No cross-tenant ordering concerns. No shared hot paths.
- **Hard limits enforced at the ledger level** — `debits_must_not_exceed_credits` on tenant quota
  accounts. No application-side balance checks. No race windows. No bypasses.
- **Idempotency as a system property** — duplicate heartbeats are rejected at three independent
  layers before a TigerBeetle transfer is ever attempted.
- **PostgreSQL is read-only for enforcement** — quota snapshots are projections that may lag. They
  are never consulted in the reject/accept path. TigerBeetle is authoritative.

At the projected initial scale (~100 tenants × 1 hb/10s × 3 resources = ~30 TigerBeetle
transfers/sec), the system is dramatically underloaded. The batch sizing, adaptive flush, and
Temporal workflow boundaries give headroom far beyond any reasonable growth trajectory before
the architecture needs to change.

---

## Key Invariants

| ID  | Invariant |
|-----|-----------|
| I-1 | Single TigerBeetle writer per tenant — all TB writes via `TenantAccountingWorkflow` |
| I-2 | One heartbeat sequence processed at most once — three-layer defense |
| I-3 | Hard limit correctness from TigerBeetle only — `debits_must_not_exceed_credits` flag |
| I-4 | PostgreSQL snapshots are projections, may lag, never authoritative for enforcement |

### I-2: Three-Layer Dedup Defense

```
Layer 1 — Gateway DedupCache (Valkey/in-memory, TTL 5min)
    Rejects duplicate seq before signaling Temporal
Layer 2 — Workflow processedSeqs map (durable workflow state)
    Per-cluster monotonic seq tracker; catches anything that slipped past layer 1
Layer 3 — Deterministic BLAKE3 transfer IDs (clusterID ‖ seq ‖ ledgerID)
    TigerBeetle deduplicates at the transfer level; double-charging is physically impossible
```

BLAKE3 is used (not xxHash) because transfer IDs double as idempotency guards — tamper resistance matters.

---

## Screenshots

### Web Dashboard (`http://localhost:8080/sim`)

![Web Dashboard](docs/screenshots/web-dashboard.gif)

### Temporal Workflows (`http://localhost:8088`)

![Temporal UI](docs/screenshots/temporal-ui.gif)

### TUI Simulator (`make simulate`)

```
┌───────────────────────────────────────────────────────────────────────────┐
│  Cloud Credit System — Live Demo                               [00:42]    │
├──────────────────────────────────┬────────────────────────────────────────┤
│  TENANT: Acme Corp (pro)         │  SCENARIO TIMELINE                     │
│  Clusters: 3 active              │                                        │
│                                  │  ✓ 1. Provisioned tenant wallets       │
│  ┌─ CPU Hours ───────────────┐   │  ✓ 2. Clusters streaming heartbeats    │
│  │ ████████████░░░░░░  62%   │   │  ▶ 3. Recording usage...               │
│  │ used: 930K / limit: 1.5M  │   │    4. Drive to exhaustion              │
│  └───────────────────────────┘   │    5. Surge pack top-up                │
│  ┌─ Memory GB-Hours ─────────┐   │    6. Idempotency proof                │
│  │ ██████░░░░░░░░░░░░  35%   │   │    7. Final summary                    │
│  │ used: 350K / limit: 1M    │   │                                        │
│  └───────────────────────────┘   ├────────────────────────────────────────┤
│  ┌─ Active Nodes ────────────┐   │  EVENT LOG                             │
│  │ ████░░░░░░░░░░░░░░  20%   │   │  [A] ack seq=312 ACK'd                 │
│  │ used: 4 / limit: 20       │   │  [B] ack seq=151 ACK'd                 │
│  └───────────────────────────┘   │  [A] SOFT LIMIT — CPU at 80%           │
│                                  │  [A] ack seq=311 ACK'd                 │
├──────────────────────────────────┤                                        │
│  A (fast) us-east-1   ● live     ├────────────────────────────────────────┤
│  B (med)  eu-west-1   ● live     │  TB STATS (all tenants)                │
│  C (slow) ap-south-1  ● live     │  Transfers: 1,284                      │
└──────────────────────────────────┴────────────────────────────────────────┘
  [TAB] switch tenant  [N] next step  [SPACE] pause  [Q] quit
```

---

## Quick Start

**Prerequisites**: Go 1.24+ and Docker (that's it — no psql, no extra tooling).

```bash
# 1. Start infrastructure (Postgres, Temporal, TigerBeetle, Valkey)
make docker-up

# 2. Generate code (proto → Go, SQL → Go)
make generate

# 3. Build all binaries
make build

# 4. Run the full demo (server + TUI simulator)
make demo
```

Or run pieces individually:

```bash
make run        # server on :8080 (gateway + Temporal worker), applies migrations on startup
make simulate   # TUI simulator — connect to running server
```

Web dashboard: http://localhost:8080/sim
Temporal UI: http://localhost:8088
pprof: http://localhost:6061/debug/pprof/

> `make docker-down` passes `-v` — **destroys all volumes** (Postgres + TigerBeetle data).

---

## Demo Scenario

The 7-step scripted scenario runs two tenants against a real server:

| Step | What happens |
|------|-------------|
| 1. Provision | Register Acme Corp (pro) + Globex Inc (starter), load TigerBeetle wallets |
| 2. Connect | Register 5 clusters (3 Acme, 2 Globex), open bidi streams |
| 3. Record usage | Heartbeats flow at ~100 msg/s; quota bars fill from real server responses |
| 4. Exhaust | Cluster A ramps CPU to 5–10× normal; TB rejects at `debits_must_not_exceed_credits` |
| 5. Surge pack | Admin issues 500K CPU credits; Temporal Update confirms; cluster unblocks |
| 6. Idempotency | Replay 3 historical sequence numbers; `processedSeqs` dedupes them; balance unchanged |
| 7. Summary | All success criteria displayed |

---

## Project Layout

```
proto/creditsystem/v1/      ← Protobuf API definitions (source of truth)
gen/creditsystem/v1/        ← buf-generated Go + ConnectRPC stubs (do not edit)
internal/
  config/                   ← Env-based config (config.Load())
  domain/                   ← Resource types, transfer codes, account codes (no deps)
  db/                       ← pgx pool + embedded migration runner
  db/sqlcgen/               ← sqlc-generated query functions (do not edit)
  ledger/                   ← TigerBeetle client wrapper, account/transfer helpers
  accounting/               ← Signals, activities, workflows, worker (flat package)
    signals.go              ← HeartbeatSignal, QuotaAdjustmentSignal
    activities_tb.go        ← TBActivities: SubmitTBBatch, CreateTenantTBAccounts, etc.
    activities_pg.go        ← PGActivities: InsertTenant, InsertCluster, snapshots, etc.
    activity_refs.go        ← Nil pointer stubs for type-safe workflow.ExecuteActivity
    workflow_accounting.go  ← TenantAccountingWorkflow (long-running, one per tenant)
    workflow_provisioning.go← TenantProvisioningWorkflow, RegisterClusterWorkflow
    worker.go               ← NewClient, StartWorker
  compress/                 ← zstd ConnectRPC codec adapter
  gateway/                  ← ConnectRPC handlers, stream manager, dedup cache
  webui/                    ← Embedded web dashboard (served at /sim)
cmd/server/                 ← Server entrypoint (gateway + Temporal worker)
cmd/worker/                 ← Standalone Temporal worker (no HTTP gateway)
cmd/simulator/              ← charmbracelet TUI demo simulator
sql/migrations/             ← PostgreSQL schema (embedded in server binary)
sql/queries/                ← sqlc query files
docs/                       ← Design doc, one-pager, PoC status, TODO
```

---

## TigerBeetle Modeling

Each resource type gets its own **ledger** — prevents cross-resource transfers at the DB level.

**Account structure per tenant per ledger**:
```
Operator (code 1) ──[QUOTA_ALLOCATION 100]──► TenantQuota (code 2) ──[USAGE_RECORD 200]──► Sink (code 3)
```

- Operator and Sink are global (shared across tenants per ledger)
- TenantQuota has `DebitsMustNotExceedCredits | History` flags
- Transfer IDs for usage records: `blake3(clusterID ‖ seq ‖ ledgerID)` — deterministic, tamper-resistant

**Transfer codes**:

| Code | Meaning |
|------|---------|
| 100  | QUOTA_ALLOCATION — initial credit at period start |
| 101  | QUOTA_ADJUSTMENT — manual credit from admin |
| 102  | SURGE_PACK_CREDIT — surge pack top-up |
| 200  | USAGE_RECORD — heartbeat usage debit |
| 300  | PERIOD_CLOSE — drain at billing period end |

---

## Temporal Workflow Design

`TenantAccountingWorkflow` is long-running (one per tenant). It:
- Accumulates heartbeat signals into a batch
- Flushes to TigerBeetle every 5–60s (adaptive: doubles when idle, immediate at batch ≥ 20)
- Tracks `processedSeqs map[clusterID]uint64` for dedup (I-2 layer 2)
- Handles `QuotaAdjustmentSignal` with immediate flush (surge packs unblock the cluster fast)
- Responds to query handler `last_tb_ack` so the gateway can return current ack_sequence

Activities reference via **nil pointer method values** (compile-time type-safe, no strings):
```go
// activity_refs.go — nil pointers used only for method name resolution
var tbActivities *activities.TBActivities

// In workflow:
workflow.ExecuteActivity(ctx, tbActivities.SubmitTBBatch, input).Get(ctx, &result)
```

---

## Environment Variables

| Variable              | Default                                                | Description |
|-----------------------|--------------------------------------------------------|-------------|
| `POSTGRES_DSN`        | `postgres://postgres:postgres@localhost:5432/creditdb` | PostgreSQL connection |
| `TIGERBEETLE_ADDR`    | `127.0.0.1:3000`                                       | TigerBeetle address |
| `TEMPORAL_HOST`       | `localhost:7233`                                       | Temporal gRPC address |
| `TEMPORAL_NAMESPACE`  | `default`                                              | Temporal namespace |
| `LISTEN_ADDR`         | `:8080`                                                | Server listen address |
| `SERVER_URL`          | `http://localhost:8080`                                | Simulator target |
| `REDIS_ADDR`          | `localhost:6379`                                       | Valkey/Redis for shared dedup |

Copy `.env.example` and export, or set directly before running.

---

## Development

### Code generation

```bash
make generate       # buf generate (proto → Go) + sqlc generate (SQL → Go)
make proto-gen      # proto only
make sqlc-gen       # SQL only
```

Both `buf` and `sqlc` are registered as `go tool` deps — no separate install needed.

### Adding a resource type

1. Add constant in `internal/domain/resource.go` with a new ledger ID (never reuse)
2. Add proto field in `proto/creditsystem/v1/heartbeat.proto`
3. Update `heartbeatAmount()` in `internal/accounting/activities_tb.go`
4. Update `defaultCredits()` in `internal/gateway/provisioning_handler.go`
5. `make generate && make build`

### Proto changes

1. Edit `proto/creditsystem/v1/`
2. `make proto-gen` — regenerates `gen/`
3. Update handler code

### SQL changes

1. Add new file `sql/migrations/NNN_description.sql` (never edit existing migrations)
2. Edit `sql/queries/*.sql`
3. `make sqlc-gen` — regenerates `internal/db/sqlcgen/`
4. The new migration runs automatically on next server startup

### Running tests

```bash
make test             # all tests
make test-unit        # unit only (no external deps)
make test-integration # requires docker-compose services running
```

---

## Build Notes

- **CGo required** — TigerBeetle Go client uses an embedded Zig/C library
- `make generate` must run before first `make build` — `gen/` and `internal/db/sqlcgen/` are gitignored
- Migrations are embedded in the server binary; starting `./bin/server` applies them automatically

---

## Further Reading

| Document | Description |
|----------|-------------|
| [docs/Design_wip.md](docs/Design_wip.md) | Full architecture design doc (v0.4) |
| [docs/poc_1_pager.md](docs/poc_1_pager.md) | Stakeholder one-pager: thesis, demo flow, success criteria |
| [docs/TODO.md](docs/TODO.md) | PoC scope vs. full design — what's implemented and what remains |
| [docs/poc_status.md](docs/poc_status.md) | Bug fixes, invariant verification, simulator behavior |
