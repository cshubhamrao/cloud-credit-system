# BYOC Cloud Credit System — Go PoC Implementation Plan

**Status: COMPLETE** — All 5 phases implemented. Build is clean (`go build ./...` passes).
Generated code (`gen/`, `internal/db/sqlcgen/`) requires `make generate` before building from scratch.

## Context

This is a greenfield PoC for a BYOC (Bring-Your-Own-Cloud) resource accounting and quota enforcement system. The goal is to prove to stakeholders that:

- Usage heartbeats become durable, auditable ledger entries
- Hard quota enforcement happens atomically in TigerBeetle, not app-side checks
- Slow and fast tenants coexist without destabilizing each other
- Reporting via PostgreSQL projections remains simple and queryable
- The architecture has throughput headroom far beyond initial load

**Design doc**: `docs/Design_wip.md` (v0.4) — authoritative reference for all decisions
**Stakeholder one-pager**: `docs/poc_1_pager.md` — defines demo flow and success criteria
**Implementation**: feature-dev plugin for guided development

---

## PoC Scope

### In Scope (3 resource types)
| Resource | Category | Ledger ID |
|----------|----------|-----------|
| `cpu_hours` | Cumulative (delta per heartbeat) | 1 |
| `memory_gb_hours` | Cumulative (delta per heartbeat) | 2 |
| `active_nodes` | Point-in-time gauge | 3 |

### Simplified for PoC
| Full Design | PoC Approach |
|------------|-------------|
| mTLS + JWT auth | No auth |
| Signed commands | Skip |
| BillingResetWorkflow | Skip |
| Four-eyes approval | Skip |
| Missed-heartbeat timers | Skip |
| ContinueAsNew | Skip |
| Gauge pending transfers | active_nodes reported but tracked as simple transfer (skip two-phase) |
| Adaptive flush interval | Fixed 30s flush |
| Soft limit alerts | Log to stdout |

### Invariants Preserved (Non-Negotiable)
- **I-1**: Single TigerBeetle writer per tenant (all TB writes via TenantAccountingWorkflow)
- **I-2**: One heartbeat sequence processed at most once (processedSeqs + deterministic transfer IDs)
- **I-3**: Hard limit correctness from TigerBeetle only (debits_must_not_exceed_credits)
- **I-4**: PostgreSQL snapshots are projections, may lag, never authoritative for enforcement
- **I-7**: Server clock authoritative for billing timestamps

---

## Project Structure

```
cloud-credit-system/
├── buf.gen.yaml
├── buf.yaml
├── go.mod
├── Makefile
├── docker-compose.yml              # PostgreSQL 16 + Temporal + TigerBeetle
│
├── proto/creditsystem/v1/
│   ├── common.proto                 # Status enum, QuotaStatus, SignedCommand
│   ├── heartbeat.proto              # HeartbeatService (bidi stream + unary fallback)
│   ├── admin.proto                  # AdminService (IssueTenantCredit, ListTenantQuotas)
│   └── provisioning.proto           # ProvisioningService (RegisterTenant, RegisterCluster)
│
├── gen/creditsystem/v1/             # buf-generated Go + ConnectRPC code
│
├── sql/
│   ├── migrations/001_initial.sql   # Core tables: tenants, clusters, quota_configs, etc.
│   ├── queries/*.sql                # sqlc query files
│   └── sqlc.yaml
│
├── internal/
│   ├── config/config.go             # Env-based config
│   ├── domain/
│   │   ├── resource.go              # ResourceType enum, ledger mapping
│   │   ├── transfer_codes.go        # 100, 101, 102, 200, 300
│   │   └── account_codes.go         # 1=operator, 2=tenant_quota, 3=sink
│   ├── db/
│   │   ├── postgres.go              # pgx pool
│   │   └── sqlcgen/                 # sqlc output
│   ├── ledger/
│   │   ├── client.go                # TigerBeetle client wrapper
│   │   ├── accounts.go              # Create operator/tenant_quota/sink accounts
│   │   ├── transfers.go             # Build usage/allocation/adjustment transfers
│   │   └── ids.go                   # Deterministic ID generation (blake3)
│   ├── accounting/                  # Flat package — no sub-packages, no import cycles
│   │   ├── signals.go               # HeartbeatSignal, QuotaAdjustmentSignal, signal/query name constants
│   │   ├── activities_tb.go         # TBActivities: SubmitTBBatch, CreateTenantTBAccounts, SubmitAllocationTransfers
│   │   ├── activities_pg.go         # PGActivities: InsertTenant, InsertCluster, InsertTBAccountMapping, etc.
│   │   ├── activity_refs.go         # Nil pointer stubs for type-safe workflow.ExecuteActivity references
│   │   ├── workflow_accounting.go   # TenantAccountingWorkflow (long-running, one per tenant)
│   │   ├── workflow_provisioning.go # TenantProvisioningWorkflow, RegisterClusterWorkflow
│   │   └── worker.go                # NewClient, StartWorker
│   └── gateway/
│       ├── heartbeat_handler.go     # ConnectRPC HeartbeatService (bidi stream + unary fallback)
│       ├── stream_manager.go        # Per-cluster stream tracking, send loop, connection map
│       ├── admin_handler.go         # ConnectRPC AdminService
│       ├── provisioning_handler.go  # ConnectRPC ProvisioningService
│       └── dedup.go                 # In-memory sequence dedup cache
│
├── cmd/
│   ├── server/main.go               # Gateway + Temporal worker entrypoint
│   └── simulator/main.go            # CLI: simulates workload clusters for demo
│
└── scripts/
    ├── setup-tigerbeetle.sh
    ├── setup-db.sh
    └── demo.sh
```

---

## Phased Build Order

### Phase 0: Scaffolding & Infrastructure
**Files**: `go.mod`, `Makefile`, `docker-compose.yml`, `buf.yaml` (v2), `buf.gen.yaml` (v2), `scripts/*`

- Docker Compose: PostgreSQL 16, `temporalio/auto-setup:1.24.2`, TigerBeetle 0.16.11
- Go 1.24, buf v2 for proto codegen (remote plugins), sqlc for SQL codegen
- buf config: `buf.yaml` v2 with `modules[].path = proto`; `buf.gen.yaml` v2 with `inputs[].directory = proto`
- Makefile targets: `proto-gen`, `sqlc-gen`, `generate`, `build`, `test`, `run`, `docker-up`, `simulate`, `demo`

**Key deps**: `connectrpc.com/connect v1.x`, `github.com/tigerbeetle/tigerbeetle-go v0.16.11`,
`go.temporal.io/sdk v1.33.x`, `github.com/jackc/pgx/v5 v5.x`, `google.golang.org/protobuf v1.36.x`,
`github.com/google/uuid v1.6.x`, `golang.org/x/net` (h2c), `github.com/zeebo/blake3` (fast hash for transfer IDs)

> Use `@latest` for all `go get` commands — never pin pseudo-versions manually. Always run `go mod tidy` after adding deps.

**Verify**: `docker compose up -d` starts all services, `make generate && go build ./...` compiles

### Phase 1: Domain + Protobuf + Database
**Files**: `internal/domain/*`, `proto/creditsystem/v1/*.proto`, `sql/*`, `internal/db/*`
**Generated**: `gen/creditsystem/v1/` (buf), `internal/db/sqlcgen/` (sqlc)

Domain constants:
- 3 resource types with ledger IDs
- Transfer codes: 100 (QUOTA_ALLOCATION), 101 (QUOTA_ADJUSTMENT), 102 (SURGE_PACK_CREDIT), 200 (USAGE_RECORD)
- Account codes: 1 (operator), 2 (tenant_quota), 3 (sink)
- Status enum: STATUS_UNSPECIFIED=0, STATUS_OK=1, STATUS_QUOTA_WARNING=2, STATUS_QUOTA_EXCEEDED=3

Proto management: buf v2 BSR module `buf.build/cshubhamrao/cloud-credit-system`.
Run `buf dep update` in project root to refresh `buf.lock`.
Run `buf lint` to validate; `buf generate` to regenerate Go code.

Protobuf:
- `HeartbeatService`: `HeartbeatStream(stream HeartbeatRequest) returns (stream HeartbeatResponse)` + `Heartbeat` unary fallback
- `HeartbeatRequest`: cluster_id, timestamp, cpu_milliseconds_delta, memory_mb_seconds_delta, active_nodes, sequence_number, last_ack_sequence, command_results
- `HeartbeatResponse`: status, quotas, ack_sequence, server_sequence, pending_commands
- `RegisterTenant`, `RegisterCluster`, `IssueTenantCredit`, `ListTenantQuotas`

PostgreSQL (PoC subset):
- Tables: `tenants`, `workload_clusters`, `base_plans`, `quota_configs`, `tb_account_ids`, `quota_snapshots`
- sqlc queries: CRUD for each table, `UpsertQuotaSnapshot` (INSERT ON CONFLICT UPDATE)

**Verify**: `buf lint`, `buf generate`, `sqlc generate` all pass

### Phase 2: TigerBeetle Integration
**Files**: `internal/ledger/*`

- `ids.go`: `DeriveTransferID(clusterID, seqNum, ledgerID) → Uint128` via blake3 (non-crypto fast hash — IDs carry no secret)
- `accounts.go`: Create global operator/sink per ledger + tenant_quota accounts with `DebitsMustNotExceedCredits | History` flags
- `transfers.go`: Build usage transfers (code=200), allocation transfers (code=100), adjustment transfers (code=101). Submit batch, handle `exceeds_credits` result per-transfer
- `client.go`: TigerBeetle client wrapper

**Verify**: Integration tests against real TB — create accounts, allocate quota, record usage, verify balance, exhaust quota and confirm rejection, verify idempotent duplicate handling

### Phase 3: Temporal Workflows & Activities
**Files**: `internal/accounting/*`

> **Package name**: `internal/accounting` (not `internal/temporal`) to avoid confusion with the `go.temporal.io/sdk` import path.

> **Flat package**: Everything lives directly in `internal/accounting/` — no sub-packages, no import cycles. Signal types, activities, workflows, and worker setup are all `package accounting`. This is the idiomatic Temporal Go pattern (mirrors how temporal/samples-go structures single-domain apps).

> **Activity typing pattern**: Register by struct (`w.RegisterActivity(acts)`), call via nil pointer method values (`workflow.ExecuteActivity(ctx, tbActivities.SubmitTBBatch, input)`). The SDK resolves method names via reflection — no strings needed. See `workflows/activity_refs.go`.

> **pgx + sqlc bridge**: `sqlcgen.DBTX` uses `database/sql` interfaces. `pgxpool.Pool` uses pgx-native interfaces. Bridge: `db := stdlib.OpenDBFromPool(pool)` from `github.com/jackc/pgx/v5/stdlib`.

**TenantAccountingWorkflow** (the core):
- State: `curBatch []HeartbeatSignal`, `processedSeqs map[string]uint64`, `flushInterval = 30s`
- Signal handlers: `heartbeat` (dedup by seq, append to batch), `quota_adjustment` (append + immediate flush), `register_cluster` (init processedSeqs)
- Flush: build transfers from batch → `SubmitTBBatch` activity → process per-transfer results → `UpdateQuotaSnapshots` activity → update processedSeqs → clear batch
- Query handler: `last_tb_ack` for gateway to read ack_sequence

**TenantProvisioningWorkflow**:
1. InsertTenant (PG) → 2. CreateTenantTBAccounts (TB) → 3. InsertTBAccountMapping (PG) → 4. SubmitAllocationTransfers (TB) → 5. SignalWithStart TenantAccountingWorkflow

**Activities**: `SubmitTBBatch`, `CreateTenantTBAccounts`, `SubmitAllocationTransfers`, `UpdateQuotaSnapshots` (idempotent overwrite: read TB balance → write PG), `InsertTenant`, `InsertTBAccountMapping`, `InsertCluster`

**Verify**: Temporal test suite unit tests + integration tests with real services

### Phase 4: ConnectRPC Gateway (with Bidi Streaming)
**Files**: `internal/gateway/*`, `cmd/server/main.go`

- `dedup.go`: in-memory map with TTL pruning
- `stream_manager.go`: manages per-cluster stream lifecycle
  - Connection map: `cluster_id → stream` for active bidi connections
  - On connect: register stream, cancel missed-heartbeat timer if reconnecting
  - On disconnect: deregister, log (skip missed-HB timer for PoC)
- `heartbeat_handler.go`: implements both `HeartbeatStream` (bidi) and `Heartbeat` (unary fallback)
  - **Bidi stream (`HeartbeatStream`)**: per-stream goroutine pair:
    - **recv loop**: reads client heartbeats → validate → dedup → signal Temporal → update last_seen
    - **send loop**: watches for Temporal workflow query results (ack_sequence, quota status) → writes HeartbeatResponse to stream. Pushes enforcement commands immediately when available.
  - **Unary fallback (`Heartbeat`)**: same logic but request-response, commands piggyback on response
- `provisioning_handler.go`: start TenantProvisioningWorkflow, insert cluster row + signal workflow
- `admin_handler.go`: signal QuotaAdjustment to workflow, read quota_snapshots
- `cmd/server/main.go`: init PG pool + TB client + Temporal client, create global TB accounts (idempotent), start Temporal worker, start HTTP server with ConnectRPC handlers (h2c for gRPC bidi without TLS in dev), graceful shutdown

**Verify**: End-to-end: provision tenant via RPC → open bidi stream → send heartbeats → receive async ACKs + quota status → verify PG snapshots

### Phase 5: CLI Simulator & Live Dashboard Demo
**Files**: `cmd/simulator/main.go`, `cmd/simulator/ui.go`, `cmd/simulator/scenario.go`, `scripts/demo.sh`

This is the stakeholder-facing artifact. The simulator is a **terminal UI application** (using `charmbracelet/bubbletea` + `charmbracelet/lipgloss`) that runs a scripted demo scenario with a live, real-time dashboard.

#### Terminal UI Layout

```
┌─────────────────────────────────────────────────────────────────────────┐
│  BYOC Cloud Credit System — Live Demo                          [00:42] │
├───────────────────────────────────┬─────────────────────────────────────┤
│  TENANT: Acme Corp (pro)         │  SCENARIO TIMELINE                  │
│  Clusters: 2 active              │                                     │
│                                  │  ✓ 1. Provisioned tenant wallets    │
│  ┌─ CPU Hours ──────────────┐    │  ✓ 2. Clusters streaming heartbeats │
│  │ ████████████░░░░░  62%   │    │  ▶ 3. Recording usage...            │
│  │ used: 620K / limit: 1M  │    │    4. Drive to exhaustion            │
│  └──────────────────────────┘    │    5. Surge pack top-up              │
│  ┌─ Memory GB-Hours ────────┐    │    6. Idempotency proof              │
│  │ ██████░░░░░░░░░░░  35%   │    │    7. Final summary                 │
│  │ used: 350K / limit: 1M  │    │                                     │
│  └──────────────────────────┘    ├─────────────────────────────────────┤
│  ┌─ Active Nodes ───────────┐    │  EVENT LOG                          │
│  │ ████░░░░░░░░░░░░░  20%   │    │  14:32:05 → hb seq=47 ACK'd        │
│  │ used: 4 / limit: 20     │    │  14:32:05 → CPU: -23K credits       │
│  └──────────────────────────┘    │  14:32:05 → MEM: -8K credits        │
│                                  │  14:32:01 → hb seq=46 ACK'd        │
├───────────────────────────────┬──┤  14:31:55 → hb seq=45 ACK'd        │
│  CLUSTER A (fast)    ● live  │  │  ...                                │
│  stream: connected           │  ├─────────────────────────────────────┤
│  seq: 47  ack: 45            │  │  TB STATS                           │
│  rate: 1 hb/10s              │  │  Transfers submitted: 282           │
│  last: 14:32:05              │  │  Batch latency p50: 1.2ms           │
├──────────────────────────────┤  │  Batch latency p99: 4.8ms           │
│  CLUSTER B (slow)    ● live  │  │  Rejected (exceeds_credits): 0      │
│  stream: connected           │  │  Idempotent (exists): 0             │
│  seq: 12  ack: 10            │  │  TB capacity used: <0.03%           │
│  rate: 1 hb/10s              │  │                                     │
│  last: 14:32:02              │  │                                     │
└──────────────────────────────┴──┴─────────────────────────────────────┘
  [SPACE] pause   [N] next step   [R] restart   [Q] quit
```

#### Simulator Architecture

> **Real tenant behavior**: The simulator communicates with the control plane exactly as a real tenant agent would. All ConnectRPC calls are live; quota bars update from real `HeartbeatResponse` data; the idempotency proof sends duplicate sequence numbers on the real bidi stream.

`cmd/simulator/main.go`:
- `ClusterSim` struct per cluster: `exhaustCh chan struct{}` (closed to trigger step 4 ramp-up), `replayCh chan uint64` (inject old seqs for step 6)
- Recv goroutine reads real `HeartbeatResponse`, sends `quotaUpdateMsg`/`eventMsg` bubbletea messages
- `stepExhaust`: closes `simA.exhaustCh` → cluster A sends 5-10x CPU delta until hard limit hit
- `stepIdempotency`: sends 3 recent seqs to `simA.replayCh` → actually replayed on live stream
- Bubbletea app setup, connect to server, initialize UI model

`cmd/simulator/ui.go`:
- `Model` implementing bubbletea `Model` interface
- Real-time updates: quota bars, event log, cluster status, TB stats
- Color-coded status: green (OK), yellow (QUOTA_WARNING), red (QUOTA_EXCEEDED)
- Quota bars animate as usage accumulates
- Event log scrolls with timestamped entries showing heartbeat ACKs, transfers, rejections
- TB stats section shows batch latency percentiles and capacity utilization

`cmd/simulator/scenario.go`:
- Defines the 7-step scripted scenario as a state machine
- Each step has: description, action function, success condition, transition trigger
- Can auto-advance or wait for user keypress (`[N]` next step)

#### Scenario Steps (Detailed)

**Step 1 — Provision tenant wallets** (auto, ~2s)
- Call `RegisterTenant("Acme Corp", tier=pro)`
- UI shows: tenant created, TB accounts initialized, wallets loaded with credits
- Event log: "Tenant Acme Corp provisioned — 3 wallets loaded (CPU: 1M, MEM: 1M, Nodes: 20)"
- Quota bars appear at 0% used

**Step 2 — Start clusters streaming** (auto, ~3s)
- Call `RegisterCluster` x2 (Cluster A "us-east-1", Cluster B "eu-west-1")
- Open bidi stream per cluster
- Cluster panels appear with "● live" indicator and stream status
- Event log: "Cluster A connected — bidi stream established"

**Step 3 — Record usage** (runs ~30s, auto-advance)
- Cluster A: sends heartbeats with high CPU/memory deltas (aggressive consumer)
- Cluster B: sends heartbeats with low deltas (steady consumer)
- UI updates in real-time: quota bars fill, event log shows each ACK + credit deduction
- TB stats update: transfer count climbing, batch latency visible
- Quota bars visually diverge — CPU filling fast, others slower

**Step 4 — Drive to exhaustion** (runs until rejection, ~20s)
- Cluster A ramps up CPU usage dramatically
- Quota bar for CPU turns yellow at soft limit (80%), event log: "⚠ SOFT LIMIT — CPU at 80%, entering burst zone"
- CPU bar turns red when hard limit hit
- Event log: "✗ HARD LIMIT — TigerBeetle rejected CPU transfer (exceeds_credits)"
- HeartbeatResponse shows STATUS_QUOTA_EXCEEDED on stream
- Cluster A panel shows "⚠ quota exceeded"
- **Key moment**: other resources (MEM, Nodes) continue working — independent transfers, not linked

**Step 5 — Surge pack top-up** (auto, ~3s)
- Call `IssueTenantCredit(cpu_hours, 500_000, "Emergency surge pack")`
- Event log: "↑ Surge pack applied — CPU wallet +500K credits"
- CPU quota bar drops from 100% back down, turns green
- Next heartbeat from Cluster A succeeds
- Event log: "✓ CPU transfer accepted — tenant unblocked"

**Step 6 — Idempotency proof** (auto, ~5s)
- Replay 3 heartbeats with previously-used sequence numbers on the stream
- Event log shows: "↩ Duplicate seq=23 — deduped at workflow (processedSeqs)"
- TB stats: "Idempotent (exists)" counter stays at 0 if caught at workflow level, or increments if caught at TB level
- Quota bars unchanged — no double-charging
- Visual emphasis: balance before = balance after

**Step 7 — Final summary** (persists until quit)
- Display a summary card:
  ```
  ┌─ DEMO SUMMARY ──────────────────────────────────┐
  │ ✓ Heartbeats processed: 147                      │
  │ ✓ TB transfers submitted: 423                    │
  │ ✓ Hard limit rejections: 12 (CPU only)           │
  │ ✓ Duplicate heartbeats caught: 3                 │
  │ ✓ Surge pack applied and tenant unblocked        │
  │ ✓ Independent resource accounting verified       │
  │ ✓ TB batch latency p99: 4.8ms                    │
  │ ✓ TB capacity utilization: <0.03%                │
  │                                                   │
  │ All success criteria met.                         │
  └───────────────────────────────────────────────────┘
  ```

#### Bidi Stream Integration in Simulator

Each simulated cluster runs two goroutines feeding bubbletea messages:
- **Send goroutine**: ticks on 10s interval (or accelerated for demo), builds HeartbeatRequest with metrics + sequence_number + last_ack_sequence, sends on stream
- **Recv goroutine**: blocks on stream.Recv(), parses HeartbeatResponse, extracts ack_sequence + quota status + commands, sends bubbletea message to update UI
- On stream error: visual indicator flips to "● reconnecting", exponential backoff, auto-reconnect

#### Key Dependencies for Simulator
- `github.com/charmbracelet/bubbletea` — terminal UI framework
- `github.com/charmbracelet/lipgloss` — styling/layout
- `github.com/charmbracelet/bubbles` — progress bars, spinners, viewport (event log)
- ConnectRPC generated gRPC client for HeartbeatStream + admin RPCs

**Verify**: Full demo runs end-to-end with live TUI. All 7 steps execute. Visual output is clear and stakeholder-ready.

---

## Verification Plan

| Success Criterion (from one-pager) | How to Verify |
|-------------------------------------|---------------|
| Exactly-once usage despite retries/duplicates | Send same seq twice, verify TB balance unchanged |
| Hard quota rejection by TigerBeetle | Exhaust wallet, verify `exceeds_credits`, no app bypass |
| Accurate PG quota snapshots | Compare PG snapshots to TB account balances |
| Different heartbeat rates handled | Fast + slow cluster sim, both accounted correctly |
| Throughput headroom | Log TB batch latency; show <1% of TB capacity used |

---

## Risks

| Risk | Mitigation |
|------|-----------|
| TigerBeetle Go client CGo/Zig build issues | Use Docker for TB server; test client build early in Phase 0 |
| Temporal workflow determinism pitfalls | Strict separation: all I/O in activities, decisions in workflow |
| sqlc BYTEA handling for TB account IDs | Use `[]byte` in Go; verify in Phase 1 |
