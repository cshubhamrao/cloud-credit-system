# cloud-credit-system

Resource accounting and quota enforcement for BYOC (Bring-Your-Own-Cloud) deployments.

## What is this?

A central control plane that manages tenant workload clusters, tracking resource consumption via a double-entry ledger and enforcing hard/soft limits on sliceable resources.

**Tracked resources:**

- CPU hours, memory, storage, network egress
- Active nodes, active users
- Exotic resources (GPU slices, etc.)

**How it works:**

- Workload clusters heartbeat to the control plane every ~60 seconds
- Heartbeats report resource usage (deltas for cumulative, gauges for point-in-time)
- Control Plane records usage in TigerBeetle, checks quotas, returns status + commands
- Hard limits enforced atomically; soft limits trigger warnings
- Missed heartbeats → degraded → kill command
