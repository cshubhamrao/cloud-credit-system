package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"go.temporal.io/sdk/client"

	creditsystemv1 "github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1"
	"github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1/creditsystemv1connect"
	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/db/sqlcgen"
	"github.com/cshubhamrao/cloud-credit-system/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)


// AdminHandler implements the ConnectRPC AdminService.
type AdminHandler struct {
	creditsystemv1connect.UnimplementedAdminServiceHandler

	temporal client.Client
	db       *sql.DB
	log      *slog.Logger
}

func NewAdminHandler(tc client.Client, pool *pgxpool.Pool) *AdminHandler {
	return &AdminHandler{
		temporal: tc,
		db:       stdlib.OpenDBFromPool(pool),
		log:      slog.Default().With("handler", "admin"),
	}
}

// IssueTenantCredit issues a quota credit via a Temporal Update.
// The RPC blocks until TigerBeetle confirms the allocation — the response
// includes the live post-adjustment balances for all resources.
func (h *AdminHandler) IssueTenantCredit(
	ctx context.Context,
	req *connect.Request[creditsystemv1.IssueTenantCreditRequest],
) (*connect.Response[creditsystemv1.IssueTenantCreditResponse], error) {
	r := req.Msg
	if r.Amount <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("amount must be positive"))
	}

	sig := accounting.QuotaAdjustmentSignal{
		ResourceType: r.ResourceType,
		Amount:       r.Amount,
		Reason:       r.Reason,
		Code:         uint16(domain.CodeQuotaAdjustment),
	}

	workflowID := accounting.AccountingWorkflowID(r.TenantId)
	handle, err := h.temporal.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   workflowID,
		UpdateName:   accounting.UpdateIssueCredit,
		Args:         []interface{}{sig},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update workflow: %w", err))
	}

	var result accounting.IssueCreditResult
	if err := handle.Get(ctx, &result); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update result: %w", err))
	}

	h.log.Info("credit issued", "tenant", r.TenantId, "resource", r.ResourceType, "amount", r.Amount)

	quotas := make([]*creditsystemv1.QuotaInfo, 0, len(result.Balances))
	for resourceType, snap := range result.Balances {
		available := snap.CreditsTotal - snap.DebitsPosted - snap.DebitsPending
		used := snap.DebitsPosted
		if domain.ResourceType(resourceType).IsGauge() {
			used = snap.DebitsPending
		}
		status := creditsystemv1.Status_STATUS_OK
		if available <= 0 {
			status = creditsystemv1.Status_STATUS_QUOTA_EXCEEDED
		} else if snap.CreditsTotal > 0 && float64(used)/float64(snap.CreditsTotal) >= 0.8 {
			status = creditsystemv1.Status_STATUS_QUOTA_WARNING
		}
		quotas = append(quotas, &creditsystemv1.QuotaInfo{
			ResourceType: resourceType,
			Used:         used,
			Limit:        snap.CreditsTotal,
			Available:    available,
			Status:       status,
		})
	}
	return connect.NewResponse(&creditsystemv1.IssueTenantCreditResponse{Quotas: quotas}), nil
}

// ListTenantQuotas reads quota_snapshots from PostgreSQL for the given tenant.
func (h *AdminHandler) ListTenantQuotas(
	ctx context.Context,
	req *connect.Request[creditsystemv1.ListTenantQuotasRequest],
) (*connect.Response[creditsystemv1.ListTenantQuotasResponse], error) {
	tenantUUID, err := uuid.Parse(req.Msg.TenantId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant ID: %w", err))
	}

	q := sqlcgen.New(h.db)
	snapshots, err := q.GetQuotaSnapshots(ctx, tenantUUID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("GetQuotaSnapshots: %w", err))
	}

	quotas := make([]*creditsystemv1.QuotaInfo, 0, len(snapshots))
	for _, s := range snapshots {
		available := s.Available.Int64
		used := s.DebitsPosted
		if domain.ResourceType(s.ResourceType).IsGauge() {
			used = s.DebitsPending
		}
		status := creditsystemv1.Status_STATUS_OK
		if !s.Available.Valid || available <= 0 {
			status = creditsystemv1.Status_STATUS_QUOTA_EXCEEDED
		} else if s.CreditsTotal > 0 && float64(used)/float64(s.CreditsTotal) >= 0.8 {
			status = creditsystemv1.Status_STATUS_QUOTA_WARNING
		}
		quotas = append(quotas, &creditsystemv1.QuotaInfo{
			ResourceType: s.ResourceType,
			Used:         used,
			Limit:        s.CreditsTotal,
			Available:    available,
			Status:       status,
		})
	}

	return connect.NewResponse(&creditsystemv1.ListTenantQuotasResponse{Quotas: quotas}), nil
}

// ListTenants returns all active (non-deleted) tenants for dashboard display.
func (h *AdminHandler) ListTenants(
	ctx context.Context,
	_ *connect.Request[creditsystemv1.ListTenantsRequest],
) (*connect.Response[creditsystemv1.ListTenantsResponse], error) {
	q := sqlcgen.New(h.db)
	rows, err := q.ListTenants(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ListTenants: %w", err))
	}

	tenants := make([]*creditsystemv1.TenantSummary, 0, len(rows))
	for _, t := range rows {
		tenants = append(tenants, &creditsystemv1.TenantSummary{
			Id:          t.ID.String(),
			Name:        t.Name,
			BillingTier: t.BillingTier,
			Status:      t.Status,
			CreatedAt:   t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return connect.NewResponse(&creditsystemv1.ListTenantsResponse{Tenants: tenants}), nil
}

// ListCreditAdjustments returns the credit adjustment audit trail for a tenant.
func (h *AdminHandler) ListCreditAdjustments(
	ctx context.Context,
	req *connect.Request[creditsystemv1.ListCreditAdjustmentsRequest],
) (*connect.Response[creditsystemv1.ListCreditAdjustmentsResponse], error) {
	tenantUUID, err := uuid.Parse(req.Msg.TenantId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant ID: %w", err))
	}

	q := sqlcgen.New(h.db)
	rows, err := q.ListCreditAdjustmentsByTenant(ctx, tenantUUID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ListCreditAdjustmentsByTenant: %w", err))
	}

	entries := make([]*creditsystemv1.CreditAdjustmentEntry, 0, len(rows))
	for _, r := range rows {
		entry := &creditsystemv1.CreditAdjustmentEntry{
			Id:           r.ID.String(),
			ResourceType: r.ResourceType,
			Amount:       r.Amount,
			CreatedAt:    r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if r.Reason.Valid {
			entry.Reason = r.Reason.String
		}
		if len(r.TbTransferID) == 16 {
			entry.TransferId = fmt.Sprintf("%x", r.TbTransferID)
		}
		entries = append(entries, entry)
	}
	return connect.NewResponse(&creditsystemv1.ListCreditAdjustmentsResponse{Adjustments: entries}), nil
}

// ListClusters returns active workload clusters for a tenant.
func (h *AdminHandler) ListClusters(
	ctx context.Context,
	req *connect.Request[creditsystemv1.ListClustersRequest],
) (*connect.Response[creditsystemv1.ListClustersResponse], error) {
	tenantUUID, err := uuid.Parse(req.Msg.TenantId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant ID: %w", err))
	}

	q := sqlcgen.New(h.db)
	rows, err := q.ListClustersByTenant(ctx, tenantUUID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ListClustersByTenant: %w", err))
	}

	clusters := make([]*creditsystemv1.ClusterSummary, 0, len(rows))
	for _, c := range rows {
		cs := &creditsystemv1.ClusterSummary{
			Id:            c.ID.String(),
			TenantId:      c.TenantID.String(),
			CloudProvider: c.CloudProvider,
			Region:        c.Region,
			Status:        c.Status,
		}
		if c.LastHeartbeat.Valid {
			cs.LastHeartbeat = c.LastHeartbeat.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		if c.AgentVersion.Valid {
			cs.AgentVersion = c.AgentVersion.String
		}
		clusters = append(clusters, cs)
	}
	return connect.NewResponse(&creditsystemv1.ListClustersResponse{Clusters: clusters}), nil
}
