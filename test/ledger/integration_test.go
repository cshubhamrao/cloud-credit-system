package ledger_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// getTBClient creates a TigerBeetle client for integration tests.
// Set TIGERBEETLE_ADDR environment variable to override the default.
func getTBClient(t *testing.T) *ledger.Client {
	t.Helper()
	addr := os.Getenv("TIGERBEETLE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:3000"
	}
	client, err := ledger.NewClient(0, addr)
	require.NoError(t, err, "failed to connect to TigerBeetle")
	return client
}

// TestIntegration_CreateGlobalAccounts creates the global operator and sink accounts.
func TestIntegration_CreateGlobalAccounts(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	idMap := make(map[ledger.GlobalAccountKey]types.Uint128)
	result, err := ledger.CreateGlobalAccounts(c, idMap)
	require.NoError(t, err)

	// Should create 6 accounts: (operator + sink) × 3 resources
	assert.Len(t, result, 6)

	// Verify each resource has operator and sink
	for _, r := range domain.AllResources {
		opKey := ledger.GlobalAccountKey{Resource: r, AccountType: domain.AccountTypeOperator}
		sinkKey := ledger.GlobalAccountKey{Resource: r, AccountType: domain.AccountTypeSink}
		assert.Contains(t, result, opKey, "missing operator account for %s", r)
		assert.Contains(t, result, sinkKey, "missing sink account for %s", r)
	}
}

// TestIntegration_CreateGlobalAccounts_Idempotent verifies that creating accounts
// again succeeds (AccountExists is treated as success).
func TestIntegration_CreateGlobalAccounts_Idempotent(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	idMap := make(map[ledger.GlobalAccountKey]types.Uint128)

	// First call
	_, err := ledger.CreateGlobalAccounts(c, idMap)
	require.NoError(t, err)

	// Second call should also succeed
	_, err = ledger.CreateGlobalAccounts(c, idMap)
	require.NoError(t, err)
}

// TestIntegration_TenantQuotaAccount_HasDebitsMustNotExceedCreditsFlag verifies
// that tenant quota accounts are created with the hard limit enforcement flag.
func TestIntegration_TenantQuotaAccount_HasDebitsMustNotExceedCreditsFlag(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	// First create global accounts
	_, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	accountIDs, err := ledger.CreateTenantQuotaAccounts(c, tenantUUID, nil)
	require.NoError(t, err)
	require.Len(t, accountIDs, 3)

	// Look up one account and verify it has the DebitsMustNotExceedCredits flag
	for r, id := range accountIDs {
		acc, err := ledger.LookupAccount(c, id)
		require.NoError(t, err)
		require.NotNil(t, acc, "account not found for resource %s", r)

		flags := acc.AccountFlags()
		assert.True(t, flags.DebitsMustNotExceedCredits,
			"account for %s should have DebitsMustNotExceedCredits flag (I-3)", r)
		assert.True(t, flags.History,
			"account for %s should have History flag", r)
		break // Just check one
	}
}

// TestIntegration_AllocateAndUse tests the full lifecycle: allocate credits, then use them.
func TestIntegration_AllocateAndUse(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	// Setup global accounts
	globalIDs, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	// Use a unique tenant UUID per run to avoid cross-run state pollution.
	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	tenantAccounts, err := ledger.CreateTenantQuotaAccounts(c, tenantUUID, nil)
	require.NoError(t, err)

	// Get IDs for cpu_hours
	cpuResource := domain.ResourceCPUHours
	opID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeOperator}]
	sinkID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeSink}]
	quotaID := tenantAccounts[cpuResource]

	// Allocate 1000 credits
	allocations := []ledger.AllocationTransfer{
		{
			Resource:      cpuResource,
			OperatorID:    opID,
			TenantQuotaID: quotaID,
			Amount:        1000,
			Code:          domain.CodeQuotaAllocation,
			TenantUUID:    tenantUUID,
		},
	}
	_, err = ledger.SubmitAllocations(c, allocations)
	require.NoError(t, err)

	// Verify balance
	balances, err := ledger.LookupBalances(c, map[string][16]byte{"cpu_hours": ledger.Uint128ToBytes(quotaID)})
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), balances["cpu_hours"].CreditsPosted)
	assert.Equal(t, uint64(0), balances["cpu_hours"].DebitsPosted)

	// Use 300 credits — random transfer ID (not testing idempotency here).
	transfers := []ledger.UsageTransfer{
		{
			Resource:      cpuResource,
			OperatorID:    opID,
			TenantQuotaID: quotaID,
			SinkID:        sinkID,
			Amount:        300,
			TransferID:    ledger.RandomID(),
		},
	}
	results, err := ledger.SubmitUsageBatch(c, transfers)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Accepted, "transfer should be accepted")

	// Verify new balance
	balances, err = ledger.LookupBalances(c, map[string][16]byte{"cpu_hours": ledger.Uint128ToBytes(quotaID)})
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), balances["cpu_hours"].CreditsPosted)
	assert.Equal(t, uint64(300), balances["cpu_hours"].DebitsPosted)
}

// TestIntegration_ExceedsCredits verifies hard limit rejection when credits are exhausted.
func TestIntegration_ExceedsCredits(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	// Setup
	globalIDs, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	tenantAccounts, err := ledger.CreateTenantQuotaAccounts(c, tenantUUID, nil)
	require.NoError(t, err)

	cpuResource := domain.ResourceCPUHours
	opID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeOperator}]
	quotaID := tenantAccounts[cpuResource]
	sinkID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeSink}]

	// Allocate only 100 credits
	allocations := []ledger.AllocationTransfer{
		{
			Resource:      cpuResource,
			OperatorID:    opID,
			TenantQuotaID: quotaID,
			Amount:        100,
			Code:          domain.CodeQuotaAllocation,
			TenantUUID:    tenantUUID,
		},
	}
	_, err = ledger.SubmitAllocations(c, allocations)
	require.NoError(t, err)

	// Try to use 200 credits (exceeds balance) — random ID, not testing idempotency.
	transfers := []ledger.UsageTransfer{
		{
			Resource:      cpuResource,
			OperatorID:    opID,
			TenantQuotaID: quotaID,
			SinkID:        sinkID,
			Amount:        200,
			TransferID:    ledger.RandomID(),
		},
	}
	results, err := ledger.SubmitUsageBatch(c, transfers)
	require.NoError(t, err)
	require.Len(t, results, 1)

	// Should be rejected by TigerBeetle (I-3: hard limit from TB only)
	assert.False(t, results[0].Accepted, "transfer should be rejected")
	assert.True(t, results[0].ExceedsCredits, "rejection should be due to exceeding credits")
}

// TestIntegration_ExceedsCredits_OtherResourcesUnaffected verifies that exhausting
// one resource doesn't affect others (independent per-ledger enforcement).
func TestIntegration_ExceedsCredits_OtherResourcesUnaffected(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	// Setup
	globalIDs, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	tenantAccounts, err := ledger.CreateTenantQuotaAccounts(c, tenantUUID, nil)
	require.NoError(t, err)

	cpuResource := domain.ResourceCPUHours
	memResource := domain.ResourceMemoryGBHours

	cpuOpID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeOperator}]
	cpuQuotaID := tenantAccounts[cpuResource]
	cpuSinkID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeSink}]

	memOpID := globalIDs[ledger.GlobalAccountKey{Resource: memResource, AccountType: domain.AccountTypeOperator}]
	memQuotaID := tenantAccounts[memResource]
	memSinkID := globalIDs[ledger.GlobalAccountKey{Resource: memResource, AccountType: domain.AccountTypeSink}]

	// Allocate 100 CPU, 1000 Memory
	allocations := []ledger.AllocationTransfer{
		{Resource: cpuResource, OperatorID: cpuOpID, TenantQuotaID: cpuQuotaID, Amount: 100, Code: domain.CodeQuotaAllocation, TenantUUID: tenantUUID},
		{Resource: memResource, OperatorID: memOpID, TenantQuotaID: memQuotaID, Amount: 1000, Code: domain.CodeQuotaAllocation, TenantUUID: tenantUUID},
	}
	_, err = ledger.SubmitAllocations(c, allocations)
	require.NoError(t, err)

	// Exhaust CPU — random IDs since we're testing limit enforcement, not idempotency.
	cpuTransfer := ledger.UsageTransfer{
		Resource:      cpuResource,
		OperatorID:    cpuOpID,
		TenantQuotaID: cpuQuotaID,
		SinkID:        cpuSinkID,
		Amount:        100,
		TransferID:    ledger.RandomID(),
	}
	results, err := ledger.SubmitUsageBatch(c, []ledger.UsageTransfer{cpuTransfer})
	require.NoError(t, err)
	assert.True(t, results[0].Accepted, "CPU transfer should be accepted")

	// Try to use more CPU (should fail)
	cpuTransfer2 := ledger.UsageTransfer{
		Resource:      cpuResource,
		OperatorID:    cpuOpID,
		TenantQuotaID: cpuQuotaID,
		SinkID:        cpuSinkID,
		Amount:        50,
		TransferID:    ledger.RandomID(),
	}
	results, err = ledger.SubmitUsageBatch(c, []ledger.UsageTransfer{cpuTransfer2})
	require.NoError(t, err)
	assert.False(t, results[0].Accepted, "CPU transfer should be rejected")
	assert.True(t, results[0].ExceedsCredits)

	// But memory should still work
	memTransfer := ledger.UsageTransfer{
		Resource:      memResource,
		OperatorID:    memOpID,
		TenantQuotaID: memQuotaID,
		SinkID:        memSinkID,
		Amount:        500,
		TransferID:    ledger.RandomID(),
	}
	results, err = ledger.SubmitUsageBatch(c, []ledger.UsageTransfer{memTransfer})
	require.NoError(t, err)
	assert.True(t, results[0].Accepted, "Memory transfer should be accepted (I-3: independent enforcement)")
}

// TestIntegration_DeterministicTransferID_Idempotent verifies that using the same
// (clusterID, seq, ledgerID) produces the same transfer ID, and TB returns Exists.
func TestIntegration_DeterministicTransferID_Idempotent(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	// Setup
	globalIDs, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	// Random tenant and cluster UUIDs per run — DeriveTransferID inputs change,
	// but the test still verifies that calling it TWICE with the SAME inputs yields the SAME ID.
	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	tenantAccounts, err := ledger.CreateTenantQuotaAccounts(c, tenantUUID, nil)
	require.NoError(t, err)

	cpuResource := domain.ResourceCPUHours
	opID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeOperator}]
	quotaID := tenantAccounts[cpuResource]
	sinkID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeSink}]

	// Allocate credits
	allocations := []ledger.AllocationTransfer{
		{Resource: cpuResource, OperatorID: opID, TenantQuotaID: quotaID, Amount: 1000, Code: domain.CodeQuotaAllocation, TenantUUID: tenantUUID},
	}
	_, err = ledger.SubmitAllocations(c, allocations)
	require.NoError(t, err)

	clusterUUID := ledger.Uint128ToBytes(ledger.RandomID()) // unique per run
	seqNum := uint64(42)
	ledgerID := cpuResource.LedgerID()

	// First transfer
	transfer1 := ledger.UsageTransfer{
		Resource:      cpuResource,
		OperatorID:    opID,
		TenantQuotaID: quotaID,
		SinkID:        sinkID,
		Amount:        100,
		TransferID:    ledger.DeriveTransferID(clusterUUID, seqNum, ledgerID),
		ClusterUUID:   clusterUUID,
	}
	results, err := ledger.SubmitUsageBatch(c, []ledger.UsageTransfer{transfer1})
	require.NoError(t, err)
	assert.True(t, results[0].Accepted)
	assert.False(t, results[0].Exists)

	// Same transfer again (same clusterUUID, seqNum, ledgerID)
	transfer2 := ledger.UsageTransfer{
		Resource:      cpuResource,
		OperatorID:    opID,
		TenantQuotaID: quotaID,
		SinkID:        sinkID,
		Amount:        100,
		TransferID:    ledger.DeriveTransferID(clusterUUID, seqNum, ledgerID), // Same ID!
		ClusterUUID:   clusterUUID,
	}
	results, err = ledger.SubmitUsageBatch(c, []ledger.UsageTransfer{transfer2})
	require.NoError(t, err)
	// Should be idempotent - not rejected, but marked as Exists
	assert.True(t, results[0].Accepted, "duplicate transfer should still be accepted (idempotent)")
	assert.True(t, results[0].Exists, "duplicate transfer should return Exists=true (I-2 layer 3)")

	// Verify balance wasn't double-counted
	balances, err := ledger.LookupBalances(c, map[string][16]byte{"cpu_hours": ledger.Uint128ToBytes(quotaID)})
	require.NoError(t, err)
	assert.Equal(t, uint64(100), balances["cpu_hours"].DebitsPosted, "debits should not be double-counted")
}

// TestIntegration_LookupAccountBalances verifies fetching balances after transfers.
func TestIntegration_LookupAccountBalances(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	// Setup
	globalIDs, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	tenantAccounts, err := ledger.CreateTenantQuotaAccounts(c, tenantUUID, nil)
	require.NoError(t, err)

	cpuResource := domain.ResourceCPUHours
	opID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeOperator}]
	quotaID := tenantAccounts[cpuResource]

	// Allocate 5000 credits
	allocations := []ledger.AllocationTransfer{
		{Resource: cpuResource, OperatorID: opID, TenantQuotaID: quotaID, Amount: 5000, Code: domain.CodeQuotaAllocation, TenantUUID: tenantUUID},
	}
	_, err = ledger.SubmitAllocations(c, allocations)
	require.NoError(t, err)

	// Lookup balances
	accountIDs := map[string][16]byte{"cpu_hours": ledger.Uint128ToBytes(quotaID)}
	balances, err := ledger.LookupBalances(c, accountIDs)
	require.NoError(t, err)

	assert.Equal(t, uint64(5000), balances["cpu_hours"].CreditsPosted)
	assert.Equal(t, uint64(0), balances["cpu_hours"].DebitsPosted)
	assert.Equal(t, uint64(0), balances["cpu_hours"].DebitsPending)
}

// TestIntegration_DeterministicAllocationID_Idempotent verifies I-5: submitting the same
// allocation transfer twice with the same deterministic ID is idempotent — TigerBeetle
// returns TransferExists and the balance is not double-credited.
//
// This is the safety net for Temporal activity retries: if SubmitAllocationTransfers
// commits to TB but the response is lost, the retry uses the same DeriveAllocationTransferID
// output and TB accepts it without creating a duplicate credit.
func TestIntegration_DeterministicAllocationID_Idempotent(t *testing.T) {
	c := getTBClient(t)
	defer c.Close()

	globalIDs, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	tenantAccounts, err := ledger.CreateTenantQuotaAccounts(c, tenantUUID, nil)
	require.NoError(t, err)

	cpuResource := domain.ResourceCPUHours
	opID := globalIDs[ledger.GlobalAccountKey{Resource: cpuResource, AccountType: domain.AccountTypeOperator}]
	quotaID := tenantAccounts[cpuResource]

	// Derive a deterministic ID (simulates what flushAdjustment does in the workflow).
	seqNo := uint64(1)
	transferID := ledger.DeriveAllocationTransferID(tenantUUID, seqNo, cpuResource.LedgerID())

	mkAllocation := func() ledger.AllocationTransfer {
		return ledger.AllocationTransfer{
			Resource:      cpuResource,
			OperatorID:    opID,
			TenantQuotaID: quotaID,
			Amount:        1000,
			Code:          domain.CodeQuotaAllocation,
			TenantUUID:    tenantUUID,
			TransferID:    transferID,
		}
	}

	// First submission — should be accepted.
	results, err := ledger.SubmitAllocations(c, []ledger.AllocationTransfer{mkAllocation()})
	require.NoError(t, err)
	assert.True(t, results[0].Accepted, "first allocation should be accepted")
	assert.False(t, results[0].Exists)

	// Second submission — same ID, simulates Temporal activity retry.
	results, err = ledger.SubmitAllocations(c, []ledger.AllocationTransfer{mkAllocation()})
	require.NoError(t, err)
	assert.True(t, results[0].Accepted, "duplicate allocation should be accepted (idempotent)")
	assert.True(t, results[0].Exists, "duplicate allocation should return Exists=true (I-5)")

	// Verify balance was credited exactly once.
	balances, err := ledger.LookupBalances(c, map[string][16]byte{"cpu_hours": ledger.Uint128ToBytes(quotaID)})
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), balances["cpu_hours"].CreditsPosted,
		"credits must not be double-counted on activity retry")
}
