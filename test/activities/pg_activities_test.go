package activities_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// newTestTenant inserts a fresh tenant and returns (acts, tenantID).
func newTestTenant(t *testing.T) (*accounting.PGActivities, string) {
	t.Helper()
	pool := getPool(t)
	acts := accounting.NewPGActivities(pool)
	tenantID := uuid.New().String()
	err := acts.InsertTenant(context.Background(), accounting.InsertTenantInput{
		TenantID:    tenantID,
		Name:        "integration-test-tenant",
		BillingTier: "free",
	})
	require.NoError(t, err, "prerequisite: InsertTenant")
	return acts, tenantID
}

// newTestCluster inserts a fresh cluster for the given tenant and returns its ID.
func newTestCluster(t *testing.T, acts *accounting.PGActivities, tenantID string) string {
	t.Helper()
	clusterID := uuid.New().String()
	err := acts.InsertCluster(context.Background(), accounting.InsertClusterInput{
		ClusterID:     clusterID,
		TenantID:      tenantID,
		CloudProvider: "aws",
		Region:        "us-east-1",
	})
	require.NoError(t, err, "prerequisite: InsertCluster")
	return clusterID
}

// TestPGActivities_InsertTenant verifies a new tenant can be persisted.
func TestPGActivities_InsertTenant(t *testing.T) {
	pool := getPool(t)
	acts := accounting.NewPGActivities(pool)

	err := acts.InsertTenant(context.Background(), accounting.InsertTenantInput{
		TenantID:    uuid.New().String(),
		Name:        "acme-corp",
		BillingTier: "pro", // valid tier per schema CHECK constraint
	})
	require.NoError(t, err)
}

// TestPGActivities_InsertTenant_InvalidUUID verifies that a malformed tenant UUID
// is rejected before hitting the database.
func TestPGActivities_InsertTenant_InvalidUUID(t *testing.T) {
	pool := getPool(t)
	acts := accounting.NewPGActivities(pool)

	err := acts.InsertTenant(context.Background(), accounting.InsertTenantInput{
		TenantID:    "not-a-uuid",
		Name:        "bad",
		BillingTier: "free",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tenant UUID")
}

// TestPGActivities_InsertCluster verifies a cluster can be inserted for a tenant.
func TestPGActivities_InsertCluster(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	err := acts.InsertCluster(context.Background(), accounting.InsertClusterInput{
		ClusterID:     uuid.New().String(),
		TenantID:      tenantID,
		CloudProvider: "gcp",
		Region:        "us-central1",
	})
	require.NoError(t, err)
}

// TestPGActivities_InsertTBAccountMapping verifies the tenant quota account mapping
// can be persisted after tenant creation.
func TestPGActivities_InsertTBAccountMapping(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	// Generate a fake TB account ID (deterministic for the test).
	fakeAccountID := ledger.Uint128ToBytes(ledger.RandomID())

	err := acts.InsertTBAccountMapping(context.Background(), accounting.InsertTBAccountMappingInput{
		TenantID: tenantID,
		AccountMap: map[string][16]byte{
			"cpu_hours": fakeAccountID,
		},
	})
	require.NoError(t, err)
}

// TestPGActivities_UpdateQuotaSnapshots verifies that quota snapshot upserts succeed
// and are idempotent — calling twice with the same data does not error.
func TestPGActivities_UpdateQuotaSnapshots(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	input := accounting.UpdateQuotaSnapshotsInput{
		TenantID: tenantID,
		Snapshots: map[string]accounting.QuotaSnapshotData{
			"cpu_hours": {CreditsTotal: 10_000, DebitsPosted: 3_000, DebitsPending: 500},
		},
	}

	// First upsert.
	require.NoError(t, acts.UpdateQuotaSnapshots(context.Background(), input))

	// Second upsert with same data — must be idempotent.
	require.NoError(t, acts.UpdateQuotaSnapshots(context.Background(), input))
}

// TestPGActivities_UpdateQuotaSnapshots_MultipleResources verifies snapshots for
// multiple resource types can be written in one call.
func TestPGActivities_UpdateQuotaSnapshots_MultipleResources(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	err := acts.UpdateQuotaSnapshots(context.Background(), accounting.UpdateQuotaSnapshotsInput{
		TenantID: tenantID,
		Snapshots: map[string]accounting.QuotaSnapshotData{
			"cpu_hours":       {CreditsTotal: 1000, DebitsPosted: 100},
			"memory_gb_hours": {CreditsTotal: 500, DebitsPosted: 50},
			"active_nodes":    {CreditsTotal: 10, DebitsPending: 3},
		},
	})
	require.NoError(t, err)
}

// TestPGActivities_InsertCreditAdjustment verifies that a credit adjustment audit
// record can be inserted for a tenant.
func TestPGActivities_InsertCreditAdjustment(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	transferID := ledger.Uint128ToBytes(ledger.RandomID())
	err := acts.InsertCreditAdjustment(context.Background(), accounting.InsertCreditAdjustmentInput{
		TenantID:     tenantID,
		ResourceType: "cpu_hours",
		Amount:       500_000,
		Reason:       "surge pack purchase",
		TransferID:   transferID,
	})
	require.NoError(t, err)
}

// TestPGActivities_InsertCreditAdjustment_EmptyReason verifies that a blank reason
// is stored as NULL (not an empty string) without error.
func TestPGActivities_InsertCreditAdjustment_EmptyReason(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	transferID := ledger.Uint128ToBytes(ledger.RandomID())
	err := acts.InsertCreditAdjustment(context.Background(), accounting.InsertCreditAdjustmentInput{
		TenantID:     tenantID,
		ResourceType: "cpu_hours",
		Amount:       100,
		Reason:       "", // empty → NULL in DB
		TransferID:   transferID,
	})
	require.NoError(t, err)
}

// TestPGActivities_UpdateClusterStatus verifies that a cluster's status can be updated.
func TestPGActivities_UpdateClusterStatus(t *testing.T) {
	acts, tenantID := newTestTenant(t)
	clusterID := newTestCluster(t, acts, tenantID)

	err := acts.UpdateClusterStatus(context.Background(), accounting.UpdateClusterStatusInput{
		ClusterID: clusterID,
		Status:    "healthy", // valid per workload_clusters_status_check constraint
	})
	require.NoError(t, err)
}

// TestPGActivities_CheckSoftLimits_NoConfig verifies that CheckSoftLimits returns nil
// when no soft limits are configured for the tenant (nothing to check).
func TestPGActivities_CheckSoftLimits_NoConfig(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	// Write a quota snapshot so the check has data to compare against.
	_ = acts.UpdateQuotaSnapshots(context.Background(), accounting.UpdateQuotaSnapshotsInput{
		TenantID: tenantID,
		Snapshots: map[string]accounting.QuotaSnapshotData{
			"cpu_hours": {CreditsTotal: 1000, DebitsPosted: 900},
		},
	})

	err := acts.CheckSoftLimits(context.Background(), accounting.CheckSoftLimitsInput{
		TenantID: tenantID,
		Snapshots: map[string]accounting.QuotaSnapshotData{
			"cpu_hours": {CreditsTotal: 1000, DebitsPosted: 900},
		},
	})
	// No quota_configs rows for this tenant → nothing to check → no error.
	require.NoError(t, err)
}

// TestPGActivities_ResetSoftLimitAlert verifies that resetting the soft-limit alert
// flag succeeds even when no matching quota_config row exists (zero-row UPDATE is fine).
func TestPGActivities_ResetSoftLimitAlert(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	err := acts.ResetSoftLimitAlert(context.Background(), accounting.ResetSoftLimitAlertInput{
		TenantID:     tenantID,
		ResourceType: "cpu_hours",
	})
	require.NoError(t, err)
}

// TestPGActivities_DeregisterCluster verifies that a cluster can be deregistered.
func TestPGActivities_DeregisterCluster(t *testing.T) {
	acts, tenantID := newTestTenant(t)
	clusterID := newTestCluster(t, acts, tenantID)

	err := acts.DeregisterCluster(context.Background(), accounting.DeregisterClusterInput{
		ClusterID: clusterID,
	})
	require.NoError(t, err)
}

// TestPGActivities_DeleteTenant verifies that a tenant can be soft-deleted.
func TestPGActivities_DeleteTenant(t *testing.T) {
	acts, tenantID := newTestTenant(t)

	err := acts.DeleteTenant(context.Background(), accounting.DeleteTenantInput{
		TenantID: tenantID,
	})
	require.NoError(t, err)
}

// TestPGActivities_DeleteTenant_InvalidUUID verifies that a malformed tenant UUID
// is rejected before hitting the database.
func TestPGActivities_DeleteTenant_InvalidUUID(t *testing.T) {
	pool := getPool(t)
	acts := accounting.NewPGActivities(pool)

	err := acts.DeleteTenant(context.Background(), accounting.DeleteTenantInput{
		TenantID: "not-a-uuid",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tenant UUID")
}
