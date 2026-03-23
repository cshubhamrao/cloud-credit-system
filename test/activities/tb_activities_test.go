package activities_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// TestTBActivities_CreateTenantTBAccounts verifies that quota accounts are created
// for all resource types and the returned IDs are non-zero.
func TestTBActivities_CreateTenantTBAccounts(t *testing.T) {
	c := getTBClient(t)
	acts := accounting.NewTBActivities(c)

	_, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	res, err := acts.CreateTenantTBAccounts(context.Background(), accounting.CreateTenantTBAccountsInput{
		TenantUUID: tenantUUID,
	})
	require.NoError(t, err)

	// One account per resource type (cpu_hours, memory_gb_hours, active_nodes).
	assert.Len(t, res.AccountIDs, 3)
	for resource, id := range res.AccountIDs {
		assert.NotEqual(t, [16]byte{}, id, "account ID for %s should be non-zero", resource)
	}
}

// TestTBActivities_CreateTenantTBAccounts_Idempotent verifies that creating accounts
// twice for the same tenant succeeds (TB AccountExists = idempotent).
func TestTBActivities_CreateTenantTBAccounts_Idempotent(t *testing.T) {
	c := getTBClient(t)
	acts := accounting.NewTBActivities(c)

	_, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	input := accounting.CreateTenantTBAccountsInput{TenantUUID: tenantUUID}

	_, err = acts.CreateTenantTBAccounts(context.Background(), input)
	require.NoError(t, err)

	// Second call must also succeed.
	_, err = acts.CreateTenantTBAccounts(context.Background(), input)
	require.NoError(t, err)
}

// TestTBActivities_SubmitAllocationTransfers verifies that allocating credits
// increments the tenant quota account's credit balance in TigerBeetle.
func TestTBActivities_SubmitAllocationTransfers(t *testing.T) {
	fx := newTBFixture(t)

	fx.allocate(t, map[string]int64{"cpu_hours": 5000})

	res, err := fx.acts.LookupAccountBalances(context.Background(), accounting.LookupAccountBalancesInput{
		TenantQuotaIDs: fx.quotaIDs,
	})
	require.NoError(t, err)

	snap := res.Balances["cpu_hours"]
	assert.Equal(t, int64(5000), snap.CreditsTotal, "credits should be 5000 after allocation")
	assert.Equal(t, int64(0), snap.DebitsPosted)
	assert.Equal(t, int64(0), snap.DebitsPending)
}

// TestTBActivities_SubmitTBBatch_Accepted submits a heartbeat batch and verifies
// the usage transfer is accepted when credits are available.
func TestTBActivities_SubmitTBBatch_Accepted(t *testing.T) {
	fx := newTBFixture(t)
	fx.allocate(t, map[string]int64{"cpu_hours": 10_000})

	clusterUUID := ledger.Uint128ToBytes(ledger.RandomID())
	result, err := fx.acts.SubmitTBBatch(context.Background(), accounting.TBBatchInput{
		TenantID:          "test-tenant",
		TenantUUID:        fx.tenantUUID,
		AccountMap:        accounting.TBAccountMap{TenantQuotaIDs: fx.quotaIDs},
		GlobalOperatorIDs: fx.opIDs,
		GlobalSinkIDs:     fx.sinkIDs,
		Heartbeats: []accounting.HeartbeatSignal{
			{
				ClusterID:            "cluster-a",
				ClusterUUID:          clusterUUID,
				SequenceNumber:       1,
				CPUMillisecondsDelta: 1000,
			},
		},
	})
	require.NoError(t, err)

	r := result.PerResource["cpu_hours"]
	assert.True(t, r.Accepted, "transfer should be accepted")
	assert.False(t, r.ExceedsCredits)
}

// TestTBActivities_SubmitTBBatch_ExceedsCredits verifies hard-limit enforcement:
// a usage batch is rejected when the tenant's quota is exhausted (I-3).
func TestTBActivities_SubmitTBBatch_ExceedsCredits(t *testing.T) {
	fx := newTBFixture(t)
	fx.allocate(t, map[string]int64{"cpu_hours": 100})

	clusterUUID := ledger.Uint128ToBytes(ledger.RandomID())
	result, err := fx.acts.SubmitTBBatch(context.Background(), accounting.TBBatchInput{
		TenantID:          "test-tenant",
		TenantUUID:        fx.tenantUUID,
		AccountMap:        accounting.TBAccountMap{TenantQuotaIDs: fx.quotaIDs},
		GlobalOperatorIDs: fx.opIDs,
		GlobalSinkIDs:     fx.sinkIDs,
		Heartbeats: []accounting.HeartbeatSignal{
			{
				ClusterID:            "cluster-a",
				ClusterUUID:          clusterUUID,
				SequenceNumber:       1,
				CPUMillisecondsDelta: 999_999, // far exceeds the 100 credit balance
			},
		},
	})
	require.NoError(t, err)

	r := result.PerResource["cpu_hours"]
	assert.False(t, r.Accepted, "transfer should be rejected — quota exhausted (I-3)")
	assert.True(t, r.ExceedsCredits, "rejection reason should be ExceedsCredits")
}

// TestTBActivities_SubmitTBBatch_Idempotent verifies that submitting the same heartbeat
// twice (same clusterUUID + seq) yields Exists=true and does not double-count usage (I-2).
func TestTBActivities_SubmitTBBatch_Idempotent(t *testing.T) {
	fx := newTBFixture(t)
	fx.allocate(t, map[string]int64{"cpu_hours": 10_000})

	clusterUUID := ledger.Uint128ToBytes(ledger.RandomID())
	hb := accounting.HeartbeatSignal{
		ClusterID:            "cluster-a",
		ClusterUUID:          clusterUUID,
		SequenceNumber:       42,
		CPUMillisecondsDelta: 500,
	}
	batchInput := accounting.TBBatchInput{
		TenantID:          "test-tenant",
		TenantUUID:        fx.tenantUUID,
		AccountMap:        accounting.TBAccountMap{TenantQuotaIDs: fx.quotaIDs},
		GlobalOperatorIDs: fx.opIDs,
		GlobalSinkIDs:     fx.sinkIDs,
		Heartbeats:        []accounting.HeartbeatSignal{hb},
	}

	// First submission.
	r1, err := fx.acts.SubmitTBBatch(context.Background(), batchInput)
	require.NoError(t, err)
	assert.True(t, r1.PerResource["cpu_hours"].Accepted)
	assert.False(t, r1.PerResource["cpu_hours"].Exists)

	// Identical submission — same deterministic transfer ID.
	r2, err := fx.acts.SubmitTBBatch(context.Background(), batchInput)
	require.NoError(t, err)
	assert.True(t, r2.PerResource["cpu_hours"].Accepted, "duplicate should still be accepted (idempotent)")
	assert.True(t, r2.PerResource["cpu_hours"].Exists, "duplicate should report Exists=true (I-2 layer 3)")

	// Balance must not be double-counted.
	bal, err := fx.acts.LookupAccountBalances(context.Background(), accounting.LookupAccountBalancesInput{
		TenantQuotaIDs: fx.quotaIDs,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(500), bal.Balances["cpu_hours"].DebitsPosted,
		"debit should be 500, not 1000 — no double-counting")
}

// TestTBActivities_LookupAccountBalances verifies balances after allocation and usage.
func TestTBActivities_LookupAccountBalances(t *testing.T) {
	fx := newTBFixture(t)
	fx.allocate(t, map[string]int64{"cpu_hours": 8000, "memory_gb_hours": 4000})

	res, err := fx.acts.LookupAccountBalances(context.Background(), accounting.LookupAccountBalancesInput{
		TenantQuotaIDs: fx.quotaIDs,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(8000), res.Balances["cpu_hours"].CreditsTotal)
	assert.Equal(t, int64(4000), res.Balances["memory_gb_hours"].CreditsTotal)
	assert.Equal(t, int64(0), res.Balances["cpu_hours"].DebitsPosted)
}

// TestTBActivities_SubmitTBBatch_MultiResource verifies that batches with multiple
// resource types are processed independently — a rejection on one does not affect others.
func TestTBActivities_SubmitTBBatch_MultiResource(t *testing.T) {
	fx := newTBFixture(t)
	// Allocate only CPU, not memory.
	fx.allocate(t, map[string]int64{"cpu_hours": 5000})

	clusterUUID := ledger.Uint128ToBytes(ledger.RandomID())
	result, err := fx.acts.SubmitTBBatch(context.Background(), accounting.TBBatchInput{
		TenantID:          "test-tenant",
		TenantUUID:        fx.tenantUUID,
		AccountMap:        accounting.TBAccountMap{TenantQuotaIDs: fx.quotaIDs},
		GlobalOperatorIDs: fx.opIDs,
		GlobalSinkIDs:     fx.sinkIDs,
		Heartbeats: []accounting.HeartbeatSignal{
			{
				ClusterID:            "cluster-a",
				ClusterUUID:          clusterUUID,
				SequenceNumber:       1,
				CPUMillisecondsDelta: 100,
				MemoryMBSecondsDelta: 999_999, // no memory credits allocated
			},
		},
	})
	require.NoError(t, err)

	assert.True(t, result.PerResource["cpu_hours"].Accepted, "cpu should be accepted")
	assert.True(t, result.PerResource["memory_gb_hours"].ExceedsCredits,
		"memory should be rejected (no credits allocated)")
}
