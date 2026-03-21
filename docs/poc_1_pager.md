# Artifact 1 — One Pager for PoC / Demo

# PoC / Demo Proposal

## Ledger-Backed Quota Enforcement for BYOC at High Throughput

### Why this PoC

In a BYOC model, the control plane must do two things extremely well:

1. turn tenant heartbeats into accurate, auditable usage records, and
2. enforce hard limits reliably, even when tenant behavior varies widely.

This PoC demonstrates an architecture built for exactly that. At its core, **TigerBeetle** acts as the quota ledger and hard-limit engine, while **Temporal** serializes tenant accounting and absorbs bursty control-plane behavior without forcing the system into a heavyweight streaming stack. The result is a design that can handle both **fast tenants** and **slow tenants** cleanly, while keeping enforcement and reporting correct.

### What we are proving

This PoC will prove that:

- usage heartbeats can be converted into durable ledger entries
- hard quota enforcement happens atomically in TigerBeetle, not through fragile app-side checks
- slow and fast tenants can coexist without one tenant’s behavior destabilizing another
- the reporting path remains simple and queryable via PostgreSQL projections
- the architecture retains headroom far beyond the initial target load

### Architecture thesis

The design deliberately splits responsibilities so each layer does one job well:

- **Gateway / agent protocol** receives heartbeats from workload clusters
- **Temporal `TenantAccountingWorkflow`** provides one sequential accounting lane per tenant, giving us durable state, ordered processing, retry safety, and natural tenant isolation
- **TigerBeetle** records usage as immutable transfers and enforces hard limits atomically using `debits_must_not_exceed_credits`
- **PostgreSQL** stores materialized quota snapshots and billing snapshots for dashboards and operators; it is the reporting layer, not the enforcement layer

This is what allows TigerBeetle to shine: the surrounding architecture does not fight it. It batches intelligently, preserves idempotency, and ensures a single TigerBeetle writer per tenant, so the ledger can focus on what it does best: **high-throughput, low-latency financial-style state transitions with strict correctness guarantees.**

### Why this matters for slow and fast tenants

Tenant behavior is never uniform. Some tenants generate predictable low-rate heartbeats; others may burst, reconnect, or run several active clusters. This architecture handles both classes well:

- **Fast tenants** benefit because heartbeats are batched into TigerBeetle efficiently, and even worst-case batch sizes remain tiny relative to TigerBeetle’s capacity
- **Slow tenants** do not pay a penalty for the system being optimized around bursts; each tenant has its own workflow boundary and its own ordered accounting path
- **Noisy neighbors are constrained** because accounting is partitioned per tenant rather than mixed into one shared queue with complicated cross-tenant ordering concerns

The batch sizing in the design makes the headroom story especially compelling: even a safety-cap-hit scenario of roughly **350 TigerBeetle transfers per flush** is still a tiny fraction of TigerBeetle’s throughput envelope. The architecture also adapts flush intervals based on observed TigerBeetle activity time, batching more aggressively when needed and flushing sooner when the ledger is fast.

### Throughput story: why TigerBeetle is the right center of gravity

At the projected initial scale, the system sees roughly **100 tenants × 1 heartbeat / 10s = 10 heartbeats/sec**, which maps to about **~200 TigerBeetle transfers/sec** at full metric scope. That is intentionally far below the throughput threshold where a Kafka-style streaming architecture becomes compelling.

The point of this design is not to optimize message-bus throughput. It is to maximize **correctness, simplicity, and ledger-backed enforcement** at the actual operating scale.

The key stakeholder takeaway is:

**We are not building a throughput bottleneck around TigerBeetle; we are building an architecture that lets TigerBeetle remain dramatically underutilized while still serving as the hard-limit authority.**

### Demo flow

The demo will show a complete end-to-end loop:

1. **Provision tenant wallets** with plan credits in TigerBeetle
2. **Start simulated clusters** that send usage heartbeats at different rates
3. **Record usage** into TigerBeetle through the tenant workflow
4. **Show PostgreSQL snapshots** updating for dashboard/reporting use
5. **Drive one tenant to quota exhaustion** and show TigerBeetle reject further usage atomically
6. **Apply a manual top-up / surge pack** and show the tenant resume successfully
7. **Replay duplicate heartbeats** and show idempotent behavior with no double charging

### What stakeholders will see

By the end of the PoC, stakeholders should be able to see and believe three things:

- **Correctness:** every accepted heartbeat becomes an auditable ledger-backed usage effect
- **Isolation:** tenant throughput differences do not break the accounting model
- **Scalability with headroom:** the projected workload is small relative to TigerBeetle’s capacity, and the batching/adaptive flush design gives further margin

### Success criteria

This PoC is successful if it demonstrates:

- exactly-once usage effect despite retries or duplicate heartbeats
- hard quota rejection enforced by TigerBeetle rather than app logic
- accurate, easy-to-read quota snapshots in PostgreSQL
- smooth handling of tenants with different heartbeat rates
- clear throughput headroom at the projected launch scale

### Bottom line

This PoC is not just a metering demo. It is a proof that the proposed control-plane architecture uses **TigerBeetle as a true system primitive**: not merely as storage, but as the **atomic enforcement engine** around which throughput, batching, ordering, and reporting are designed.

That is what gives the system both credibility and leverage as the tenant base grows — whether tenants are quiet, bursty, slow, or fast.
