package accounting

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TenantProvisioningInput carries everything needed to bootstrap a new tenant.
type TenantProvisioningInput struct {
	TenantID          string
	TenantUUID        [16]byte
	Name              string
	BillingTier       string
	Credits           map[string]int64 // resource_type → initial credits
	GlobalOperatorIDs map[string][16]byte
	GlobalSinkIDs     map[string][16]byte
	PeriodStartNs     int64
}

// TenantProvisioningResult is returned after provisioning completes.
type TenantProvisioningResult struct {
	AccountMap map[string][16]byte // resource_type → TB account ID
}

// TenantProvisioningWorkflow orchestrates the creation of a new tenant:
//  1. InsertTenant (PG)
//  2. CreateTenantTBAccounts (TB)
//  3. InsertTBAccountMapping (PG)
//  4. SubmitAllocationTransfers (TB) — load the wallet
//  5. SignalWithStart TenantAccountingWorkflow
func TenantProvisioningWorkflow(ctx workflow.Context, input TenantProvisioningInput) (TenantProvisioningResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("TenantProvisioningWorkflow started", "tenantID", input.TenantID)

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 60 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 5,
			InitialInterval: 2 * time.Second,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	// Step 1: Insert tenant row in PG.
	pgInsertInput := InsertTenantInput{
		TenantID:    input.TenantID,
		Name:        input.Name,
		BillingTier: input.BillingTier,
	}
	if err := workflow.ExecuteActivity(ctx, pgActivities.InsertTenant, pgInsertInput).Get(ctx, nil); err != nil {
		return TenantProvisioningResult{}, fmt.Errorf("InsertTenant: %w", err)
	}

	// Step 2: Create TB quota accounts.
	var tbResult CreateTenantTBAccountsResult
	if err := workflow.ExecuteActivity(ctx,
		tbActivities.CreateTenantTBAccounts,
		CreateTenantTBAccountsInput{TenantUUID: input.TenantUUID},
	).Get(ctx, &tbResult); err != nil {
		return TenantProvisioningResult{}, fmt.Errorf("CreateTenantTBAccounts: %w", err)
	}

	// Step 3: Persist TB account ID mapping in PG.
	mappingInput := InsertTBAccountMappingInput{
		TenantID:   input.TenantID,
		AccountMap: tbResult.AccountIDs,
	}
	if err := workflow.ExecuteActivity(ctx, pgActivities.InsertTBAccountMapping, mappingInput).Get(ctx, nil); err != nil {
		return TenantProvisioningResult{}, fmt.Errorf("InsertTBAccountMapping: %w", err)
	}

	// Step 4: Load the wallet with initial allocation transfers.
	allocInput := SubmitAllocationInput{
		TenantUUID:        input.TenantUUID,
		TenantQuotaIDs:    tbResult.AccountIDs,
		GlobalOperatorIDs: input.GlobalOperatorIDs,
		Credits:           input.Credits,
		PeriodStartNs:     input.PeriodStartNs,
	}
	if err := workflow.ExecuteActivity(ctx, tbActivities.SubmitAllocationTransfers, allocInput).Get(ctx, nil); err != nil {
		return TenantProvisioningResult{}, fmt.Errorf("SubmitAllocationTransfers: %w", err)
	}

	// Step 5: Start (or signal-with-start) the long-running TenantAccountingWorkflow.
	accountingInput := TenantAccountingInput{
		TenantID:          input.TenantID,
		TenantUUID:        input.TenantUUID,
		AccountMap:        tbResult.AccountIDs,
		GlobalOperatorIDs: input.GlobalOperatorIDs,
		GlobalSinkIDs:     input.GlobalSinkIDs,
	}
	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID: AccountingWorkflowID(input.TenantID),
		TaskQueue:  TaskQueueAccounting,
		// ABANDON so the accounting workflow keeps running after provisioning completes.
		// Default (TERMINATE) would kill it when this workflow exits.
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	})
	// Wait for accounting workflow to start (not complete — it runs indefinitely).
	// This ensures the workflow exists before RegisterClusterWorkflow tries to signal it.
	childFuture := workflow.ExecuteChildWorkflow(childCtx, TenantAccountingWorkflow, accountingInput)
	if err := childFuture.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
		var alreadyStarted *temporal.ChildWorkflowExecutionAlreadyStartedError
		if errors.As(err, &alreadyStarted) {
			// Accounting workflow already running for this tenant (e.g. re-provisioning after restart).
			// Safe to reuse — it shares the same stable WorkflowID.
			logger.Info("TenantAccountingWorkflow already running, reusing", "tenantID", input.TenantID)
		} else {
			return TenantProvisioningResult{}, fmt.Errorf("start TenantAccountingWorkflow: %w", err)
		}
	}

	logger.Info("TenantProvisioningWorkflow complete", "tenantID", input.TenantID)
	return TenantProvisioningResult{AccountMap: tbResult.AccountIDs}, nil
}

// AccountingWorkflowID returns the stable workflow ID for a tenant's accounting workflow.
func AccountingWorkflowID(tenantID string) string {
	return fmt.Sprintf("tenant-accounting-%s", tenantID)
}

// ProvisioningWorkflowID returns the workflow ID for a one-time provisioning run.
func ProvisioningWorkflowID(tenantID string) string {
	return fmt.Sprintf("tenant-provisioning-%s", tenantID)
}

// ClusterRegistrationWorkflowID returns the workflow ID for a cluster registration.
func ClusterRegistrationWorkflowID(clusterID string) string {
	return fmt.Sprintf("cluster-registration-%s", clusterID)
}

// RegisterClusterWorkflowInput carries cluster registration data.
type RegisterClusterWorkflowInput struct {
	ClusterID     string
	TenantID      string
	CloudProvider string
	Region        string
}

// RegisterClusterWorkflow creates a cluster and signals the tenant accounting workflow.
func RegisterClusterWorkflow(ctx workflow.Context, input RegisterClusterWorkflowInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 5},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	// Insert cluster row in PG.
	if err := workflow.ExecuteActivity(ctx,
		pgActivities.InsertCluster,
		InsertClusterInput{
			ClusterID:     input.ClusterID,
			TenantID:      input.TenantID,
			CloudProvider: input.CloudProvider,
			Region:        input.Region,
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("InsertCluster: %w", err)
	}

	// Signal the tenant accounting workflow to initialise processedSeqs for this cluster.
	workflowID := AccountingWorkflowID(input.TenantID)
	return workflow.SignalExternalWorkflow(ctx, workflowID, "",
		SignalRegisterCluster,
		RegisterClusterSignal{ClusterID: input.ClusterID},
	).Get(ctx, nil)
}
