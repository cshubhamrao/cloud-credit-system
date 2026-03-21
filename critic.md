What still feels risky or under-specified

1. **The write amplification / cardinality may be higher than the opening math suggests.**
   The exec summary says ~20 metrics each and ~200 TB transfers/sec at the target scale, but later the examples and batching tables reason in terms of ~7 tracked ledgers/resources. That may just be “20 raw metrics collapse into ~7 billable resources,” but the doc should say that explicitly. Otherwise someone will read 20 heartbeat metrics and assume 20 ledger writes per heartbeat.

2. **Gauge-resource accounting is clever, but operationally the trickiest part of the whole system.**
   The pending-transfer refresh model is elegant, but it is also where you’ll likely get the nastiest edge cases: agent restarts during refresh, duplicated refresh attempts, long GC pauses, temporary loss of local persisted pending IDs, and timing sensitivity around 10s heartbeat vs 30s timeout. I don’t think the design is wrong; I think this area deserves a dedicated invariants section and probably its own failure matrix.

3. **`QuotaAdjustmentWorkflow` still reads slightly inconsistent with the single-writer story.**
   One section says quota adjustments should be routed into `TenantAccountingWorkflow` via signal to avoid concurrent writers. Another still describes `QuotaAdjustmentWorkflow` as directly submitting to TigerBeetle and then updating Postgres. Those two narratives conflict a bit. I’d normalize the doc so there is exactly one blessed path.

4. **Backpressure behavior is a bit thin for a control-plane doc.**
   “Gateway returns `RESOURCE_EXHAUSTED` and agents retry with backoff” is directionally fine, but the policy matters a lot: bounded queue size, what gets dropped first, whether stale heartbeats are coalesced, whether only the newest heartbeat per cluster should survive when overloaded, and whether command delivery has priority over metric ingestion. Right now it’s more of a note than a policy. 

5. **Usage correction is named, but not really modeled.**
   You define `USAGE_CORRECTION` (`201`), which is good, but I didn’t see a corresponding correction flow with invariants: can it only offset prior usage, can it be positive and negative, how does it link to the original transfer, how is it exposed in billing snapshots, and what authority path is required. That will matter the first time you discover bad metering or a bug in an agent release. 

6. **Multi-region is still at “open question” level, and that is probably the biggest architectural cliff ahead.**
   That is fine for v0.3, but it means the doc is strong for a **single-region control-plane design**, not yet a globally complete design. If multi-region is even a medium-term requirement, I’d explicitly stamp the current design as single-region by intent so nobody overreads it. 

What I would change before calling this “ready for implementation”

* Add a one-paragraph clarification near the top: **“20 reported metrics collapse into N billable ledgered resources.”**
* Make one canonical statement that **all tenant TB writes go through `TenantAccountingWorkflow`**, including credits/adjustments.
* Add an explicit **invariants section**. Example:

  * one tenant, one workflow writer
  * one heartbeat sequence processed at most once per cluster
  * hard limit correctness comes only from TigerBeetle
  * PostgreSQL snapshots are projections and may lag
  * gauge resources are represented only by active pending transfers
* Add a short **overload policy** section.
* Add a full **correction/reconciliation** section for `USAGE_CORRECTION`.
* Add a small **non-goals** section: single-region only for now, no adversarial metering guarantees from agent-reported data alone, etc.

Overall score:

* **Architecture quality:** 8.5/10
* **Clarity of reasoning:** 9/10
* **Implementation readiness:** 7.5/10
* **Biggest remaining gap:** operational semantics around corrections, overload, and gauge-resource edge cases
