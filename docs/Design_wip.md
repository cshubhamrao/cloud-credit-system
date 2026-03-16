# BYOC Cloud Credit System

**Architecture Design Document**

_Version 0.1 — Draft_

---

## Executive Summary

This document describes a resource accounting and quota enforcement system for a Bring-Your-Own-Cloud (BYOC) deployment model. The system uses a "mothership" control plane that manages tenant "shoot" clusters, tracking resource consumption via a double-entry ledger (TigerBeetle) and enforcing both hard and soft limits on sliceable resources.

---

## Core Concepts

### The Gardener Model

Borrowing terminology from Gardener (Kubernetes cluster management), the architecture distinguishes between:

- **Mothership (Control Plane):** The central cluster that manages configuration, authentication, quota enforcement, and billing. Tenants never directly access this.
- **Shoot Clusters:** Tenant workload clusters running in their own cloud accounts. These "phone home" to the mothership at regular intervals.
- **Credits:** The unit of account for resource consumption. Each resource type (CPU, memory, storage, etc.) is denominated in credits based on pricing tiers.

### Resource Types

The system tracks two categories of resources:

| Category          | Examples                                              | Measurement                                               |
| ----------------- | ----------------------------------------------------- | --------------------------------------------------------- |
| **Cumulative**    | CPU-hours, memory-GB-hours, network egress, API calls | Delta reported each heartbeat, summed over billing period |
| **Point-in-time** | Active users, active nodes, storage used, GPU slices  | Current count/gauge reported each heartbeat               |

---

## System Architecture

### Component Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                     MOTHERSHIP (Control Plane)                  │
│                                                                 │
│  ┌──────────────┐    ┌─────────┐    ┌─────────────────────────┐ │
│  │ ConnectRPC   │───▶│  Kafka  │───▶│  Workers (N instances)  │ │
│  │ Gateway      │    │ (Proto) │    │  ├─ PostgreSQL writes   │ │
│  └──────────────┘    └─────────┘    │  └─ TigerBeetle batches │ │
│         │                           └─────────────────────────┘ │
│         │                                                       │
│         │              ┌────────────┐     ┌──────────────┐      │
│         │              │ PostgreSQL │     │ TigerBeetle  │      │
│         │              │ (metadata) │     │ (ledger)     │      │
│         │              └────────────┘     └──────────────┘      │
│         │                                                       │
│  ┌──────────────┐                                               │
│  │  Dashboard   │  (reads from Postgres, displays quotas)       │
│  └──────────────┘                                               │
└─────────────────────────────────────────────────────────────────┘
         │
         │ Heartbeat (1min interval)
         ▼
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│  Shoot Cluster  │  │  Shoot Cluster  │  │  Shoot Cluster  │ ...
│  (Tenant A)     │  │  (Tenant B)     │  │  (Tenant C)     │
└─────────────────┘  └─────────────────┘  └─────────────────┘
```

### Component Responsibilities

#### ConnectRPC Gateway

- Exposes typed RPC endpoints for shoot clusters (heartbeat, config pull, key rotation)
- Validates tenant authentication (mTLS or JWT)
- Produces Protobuf-encoded messages to Kafka topics
- Synchronously checks quota status before returning config (fast path via cache)

#### Kafka Message Bus

- Decouples ingestion from processing
- Topics: `heartbeats`, `quota-events`, `audit-log`, `commands`
- Enables replay for debugging and reprocessing

#### Workers (N instances)

- Consume from Kafka, partition by `tenant_id` for ordering
- Write relational data to PostgreSQL (tenant metadata, audit logs, quota configs)
- Batch transfers to TigerBeetle (usage debits, quota credits)
- Emit commands back to Kafka when limits are breached

#### TigerBeetle (Financial Ledger)

- Double-entry accounting for all resource consumption
- Account per tenant per resource type (e.g., `tenant_123_cpu_quota`, `tenant_123_cpu_usage`)
- Native overdraft protection enforces hard limits atomically
- Immutable audit trail of all transfers

#### PostgreSQL (Metadata Store)

- Tenant profiles, contact info, billing tier
- Quota configuration (limits, soft/hard thresholds, alerting rules)
- Shoot cluster registry (IDs, last heartbeat, status)
- Historical snapshots for dashboard reporting

---

## TigerBeetle Modeling

This section describes how to model the cloud credit system using TigerBeetle's primitives, aligned with [TigerBeetle's data modeling guidance](https://docs.tigerbeetle.com/coding/data-modeling/).

### Core Concepts Mapping

| Cloud Credit Concept                       | TigerBeetle Primitive                                   |
| ------------------------------------------ | ------------------------------------------------------- |
| Resource type (CPU, memory, storage, etc.) | **Ledger** — one ledger per resource type               |
| Tenant quota allocation                    | **Account** with `flags.debits_must_not_exceed_credits` |
| Usage accumulation                         | **Transfer** debiting tenant account                    |
| Hard limit enforcement                     | Native overdraft protection via account flags           |
| Soft limit warning                         | Application logic checking balance thresholds           |
| Billing period reset                       | Transfer to sink account + fresh quota credit           |

### Ledger Design

Each sliceable resource gets its own ledger. This ensures accounts for different resource types cannot accidentally transact with each other.

```
Ledger 1: cpu_hours
Ledger 2: memory_gb_hours
Ledger 3: storage_gb
Ledger 4: network_egress_gb
Ledger 5: active_nodes        (point-in-time gauge)
Ledger 6: active_users        (point-in-time gauge)
Ledger 7: gpu_hours
```

**Why separate ledgers?** Ledgers partition accounts that can transact together. A CPU-hours account should never transfer to a storage account — they're different units. Separate ledgers enforce this at the database level.

### Account Structure

Each tenant gets accounts on each resource ledger. We use TigerBeetle's `user_data` fields to link accounts to external entities.

#### Account Fields Usage

| Field           | Usage                                                                            |
| --------------- | -------------------------------------------------------------------------------- |
| `id`            | 128-bit unique ID (see ID encoding below)                                        |
| `ledger`        | Resource type identifier                                                         |
| `code`          | Account type: `1` = operator, `2` = tenant_quota, `3` = tenant_usage, `4` = sink |
| `user_data_128` | Tenant UUID (links account to PostgreSQL tenant record)                          |
| `user_data_64`  | Billing period start timestamp (nanoseconds since epoch)                         |
| `user_data_32`  | Billing tier enum (`1` = free, `2` = starter, `3` = pro, `4` = enterprise)       |
| `flags`         | See below                                                                        |

#### Account Types Per Tenant Per Resource

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Ledger: cpu_hours                                                       │
│                                                                          │
│  ┌──────────────┐      ┌──────────────────┐      ┌──────────────┐       │
│  │   Operator   │      │  Tenant Quota    │      │     Sink     │       │
│  │   (source)   │─────▶│  (credit limit)  │─────▶│  (archive)   │       │
│  │              │      │                  │      │              │       │
│  │  code: 1     │      │  code: 2         │      │  code: 4     │       │
│  │  flags: 0    │      │  flags: debits_  │      │  flags: 0    │       │
│  │              │      │  must_not_exceed │      │              │       │
│  │              │      │  _credits        │      │              │       │
│  └──────────────┘      └──────────────────┘      └──────────────┘       │
│                                 │                                        │
│                                 │ usage transfer                         │
│                                 ▼                                        │
│                        (debits increase as                               │
│                         resources consumed)                              │
└─────────────────────────────────────────────────────────────────────────┘
```

#### Account Flags

| Account Type | Flags                                                           |
| ------------ | --------------------------------------------------------------- |
| Operator     | `0` (no constraints — unlimited source)                         |
| Tenant Quota | `flags.debits_must_not_exceed_credits` (hard limit enforcement) |
| Sink         | `0` (receives archived usage at period end)                     |

**Hard limit enforcement:** With `debits_must_not_exceed_credits` set, any transfer that would make `debits_pending + debits_posted > credits_posted` is rejected atomically. This is the hard limit.

### ID Encoding Scheme

TigerBeetle IDs are 128-bit. We encode tenant and resource info directly:

```
┌─────────────────────────────────────────────────────────────────┐
│                        128-bit Account ID                        │
├──────────────────┬──────────────┬──────────────┬────────────────┤
│   Tenant UUID    │    Ledger    │ Account Type │    Reserved    │
│   (64 bits)      │   (16 bits)  │   (8 bits)   │   (40 bits)    │
└──────────────────┴──────────────┴──────────────┴────────────────┘

Encoding (Go example):
  id = (tenant_uuid_high64 << 64) | (ledger << 48) | (account_type << 40) | random_bits
```

This scheme allows:

- Deriving account IDs deterministically from (tenant, resource, type) tuple
- Efficient lookup without querying PostgreSQL
- Collision avoidance via random suffix

For **transfers**, use TigerBeetle's recommended time-based IDs:

```
id = (timestamp_ms << 80) | random_80bits
```

### Transfer Patterns

#### 1. Quota Allocation (Billing Period Start)

Credit the tenant's quota account from the operator.

```
Transfer {
  id:                <time-based-id>,
  debit_account_id:  <operator_account>,
  credit_account_id: <tenant_quota_account>,
  amount:            1000000,              // e.g., 1M CPU-milliseconds
  ledger:            1,                    // cpu_hours
  code:              100,                  // QUOTA_ALLOCATION
  user_data_128:     <tenant_uuid>,
  user_data_64:      <billing_period_start_ns>,
}
```

#### 2. Usage Recording (Heartbeat)

Debit the tenant's quota account. Fails atomically if over limit.

```
Transfer {
  id:                <time-based-id>,
  debit_account_id:  <tenant_quota_account>,
  credit_account_id: <sink_account>,
  amount:            47000,                // 47 CPU-seconds since last heartbeat
  ledger:            1,                    // cpu_hours
  code:              200,                  // USAGE_RECORD
  user_data_128:     <cluster_id>,         // which shoot cluster reported this
  user_data_64:      <heartbeat_timestamp_ns>,
}
```

**If this transfer fails with `exceeds_credits`**, the tenant has hit their hard limit.

#### 3. Soft Limit Check (Application Logic)

Soft limits are checked _before_ submitting the transfer:

```go
// Pseudocode
account := tb.LookupAccount(tenantQuotaAccountID)
balance := account.CreditsPosted - account.DebitsPosted - account.DebitsPending

softLimitThreshold := quotaConfig.SoftLimit  // e.g., 80% of hard limit

if balance < softLimitThreshold {
    // Emit warning, but still allow transfer
    emitSoftLimitWarning(tenant, resource, balance)
}

// Proceed with transfer (may still fail at hard limit)
```

#### 4. Billing Period Reset

At the end of each billing period, archive usage and refresh quota.

```
// Step 1: Zero out remaining balance (transfer to sink)
// Use balancing_debit to transfer whatever remains
Transfer {
  id:                <time-based-id>,
  debit_account_id:  <tenant_quota_account>,
  credit_account_id: <sink_account>,
  amount:            AMOUNT_MAX,           // transfer whatever is available
  ledger:            1,
  code:              300,                  // PERIOD_CLOSE
  flags:             flags.balancing_debit,
}

// Step 2: Credit fresh quota for new period
Transfer {
  id:                <time-based-id>,
  debit_account_id:  <operator_account>,
  credit_account_id: <tenant_quota_account>,
  amount:            <new_period_quota>,
  ledger:            1,
  code:              100,                  // QUOTA_ALLOCATION
  user_data_64:      <new_billing_period_start_ns>,
}
```

### Handling Point-in-Time Gauges (Active Nodes/Users)

Cumulative resources (CPU-hours) are straightforward — just keep debiting. **Point-in-time gauges** (active nodes, active users) are trickier because they go up _and_ down.

**Approach: Pending Transfers with Timeout (Rate Limiting Pattern)**

Inspired by TigerBeetle's [rate limiting recipe](https://docs.tigerbeetle.com/coding/recipes/rate-limiting/):

1. Credit tenant with their node limit (e.g., 10 nodes)
2. When a node activates, create a **pending transfer** with a timeout
3. The pending amount is "reserved" — it reduces available balance
4. When node deactivates, **void** the pending transfer (or let it timeout)

```
// Node activation
Transfer {
  id:                <node-activation-transfer-id>,
  debit_account_id:  <tenant_nodes_account>,
  credit_account_id: <operator_account>,
  amount:            1,
  ledger:            5,                    // active_nodes
  code:              400,                  // NODE_ACTIVATE
  timeout:           3600,                 // 1 hour — must heartbeat to refresh
  flags:             flags.pending,
  user_data_128:     <node_id>,
}

// Node deactivation (or scale-down)
Transfer {
  id:                <new-id>,
  pending_id:        <node-activation-transfer-id>,
  flags:             flags.void_pending_transfer,
}
```

**Heartbeat refresh:** Nodes must refresh their pending transfer periodically. If a node disappears (missed heartbeats), the pending transfer times out and the slot is automatically reclaimed.

### Linked Transfers for Atomic Operations

Use `flags.linked` to make multiple transfers atomic. If any fails, all fail.

**Example: Multi-resource usage recording**

```
// All succeed or all fail
Transfer { ..., ledger: 1, code: 200, flags: flags.linked }  // CPU
Transfer { ..., ledger: 2, code: 200, flags: flags.linked }  // Memory
Transfer { ..., ledger: 3, code: 200, flags: 0 }             // Storage (last in chain, no linked flag)
```

### Code Values (Transfer Types)

| Code | Meaning                                                            |
| ---- | ------------------------------------------------------------------ |
| 100  | `QUOTA_ALLOCATION` — crediting quota at period start               |
| 101  | `QUOTA_ADJUSTMENT` — manual adjustment by operator                 |
| 200  | `USAGE_RECORD` — heartbeat usage debit                             |
| 201  | `USAGE_CORRECTION` — correcting a prior usage record               |
| 300  | `PERIOD_CLOSE` — zeroing balance at period end                     |
| 400  | `NODE_ACTIVATE` — pending transfer for active node slot            |
| 401  | `USER_ACTIVATE` — pending transfer for active user slot            |
| 500  | `OVERAGE_CHARGE` — recording usage beyond soft limit (for billing) |

### Querying Balances

```go
// Get current available quota
account := tb.LookupAccount(tenantQuotaAccountID)

availableBalance := account.CreditsPosted - account.DebitsPosted - account.DebitsPending
utilizationPercent := float64(account.DebitsPosted) / float64(account.CreditsPosted) * 100
```

### Historical Balance Tracking

For accounts where you need balance history (e.g., for dashboard charts), set `flags.history` on account creation. Then use `get_account_balances` to retrieve historical snapshots.

---

## Data Model

### PostgreSQL Schema (Core Tables)

```sql
-- tenants
CREATE TABLE tenants (
  id              UUID PRIMARY KEY,
  name            TEXT NOT NULL,
  billing_tier    TEXT CHECK (billing_tier IN ('free', 'starter', 'pro', 'enterprise')),
  created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- shoot_clusters
CREATE TABLE shoot_clusters (
  id              UUID PRIMARY KEY,
  tenant_id       UUID REFERENCES tenants(id),
  cloud_provider  TEXT CHECK (cloud_provider IN ('aws', 'gcp', 'azure')),
  region          TEXT,
  last_heartbeat  TIMESTAMPTZ,
  status          TEXT CHECK (status IN ('healthy', 'degraded', 'unreachable', 'killed'))
);

-- quota_configs
CREATE TABLE quota_configs (
  tenant_id       UUID REFERENCES tenants(id),
  resource_type   TEXT NOT NULL,  -- cpu_hours, memory_gb_hours, storage_gb, active_nodes, active_users
  hard_limit      BIGINT NOT NULL,
  soft_limit      BIGINT,
  billing_period  TEXT CHECK (billing_period IN ('monthly', 'daily')),
  PRIMARY KEY (tenant_id, resource_type)
);
```

---

## Heartbeat Protocol

### Request Payload

Each shoot cluster sends a heartbeat every 60 seconds containing:

```protobuf
message Heartbeat {
  string cluster_id = 1;
  google.protobuf.Timestamp timestamp = 2;

  // Point-in-time gauges
  int32 active_nodes = 3;
  int32 active_users = 4;
  int64 storage_bytes = 5;

  // Cumulative deltas (since last heartbeat)
  int64 cpu_milliseconds_delta = 6;
  int64 memory_mb_seconds_delta = 7;
  int64 network_egress_bytes_delta = 8;

  // Exotic resources
  repeated GpuSlice gpu_slices = 9;
}

message GpuSlice {
  string gpu_type = 1;      // e.g., "nvidia-a100-40gb"
  int32 slice_count = 2;    // MIG slices or full GPUs
  int64 gpu_seconds_delta = 3;
}
```

### Response Payload

```protobuf
message HeartbeatResponse {
  Status status = 1;  // OK | QUOTA_WARNING | QUOTA_EXCEEDED | KILL

  // Config refresh (optional, only if changed)
  optional ClusterConfig config = 2;

  // Quota status for dashboard/alerting
  repeated QuotaStatus quotas = 3;

  // Commands to execute
  repeated Command commands = 4;
}

enum Status {
  OK = 0;
  QUOTA_WARNING = 1;
  QUOTA_EXCEEDED = 2;
  KILL = 3;
}

message QuotaStatus {
  string resource_type = 1;
  int64 used = 2;
  int64 soft_limit = 3;
  int64 hard_limit = 4;
  float utilization_percent = 5;
}

message Command {
  CommandType type = 1;
  string reason = 2;
  google.protobuf.Struct parameters = 3;
}

enum CommandType {
  SCALE_DOWN = 0;
  DRAIN_NODE = 1;
  STOP_WORKLOADS = 2;
  ROTATE_KEYS = 3;
  SHUTDOWN = 4;
}
```

---

## Limit Enforcement

### Hard Limits (TigerBeetle Native)

Hard limits are enforced **atomically by TigerBeetle** using the `flags.debits_must_not_exceed_credits` flag on tenant quota accounts.

**Mechanism:**

- When a usage transfer is submitted, TigerBeetle checks: `debits_pending + debits_posted + transfer.amount ≤ credits_posted`
- If violated, the transfer fails with `exceeds_credits` — no partial execution
- The heartbeat response then includes `QUOTA_EXCEEDED` status

**What happens on breach:**

1. Worker catches `exceeds_credits` error from TigerBeetle
2. Worker emits `quota-exceeded` event to Kafka
3. Next heartbeat response includes `QUOTA_EXCEEDED` + `SCALE_DOWN` command
4. For gauge resources (nodes): new node activations are refused (pending transfer would exceed limit)

### Soft Limits (Application Logic)

Soft limits are checked **in application code** before submitting transfers.

```go
// Before recording usage
account := tb.LookupAccount(tenantQuotaAccountID)
available := account.CreditsPosted - account.DebitsPosted - account.DebitsPending

softThreshold := quotaConfig.HardLimit * 0.8  // 80% of hard limit

if available < (quotaConfig.HardLimit - softThreshold) {
    // Tenant has used > 80% of quota
    emitSoftLimitAlert(tenant, resource, available)
    // Continue with transfer — soft limit doesn't block
}
```

**Triggers:**

- Email/Slack alerts to tenant admin
- Dashboard warning banner
- `QUOTA_WARNING` status in heartbeat response
- Optional: Kafka event for external integrations

### Missed Heartbeat Handling

| Missed Heartbeats | Duration    | Action                                                    |
| ----------------- | ----------- | --------------------------------------------------------- |
| 3                 | ~3 minutes  | Mark cluster as `degraded`                                |
| 10                | ~10 minutes | Mark as `unreachable`, queue `KILL` command               |
| Next successful   | —           | Deliver `KILL` command, shoot initiates graceful shutdown |

The shoot cluster implements graceful shutdown on `KILL`:

1. Drain running workloads
2. Notify connected users
3. Persist any local state
4. Report final metrics
5. Terminate

---

## Billing Period Lifecycle

### Monthly Reset Flow

```
┌─────────────────────────────────────────────────────────────┐
│                    BILLING PERIOD RESET                      │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  1. Snapshot current usage → billing_history table           │
│  2. Generate invoice record                                  │
│  3. Reset usage accounts in TigerBeetle (transfer to sink)   │
│  4. Refresh quota accounts based on billing tier             │
│  5. Emit "period_reset" event to Kafka                       │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### Quota Refresh

At the start of each billing period:

```
For each tenant:
  For each resource_type in quota_configs:
    1. Create transfer: quota_refresh_source → tenant:resource:quota
    2. Amount = hard_limit from quota_configs
    3. Zero out usage account (or archive to history)
```

---

## Dashboard Requirements

### Tenant View

- Current usage vs limits (bar charts per resource)
- Usage trend over billing period (line chart)
- Projected overage alerts
- Cluster health status
- Recent commands issued

### Operator View

- Fleet-wide utilization heatmap
- Tenants approaching limits
- Unreachable/degraded clusters
- Audit log viewer
- Manual quota adjustment interface

---

## Open Questions & Decisions Needed

1. **Billing granularity:** Per-minute vs per-hour vs per-day for cumulative resources? Finer granularity = more TigerBeetle transfers.

2. **Offline tolerance:** How long can a shoot cluster operate without mothership contact? Should it cache last-known quotas locally?

3. **Credit pricing:** Uniform credits across resource types, or different credit rates (1 CPU-hour = 10 credits, 1 GB storage = 2 credits)?

4. **Rollover:** Do unused quota credits roll over to the next billing period?

5. **Burst allowance:** Should there be a temporary burst buffer above hard limits?

6. **Multi-cluster tenants:** Shared quota pool across all tenant clusters, or per-cluster quotas?

7. **Retroactive enforcement:** If a shoot reports usage that exceeds limits (was offline), do we bill overage or reject the data?

---

## Security Considerations

### Authentication

- Shoot → Mothership: mTLS with per-cluster certificates
- Certificate rotation via `ROTATE_KEYS` command
- Short-lived JWTs for dashboard access

### Authorization

- Tenant isolation enforced at Kafka partition level
- TigerBeetle account IDs encode tenant ID (cannot transfer across tenants)
- PostgreSQL RLS policies for multi-tenant queries

### Audit

- All TigerBeetle transfers are immutable
- Kafka provides replay capability
- PostgreSQL audit log with `who/what/when`

---

## Next Steps

1. [ ] Define Protobuf schemas for ConnectRPC service
2. [ ] Design TigerBeetle account ID encoding scheme (128-bit UUID mapping)
3. [ ] Prototype heartbeat flow with mock shoot cluster
4. [ ] Define Kafka topic schemas and partitioning strategy
5. [ ] Build dashboard wireframes
6. [ ] Load test TigerBeetle with expected transfer volume
7. [ ] Define SLOs for heartbeat latency and quota check latency

---

## Appendix: Technology Choices

| Component     | Choice      | Rationale                                                                 |
| ------------- | ----------- | ------------------------------------------------------------------------- |
| RPC Framework | ConnectRPC  | gRPC-compatible, browser support, better tooling                          |
| Message Bus   | Kafka       | Durability, replay, partitioning by tenant                                |
| Ledger        | TigerBeetle | Purpose-built for financial accounting, ACID, native overdraft protection |
| Metadata DB   | PostgreSQL  | Relational queries, mature, RLS for multi-tenancy                         |
| Serialization | Protobuf    | Schema evolution, compact, typed                                          |

---

## Appendix: TigerBeetle Operational Notes

### Reliable Transaction Submission

TigerBeetle transfers are **idempotent by ID**. Use this for crash recovery:

```go
// Worker crash recovery pattern
func recordUsage(tenantID, clusterID uuid.UUID, amount uint64) error {
    // Generate deterministic transfer ID from inputs
    transferID := deriveTransferID(tenantID, clusterID, heartbeatTimestamp)

    // Safe to retry — if already exists, TigerBeetle returns `exists`
    result := tb.CreateTransfer(Transfer{
        ID:     transferID,
        Amount: amount,
        // ...
    })

    if result == exists || result == ok {
        return nil  // Success (already recorded or just recorded)
    }
    return fmt.Errorf("transfer failed: %v", result)
}
```

See [Reliable Transaction Submission](https://docs.tigerbeetle.com/coding/reliable-transaction-submission/) for details.

### Batching

TigerBeetle is optimized for batches. The worker should:

1. Consume N heartbeats from Kafka
2. Build a batch of transfers (up to 8190 per request)
3. Submit batch atomically
4. Commit Kafka offsets only after TigerBeetle confirms

```go
// Batch usage recording
transfers := make([]Transfer, 0, 8190)
for _, heartbeat := range heartbeats {
    transfers = append(transfers, buildUsageTransfer(heartbeat))
}
results := tb.CreateTransfers(transfers)
// Handle results...
```

### Cluster Sizing

- TigerBeetle clusters are 1, 3, or 6 replicas
- For this use case: **3 replicas** (tolerates 1 failure)
- Each replica needs fast NVMe storage
- Expected throughput: ~1M transfers/second per cluster

### Backup & Recovery

- TigerBeetle provides consistent snapshots
- Integrate with your existing backup pipeline
- For disaster recovery: restore from snapshot + replay from Kafka

---

## Appendix: References

- [TigerBeetle Data Modeling](https://docs.tigerbeetle.com/coding/data-modeling/)
- [TigerBeetle Rate Limiting Recipe](https://docs.tigerbeetle.com/coding/recipes/rate-limiting/)
- [TigerBeetle Balance Bounds Recipe](https://docs.tigerbeetle.com/coding/recipes/balance-bounds/)
- [TigerBeetle Two-Phase Transfers](https://docs.tigerbeetle.com/coding/two-phase-transfers/)
- [TigerBeetle Account Reference](https://docs.tigerbeetle.com/reference/account/)
- [TigerBeetle Transfer Reference](https://docs.tigerbeetle.com/reference/transfer/)
