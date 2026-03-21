package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	creditsystemv1 "github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1"
	"github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1/creditsystemv1connect"
	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// ProvisioningHandler implements the ConnectRPC ProvisioningService.
type ProvisioningHandler struct {
	creditsystemv1connect.UnimplementedProvisioningServiceHandler

	temporal       client.Client
	globalAccounts map[ledger.GlobalAccountKey][16]byte
	log            *slog.Logger
}

func NewProvisioningHandler(tc client.Client, globalAccounts map[ledger.GlobalAccountKey][16]byte) *ProvisioningHandler {
	return &ProvisioningHandler{
		temporal:       tc,
		globalAccounts: globalAccounts,
		log:            slog.Default().With("handler", "provisioning"),
	}
}

// defaultCredits returns the initial credit amounts for each resource based on billing tier.
func defaultCredits(tier string) map[string]int64 {
	switch tier {
	case "pro":
		return map[string]int64{
			string(domain.ResourceCPUHours):      1_000_000,
			string(domain.ResourceMemoryGBHours): 1_000_000,
			string(domain.ResourceActiveNodes):   20,
		}
	case "starter":
		return map[string]int64{
			string(domain.ResourceCPUHours):      200_000,
			string(domain.ResourceMemoryGBHours): 200_000,
			string(domain.ResourceActiveNodes):   5,
		}
	default: // free
		return map[string]int64{
			string(domain.ResourceCPUHours):      10_000,
			string(domain.ResourceMemoryGBHours): 10_000,
			string(domain.ResourceActiveNodes):   2,
		}
	}
}

// RegisterTenant kicks off TenantProvisioningWorkflow for a new tenant.
func (h *ProvisioningHandler) RegisterTenant(
	ctx context.Context,
	req *connect.Request[creditsystemv1.RegisterTenantRequest],
) (*connect.Response[creditsystemv1.RegisterTenantResponse], error) {
	r := req.Msg
	tenantID := uuid.New()

	credits := defaultCredits(r.BillingTier)
	if r.CpuHoursCredits > 0 {
		credits[string(domain.ResourceCPUHours)] = r.CpuHoursCredits
	}
	if r.MemoryGbHoursCredits > 0 {
		credits[string(domain.ResourceMemoryGBHours)] = r.MemoryGbHoursCredits
	}
	if r.ActiveNodesLimit > 0 {
		credits[string(domain.ResourceActiveNodes)] = r.ActiveNodesLimit
	}

	// Build global account maps for the workflow.
	opIDs := make(map[string][16]byte)
	sinkIDs := make(map[string][16]byte)
	for r := range domain.AllResources {
		res := domain.AllResources[r]
		opKey := ledger.GlobalAccountKey{Resource: res, AccountType: domain.AccountTypeOperator}
		sinkKey := ledger.GlobalAccountKey{Resource: res, AccountType: domain.AccountTypeSink}
		if id, ok := h.globalAccounts[opKey]; ok {
			opIDs[string(res)] = id
		}
		if id, ok := h.globalAccounts[sinkKey]; ok {
			sinkIDs[string(res)] = id
		}
	}

	input := accounting.TenantProvisioningInput{
		TenantID:          tenantID.String(),
		TenantUUID:        [16]byte(tenantID),
		Name:              r.Name,
		BillingTier:       r.BillingTier,
		Credits:           credits,
		GlobalOperatorIDs: opIDs,
		GlobalSinkIDs:     sinkIDs,
		PeriodStartNs:     time.Now().UnixNano(),
	}

	run, err := h.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        accounting.ProvisioningWorkflowID(tenantID.String()),
		TaskQueue: accounting.TaskQueueAccounting,
	}, accounting.TenantProvisioningWorkflow, input)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start provisioning workflow: %w", err))
	}

	// Wait for provisioning to complete so the accounting workflow is running before we return.
	// This prevents a race where RegisterCluster signals the accounting workflow before it exists.
	if err := run.Get(ctx, nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("provisioning workflow failed: %w", err))
	}

	h.log.Info("tenant provisioned", "tenantID", tenantID)
	return connect.NewResponse(&creditsystemv1.RegisterTenantResponse{TenantId: tenantID.String()}), nil
}

// RegisterCluster registers a cluster and signals the tenant accounting workflow.
func (h *ProvisioningHandler) RegisterCluster(
	ctx context.Context,
	req *connect.Request[creditsystemv1.RegisterClusterRequest],
) (*connect.Response[creditsystemv1.RegisterClusterResponse], error) {
	r := req.Msg
	clusterID := uuid.New()

	input := accounting.RegisterClusterWorkflowInput{
		ClusterID:     clusterID.String(),
		TenantID:      r.TenantId,
		CloudProvider: r.CloudProvider,
		Region:        r.Region,
	}

	run, err := h.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        accounting.ClusterRegistrationWorkflowID(clusterID.String()),
		TaskQueue: accounting.TaskQueueAccounting,
	}, accounting.RegisterClusterWorkflow, input)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start cluster workflow: %w", err))
	}

	// Wait for registration to complete: cluster must be in DB and accounting workflow
	// must have its processedSeqs entry before the first heartbeat can arrive.
	if err := run.Get(ctx, nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("cluster registration failed: %w", err))
	}

	h.log.Info("cluster registered", "clusterID", clusterID, "tenantID", r.TenantId)
	return connect.NewResponse(&creditsystemv1.RegisterClusterResponse{ClusterId: clusterID.String()}), nil
}
