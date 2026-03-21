package accounting_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
)

// TestTenantProvisioningWorkflow_HappyPath verifies I-1: provisioning creates the
// single-writer TenantAccountingWorkflow. All 5 steps execute successfully:
// 1. InsertTenant (PG)
// 2. CreateTenantTBAccounts (TB)
// 3. InsertTBAccountMapping (PG)
// 4. SubmitAllocationTransfers (TB) — load wallet
// 5. SignalWithStart TenantAccountingWorkflow (child workflow)
func TestTenantProvisioningWorkflow_HappyPath(t *testing.T) {
	s := testsuite.WorkflowTestSuite{}
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(accounting.TenantProvisioningWorkflow)
	env.RegisterWorkflow(accounting.TenantAccountingWorkflow)
	// Register activity structs
	env.RegisterActivity(&accounting.TBActivities{})
	env.RegisterActivity(&accounting.PGActivities{})

	input := accounting.TenantProvisioningInput{
		TenantID:          "tenant-abc",
		TenantUUID:        [16]byte{1, 2, 3},
		Name:              "Test Tenant",
		BillingTier:       "pro",
		Credits:           map[string]int64{"cpu_hours": 1_000_000},
		GlobalOperatorIDs: map[string][16]byte{"cpu_hours": {10}},
		GlobalSinkIDs:     map[string][16]byte{"cpu_hours": {11}},
		PeriodStartNs:     time.Now().UnixNano(),
	}

	tbAccountResult := accounting.CreateTenantTBAccountsResult{
		AccountIDs: map[string][16]byte{
			"cpu_hours": {20, 21, 22},
		},
	}

	// Step 1: InsertTenant
	env.OnActivity("InsertTenant", mock.Anything, mock.MatchedBy(func(in accounting.InsertTenantInput) bool {
		return in.TenantID == "tenant-abc" && in.Name == "Test Tenant"
	})).Return(nil)

	// Step 2: CreateTenantTBAccounts
	env.OnActivity("CreateTenantTBAccounts", mock.Anything, mock.MatchedBy(func(in accounting.CreateTenantTBAccountsInput) bool {
		return in.TenantUUID == [16]byte{1, 2, 3}
	})).Return(tbAccountResult, nil)

	// Step 3: InsertTBAccountMapping
	env.OnActivity("InsertTBAccountMapping", mock.Anything, mock.MatchedBy(func(in accounting.InsertTBAccountMappingInput) bool {
		return in.TenantID == "tenant-abc" && len(in.AccountMap) == 1
	})).Return(nil)

	// Step 4: SubmitAllocationTransfers
	env.OnActivity("SubmitAllocationTransfers", mock.Anything, mock.MatchedBy(func(in accounting.SubmitAllocationInput) bool {
		return in.Credits["cpu_hours"] == 1_000_000
	})).Return(nil)

	// Step 5: TenantAccountingWorkflow runs forever — mock it to return immediately in tests.
	env.OnWorkflow(accounting.TenantAccountingWorkflow, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(accounting.TenantProvisioningWorkflow, input)

	assert.True(t, env.IsWorkflowCompleted())
	var result accounting.TenantProvisioningResult
	assert.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, tbAccountResult.AccountIDs, result.AccountMap)
}

// TestRegisterClusterWorkflow_HappyPath verifies the cluster registration workflow:
// 1. InsertCluster (PG)
// 2. Signal the tenant accounting workflow to initialise processedSeqs
func TestRegisterClusterWorkflow_HappyPath(t *testing.T) {
	s := testsuite.WorkflowTestSuite{}
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(accounting.RegisterClusterWorkflow)
	env.RegisterWorkflow(accounting.TenantAccountingWorkflow)
	// Register activity structs
	env.RegisterActivity(&accounting.TBActivities{})
	env.RegisterActivity(&accounting.PGActivities{})

	input := accounting.RegisterClusterWorkflowInput{
		ClusterID:     "cluster-xyz",
		TenantID:      "tenant-abc",
		CloudProvider: "aws",
		Region:        "us-east-1",
	}

	// Step 1: InsertCluster
	env.OnActivity("InsertCluster", mock.Anything, mock.MatchedBy(func(in accounting.InsertClusterInput) bool {
		return in.ClusterID == "cluster-xyz" && in.TenantID == "tenant-abc"
	})).Return(nil)

	// Step 2: SignalExternalWorkflow to tenant accounting workflow
	env.OnSignalExternalWorkflow(mock.Anything, "tenant-accounting-tenant-abc", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(accounting.RegisterClusterWorkflow, input)

	assert.True(t, env.IsWorkflowCompleted())
}
