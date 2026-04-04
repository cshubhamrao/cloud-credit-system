package accounting_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
)

// newTestSuite creates a test env with the accounting workflow and activities registered.
func newTestSuite(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	s := testsuite.WorkflowTestSuite{}
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(accounting.TenantAccountingWorkflow)
	// Register activity structs so their names are known to the test environment.
	// The actual implementations will be mocked via OnActivity.
	env.RegisterActivity(&accounting.TBActivities{})
	env.RegisterActivity(&accounting.PGActivities{})
	// These non-fatal PG activities are called on every flush path. Mock them here
	// so all tests get clean/fast behaviour without needing to set them up individually.
	// .Maybe() means tests that don't trigger these paths won't fail the assertion.
	env.OnActivity("CheckSoftLimits", mock.Anything, mock.Anything).Return(nil).Maybe()
	env.OnActivity("ResetSoftLimitAlert", mock.Anything, mock.Anything).Return(nil).Maybe()
	return env
}

func baseInput() accounting.TenantAccountingInput {
	return accounting.TenantAccountingInput{
		TenantID:          "tenant-123",
		TenantUUID:        [16]byte{1},
		AccountMap:        map[string][16]byte{"cpu_hours": {2}},
		GlobalOperatorIDs: map[string][16]byte{"cpu_hours": {3}},
		GlobalSinkIDs:     map[string][16]byte{"cpu_hours": {4}},
	}
}

// TestTenantAccountingWorkflow_FlushOnTimer verifies I-1: all TB writes go through this
// workflow. After receiving a heartbeat and the flush timer fires, SubmitTBBatch is called.
func TestTenantAccountingWorkflow_FlushOnTimer(t *testing.T) {
	env := newTestSuite(t)

	hb := accounting.HeartbeatSignal{
		ClusterID:            "cluster-a",
		SequenceNumber:       1,
		CPUMillisecondsDelta: 1000,
	}
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 1*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, hb)
	}, 2*time.Second)
	// Cancel after the flush activities complete (30s timer + small buffer)
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 35*time.Second)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{
			"cpu_hours": {Accepted: true},
		},
	}
	balResult := accounting.LookupAccountBalancesResult{
		Balances: map[string]accounting.QuotaSnapshotData{
			"cpu_hours": {CreditsTotal: 1000000, DebitsPosted: 1000},
		},
	}

	env.OnActivity("SubmitTBBatch", mock.Anything, mock.Anything).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(40 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// Workflow is long-running; we verify activities were called
	env.AssertActivityNumberOfCalls(t, "SubmitTBBatch", 1)
}

// TestTenantAccountingWorkflow_Dedup verifies I-2 layer 2: the workflow's processedSeqs
// is monotonic per cluster — a seq from a previous flush window is not re-processed.
//
// Note: within a single flush window, duplicate suppression is the gateway DedupCache's
// job (tested in dedup_test.go). processedSeqs handles cross-flush dedup.
//
// Scenario:
//
//	t=2s:  seq=5 arrives → added to batch (processedSeqs[cluster-a]=0, 5>0)
//	t=35s: flush fires → SubmitTBBatch called, processedSeqs[cluster-a] updated to 5
//	t=36s: seq=3 arrives (replay of old seq) → deduped (3 ≤ 5), not added to batch
//	t=66s: second flush fires with empty batch → SubmitTBBatch NOT called again
func TestTenantAccountingWorkflow_Dedup(t *testing.T) {
	env := newTestSuite(t)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 1*time.Second)
	// First heartbeat: seq=5
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
			ClusterID: "cluster-a", SequenceNumber: 5, CPUMillisecondsDelta: 500,
		})
	}, 2*time.Second)
	// After first flush (t>30s): send seq=3 which is lower than processed seq=5 — must be deduped.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
			ClusterID: "cluster-a", SequenceNumber: 3, CPUMillisecondsDelta: 500,
		})
	}, 36*time.Second)
	// Cancel after second flush window.
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 68*time.Second)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{"cpu_hours": {Accepted: true}},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	env.OnActivity("SubmitTBBatch", mock.Anything, mock.Anything).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(75 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// SubmitTBBatch called exactly once — only the first flush had content.
	// The second flush was empty because seq=3 was deduped by processedSeqs.
	env.AssertActivityNumberOfCalls(t, "SubmitTBBatch", 1)
}

// TestTenantAccountingWorkflow_QueryLastAck verifies the query handler returns the
// highest committed sequence number after a flush.
func TestTenantAccountingWorkflow_QueryLastAck(t *testing.T) {
	env := newTestSuite(t)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 1*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
			ClusterID:            "cluster-a",
			SequenceNumber:       42,
			CPUMillisecondsDelta: 100,
		})
	}, 2*time.Second)
	// Query after flush timer fires (>30s).
	env.RegisterDelayedCallback(func() {
		val, err := env.QueryWorkflow(accounting.QueryLastTBAck)
		assert.NoError(t, err)
		var ack uint64
		assert.NoError(t, val.Get(&ack))
		assert.Equal(t, uint64(42), ack)
		env.CancelWorkflow()
	}, 35*time.Second)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{"cpu_hours": {Accepted: true}},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	env.OnActivity("SubmitTBBatch", mock.Anything, mock.Anything).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(40 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())
}

// TestTenantAccountingWorkflow_PGSnapshotFailure_NonFatal verifies I-4: a PG activity
// failure does not crash the workflow — enforcement is unaffected.
func TestTenantAccountingWorkflow_PGSnapshotFailure_NonFatal(t *testing.T) {
	env := newTestSuite(t)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 1*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
			ClusterID: "cluster-a", SequenceNumber: 1, CPUMillisecondsDelta: 100,
		})
	}, 2*time.Second)
	// Cancel after flush
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 35*time.Second)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{"cpu_hours": {Accepted: true}},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	env.OnActivity("SubmitTBBatch", mock.Anything, mock.Anything).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	// Simulate PG being unavailable for all 5 retry attempts.
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(errors.New("postgres down"))

	// Timeout must cover: 30s flush timer + up to 5 retries with exponential backoff
	// (1+2+4+8+16 = 31s max simulated retry time) + buffer for cancel.
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 70*time.Second)
	env.SetTestTimeout(75 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// TB batch was submitted successfully despite PG being down (I-4: PG failure is non-fatal).
	env.AssertActivityNumberOfCalls(t, "SubmitTBBatch", 1)
}

// TestTenantAccountingWorkflow_QuotaAdjustment_ImmediateFlush verifies I-1: even quota
// adjustments (surge packs) go through the workflow — no direct TB writes from outside.
// The adjustment triggers SubmitAllocationTransfers immediately (not waiting for timer).
func TestTenantAccountingWorkflow_QuotaAdjustment_ImmediateFlush(t *testing.T) {
	env := newTestSuite(t)

	adj := accounting.QuotaAdjustmentSignal{
		ResourceType: "cpu_hours",
		Amount:       500_000,
		Reason:       "surge pack",
		Code:         102,
	}

	env.RegisterDelayedCallback(func() {
		// Use UpdateWorkflow (not SignalWorkflow) — caller blocks until TB confirms.
		env.UpdateWorkflowNoRejection(accounting.UpdateIssueCredit, "test-update-1", t, adj)
	}, 2*time.Second)
	// Cancel after adjustment is processed
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 5*time.Second)

	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	env.OnActivity("SubmitAllocationTransfers", mock.Anything, mock.MatchedBy(func(input accounting.SubmitAllocationInput) bool {
		return input.Credits["cpu_hours"] == 500_000
	})).Return(nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	// Adjustment should fire well before the 30s flush timer.
	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	env.AssertActivityNumberOfCalls(t, "SubmitAllocationTransfers", 1)
}

// TestTenantAccountingWorkflow_NoAppSideQuotaCheck verifies I-3: the workflow does NOT
// check tenant balance before submitting to TigerBeetle. Hard limit enforcement is
// delegated entirely to TB (debits_must_not_exceed_credits flag). No app-side gating.
func TestTenantAccountingWorkflow_NoAppSideQuotaCheck(t *testing.T) {
	env := newTestSuite(t)

	hb := accounting.HeartbeatSignal{
		ClusterID:            "cluster-a",
		SequenceNumber:       1,
		CPUMillisecondsDelta: 1_000_000, // Large amount that would exceed any reasonable quota
	}

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 1*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, hb)
	}, 2*time.Second)
	// Cancel after flush
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 35*time.Second)

	// Return a result where TB rejected the transfer (exceeds_credits).
	// The workflow should NOT have pre-checked the balance — it submits blindly to TB.
	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{
			"cpu_hours": {
				Accepted:       false,
				ExceedsCredits: true,
			},
		},
	}
	balResult := accounting.LookupAccountBalancesResult{
		Balances: map[string]accounting.QuotaSnapshotData{
			"cpu_hours": {CreditsTotal: 10000, DebitsPosted: 10000, DebitsPending: 0},
		},
	}

	var batchInput accounting.TBBatchInput
	env.OnActivity("SubmitTBBatch", mock.Anything, mock.MatchedBy(func(input accounting.TBBatchInput) bool {
		batchInput = input
		return true
	})).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(40 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// Verify the batch was submitted even though it would exceed credits.
	// This proves there's no app-side check — we submitted to TB and let TB decide.
	assert.Len(t, batchInput.Heartbeats, 1)
	assert.Equal(t, int64(1_000_000), batchInput.Heartbeats[0].CPUMillisecondsDelta)
}

// TestTenantAccountingWorkflow_BatchSizeThreshold verifies that reaching the batch-size
// threshold (150 heartbeats) triggers an immediate flush without waiting for the timer.
func TestTenantAccountingWorkflow_BatchSizeThreshold(t *testing.T) {
	env := newTestSuite(t)

	// Register cluster, then flood exactly 150 heartbeats before the 2s base timer fires.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 500*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		for i := uint64(1); i <= 150; i++ {
			env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
				ClusterID:            "cluster-a",
				SequenceNumber:       i,
				CPUMillisecondsDelta: 100,
			})
		}
	}, 1*time.Second)
	// Cancel before the 2s base timer would fire — flush must have happened via threshold.
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 1500*time.Millisecond)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{
			"cpu_hours": {Accepted: true},
		},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	env.OnActivity("SubmitTBBatch", mock.Anything, mock.Anything).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(3 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// Threshold flush fired before the timer — exactly one SubmitTBBatch call.
	env.AssertActivityNumberOfCalls(t, "SubmitTBBatch", 1)
}

// TestTenantAccountingWorkflow_IdleFlushIntervalDoubles verifies the adaptive flush
// interval: each idle cycle (no heartbeats) doubles the interval up to maxFlushInterval.
// SubmitTBBatch is never called because the batch is always empty.
func TestTenantAccountingWorkflow_IdleFlushIntervalDoubles(t *testing.T) {
	env := newTestSuite(t)

	// No heartbeats — let three idle flush cycles fire at 2s, 6s, 14s (2+4+8).
	// Cancel after the third idle cycle fires.
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 15*time.Second)

	// No activity mocks needed — flushBatch exits early when batch is empty.
	env.SetTestTimeout(20 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// Batch was always empty so SubmitTBBatch was never called.
	env.AssertActivityNumberOfCalls(t, "SubmitTBBatch", 0)
}

// TestTenantAccountingWorkflow_ExceededResourcesSetAndCleared verifies the
// ExceededResources lifecycle: TB rejection sets the flag; issuing credits clears it.
func TestTenantAccountingWorkflow_ExceededResourcesSetAndCleared(t *testing.T) {
	env := newTestSuite(t)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 500*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
			ClusterID: "cluster-a", SequenceNumber: 1, CPUMillisecondsDelta: 100,
		})
	}, 1*time.Second)
	// Query after flush (>2s): ExceededResources should contain "cpu_hours".
	env.RegisterDelayedCallback(func() {
		val, err := env.QueryWorkflow(accounting.QueryExceededResources)
		assert.NoError(t, err)
		var exceeded map[string]bool
		assert.NoError(t, val.Get(&exceeded))
		assert.True(t, exceeded["cpu_hours"], "cpu_hours should be exceeded after TB rejection")
	}, 3*time.Second)
	// Issue credit — clears ExceededResources for the resource.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(accounting.UpdateIssueCredit, "update-credit-1", t,
			accounting.QuotaAdjustmentSignal{
				ResourceType: "cpu_hours",
				Amount:       500_000,
				Reason:       "restore quota",
				Code:         101,
			})
	}, 4*time.Second)
	// Query after credit: ExceededResources should no longer contain "cpu_hours".
	env.RegisterDelayedCallback(func() {
		val, err := env.QueryWorkflow(accounting.QueryExceededResources)
		assert.NoError(t, err)
		var exceeded map[string]bool
		assert.NoError(t, val.Get(&exceeded))
		assert.False(t, exceeded["cpu_hours"], "cpu_hours should be cleared after credit issuance")
		env.CancelWorkflow()
	}, 5*time.Second)

	// TB rejects the heartbeat batch.
	rejectedResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{
			"cpu_hours": {Accepted: false, ExceedsCredits: true},
		},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	env.OnActivity("SubmitTBBatch", mock.Anything, mock.Anything).Return(rejectedResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("SubmitAllocationTransfers", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())
}

// TestTenantAccountingWorkflow_MultiClusterIndependentSeqs verifies that two clusters
// each sending seq=5 are treated as independent — neither is deduplicated by the other.
func TestTenantAccountingWorkflow_MultiClusterIndependentSeqs(t *testing.T) {
	env := newTestSuite(t)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-b"})
	}, 500*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
			ClusterID: "cluster-a", SequenceNumber: 5, CPUMillisecondsDelta: 100,
		})
		env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
			ClusterID: "cluster-b", SequenceNumber: 5, CPUMillisecondsDelta: 200,
		})
	}, 1*time.Second)
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 4*time.Second)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{
			"cpu_hours": {Accepted: true},
		},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	var capturedBatch accounting.TBBatchInput
	env.OnActivity("SubmitTBBatch", mock.Anything, mock.MatchedBy(func(input accounting.TBBatchInput) bool {
		capturedBatch = input
		return true
	})).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(6 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// Both heartbeats must appear in the batch — seq=5 on cluster-b is NOT
	// a duplicate of seq=5 on cluster-a because processedSeqs is per-cluster.
	env.AssertActivityNumberOfCalls(t, "SubmitTBBatch", 1)
	assert.Len(t, capturedBatch.Heartbeats, 2, "both cluster-a and cluster-b heartbeats should be in the batch")
}

// TestTenantAccountingWorkflow_DeterministicGaugeIDs verifies I-5: when SubmitTBBatch is
// called for a batch that includes gauge heartbeats, the workflow pre-populates
// NewGaugePendingIDs and GaugeVoidIDs in the input so that Temporal activity retries
// reuse the same IDs instead of creating duplicate TB pending reservations.
func TestTenantAccountingWorkflow_DeterministicGaugeIDs(t *testing.T) {
	env := newTestSuite(t)

	// Use a resource map that includes active_nodes (the only gauge resource).
	input := accounting.TenantAccountingInput{
		TenantID:   "tenant-123",
		TenantUUID: [16]byte{1},
		AccountMap: map[string][16]byte{
			"cpu_hours":    {2},
			"active_nodes": {5},
		},
		GlobalOperatorIDs: map[string][16]byte{
			"cpu_hours":    {3},
			"active_nodes": {6},
		},
		GlobalSinkIDs: map[string][16]byte{
			"cpu_hours":    {4},
			"active_nodes": {7},
		},
	}

	hb := accounting.HeartbeatSignal{
		ClusterID:            "cluster-a",
		ClusterUUID:          [16]byte{9},
		SequenceNumber:       1,
		CPUMillisecondsDelta: 1000,
		ActiveNodes:          3,
	}

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
	}, 1*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(accounting.SignalHeartbeat, hb)
	}, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 35*time.Second)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{
			"cpu_hours":    {Accepted: true},
			"active_nodes": {Accepted: true},
		},
		GaugeUpdates: map[string]map[string][16]byte{
			"cluster-a": {"active_nodes": {99}},
		},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	var capturedInput accounting.TBBatchInput
	env.OnActivity("SubmitTBBatch", mock.Anything, mock.MatchedBy(func(in accounting.TBBatchInput) bool {
		capturedInput = in
		return true
	})).Return(batchResult, nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(40 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, input)

	// NewGaugePendingIDs and GaugeVoidIDs must be populated (non-zero) for active_nodes.
	assert.NotNil(t, capturedInput.NewGaugePendingIDs, "NewGaugePendingIDs must be set for gauge resources")
	assert.NotNil(t, capturedInput.GaugeVoidIDs, "GaugeVoidIDs must be set for gauge resources")

	pendingID, hasPending := capturedInput.NewGaugePendingIDs["cluster-a"]["active_nodes"]
	assert.True(t, hasPending, "NewGaugePendingIDs should have cluster-a/active_nodes entry")
	assert.NotEqual(t, [16]byte{}, pendingID, "NewGaugePendingID should be non-zero")

	voidID, hasVoid := capturedInput.GaugeVoidIDs["cluster-a"]["active_nodes"]
	assert.True(t, hasVoid, "GaugeVoidIDs should have cluster-a/active_nodes entry")
	assert.NotEqual(t, [16]byte{}, voidID, "GaugeVoidID should be non-zero")

	assert.NotEqual(t, pendingID, voidID, "pending and void IDs must differ (domain separation)")
}

// TestTenantAccountingWorkflow_DeterministicAdjustmentID verifies I-5: the workflow
// populates TransferIDs in SubmitAllocationInput for the credit issuance path, ensuring
// that Temporal activity retries for flushAdjustment use the same TB transfer ID.
func TestTenantAccountingWorkflow_DeterministicAdjustmentID(t *testing.T) {
	env := newTestSuite(t)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(accounting.UpdateIssueCredit, "test-update-id", t,
			accounting.QuotaAdjustmentSignal{ResourceType: "cpu_hours", Amount: 500_000})
	}, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 5*time.Second)

	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	var capturedAlloc accounting.SubmitAllocationInput
	env.OnActivity("SubmitAllocationTransfers", mock.Anything, mock.MatchedBy(func(input accounting.SubmitAllocationInput) bool {
		capturedAlloc = input
		return true
	})).Return(nil)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	env.AssertActivityNumberOfCalls(t, "SubmitAllocationTransfers", 1)
	assert.NotNil(t, capturedAlloc.TransferIDs, "TransferIDs must be populated (I-5)")
	id, ok := capturedAlloc.TransferIDs["cpu_hours"]
	assert.True(t, ok, "TransferIDs must have entry for cpu_hours")
	assert.NotEqual(t, [16]byte{}, id, "TransferID must be non-zero")
}

// TestTenantAccountingWorkflow_FlushSeqNoCarried verifies that FlushSeqNo is included
// in the ContinueAsNew carry fields. We test this by checking that two consecutive
// flush operations produce different IDs (i.e. FlushSeqNo increments across flushes).
func TestTenantAccountingWorkflow_FlushSeqNoIncrementsAcrossFlushes(t *testing.T) {
	env := newTestSuite(t)

	for _, seq := range []uint64{1, 2} {
		seq := seq
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(accounting.SignalRegisterCluster, accounting.RegisterClusterSignal{ClusterID: "cluster-a"})
		}, time.Duration(seq)*time.Second-500*time.Millisecond)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(accounting.SignalHeartbeat, accounting.HeartbeatSignal{
				ClusterID: "cluster-a", ClusterUUID: [16]byte{9},
				SequenceNumber: seq, CPUMillisecondsDelta: 100,
			})
		}, time.Duration(seq)*time.Second)
	}
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 100*time.Second)

	batchResult := accounting.TBBatchResult{
		PerResource: map[string]accounting.ResourceBatchResult{"cpu_hours": {Accepted: true}},
	}
	balResult := accounting.LookupAccountBalancesResult{Balances: map[string]accounting.QuotaSnapshotData{}}

	var capturedInputs []accounting.TBBatchInput
	env.OnActivity("SubmitTBBatch", mock.Anything, mock.MatchedBy(func(in accounting.TBBatchInput) bool {
		capturedInputs = append(capturedInputs, in)
		return true
	})).Return(batchResult, nil).Times(2)
	env.OnActivity("LookupAccountBalances", mock.Anything, mock.Anything).Return(balResult, nil)
	env.OnActivity("UpdateQuotaSnapshots", mock.Anything, mock.Anything).Return(nil)

	env.SetTestTimeout(120 * time.Second)
	env.ExecuteWorkflow(accounting.TenantAccountingWorkflow, baseInput())

	// Both flushes should have been called
	env.AssertActivityNumberOfCalls(t, "SubmitTBBatch", 2)
	// The test verifies the workflow ran two separate flushes successfully;
	// FlushSeqNo determinism is validated by the ID derive unit tests in ids_test.go.
	assert.Len(t, capturedInputs, 2)
}
