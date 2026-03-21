package accounting

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/cshubhamrao/cloud-credit-system/internal/db/sqlcgen"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// PGActivities holds PostgreSQL-facing Temporal activities.
type PGActivities struct {
	db *sql.DB
}

func NewPGActivities(pool *pgxpool.Pool) *PGActivities {
	return &PGActivities{db: stdlib.OpenDBFromPool(pool)}
}

// InsertTenantInput carries data for inserting a new tenant row.
type InsertTenantInput struct {
	TenantID    string
	Name        string
	BillingTier string
}

// InsertTenant persists a new tenant to PostgreSQL.
func (a *PGActivities) InsertTenant(ctx context.Context, input InsertTenantInput) error {
	q := sqlcgen.New(a.db)
	tenantUUID, err := uuid.Parse(input.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant UUID: %w", err)
	}
	_, err = q.InsertTenant(ctx, tenantUUID, input.Name, input.BillingTier)
	return err
}

// InsertClusterInput carries data for inserting a new cluster row.
type InsertClusterInput struct {
	ClusterID     string
	TenantID      string
	CloudProvider string
	Region        string
}

// InsertCluster persists a new workload cluster to PostgreSQL.
func (a *PGActivities) InsertCluster(ctx context.Context, input InsertClusterInput) error {
	q := sqlcgen.New(a.db)
	clusterUUID, err := uuid.Parse(input.ClusterID)
	if err != nil {
		return fmt.Errorf("parse cluster UUID: %w", err)
	}
	tenantUUID, err := uuid.Parse(input.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant UUID: %w", err)
	}
	_, err = q.InsertCluster(ctx, clusterUUID, tenantUUID, input.CloudProvider, input.Region)
	return err
}

// InsertTBAccountMappingInput holds TB account IDs to persist.
type InsertTBAccountMappingInput struct {
	TenantID   string
	AccountMap map[string][16]byte            // resource_type → 16-byte TB account ID
	GlobalMap  map[string]map[string][16]byte // resource_type → account_type → ID
}

// InsertTBAccountMapping saves the TB account ID mapping to PostgreSQL.
func (a *PGActivities) InsertTBAccountMapping(ctx context.Context, input InsertTBAccountMappingInput) error {
	q := sqlcgen.New(a.db)
	tenantUUID, err := uuid.Parse(input.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant UUID: %w", err)
	}

	for resourceType, idBytes := range input.AccountMap {
		idCopy := idBytes
		err = q.InsertTBAccountMapping(ctx, tenantUUID, resourceType, "tenant_quota", idCopy[:])
		if err != nil {
			return fmt.Errorf("InsertTBAccountMapping %s: %w", resourceType, err)
		}
	}
	return nil
}

// UpdateQuotaSnapshotsInput carries TB balance data to persist in quota_snapshots.
type UpdateQuotaSnapshotsInput struct {
	TenantID     string
	TenantUUID   [16]byte
	AccountMap   map[string][16]byte // resource_type → TB account ID bytes
	LedgerClient *ledger.Client      // NOTE: activities should not hold long-lived references;
	// pass the snapshot data directly instead in production.
	Snapshots map[string]QuotaSnapshotData
}

// QuotaSnapshotData holds a pre-fetched TB balance snapshot.
type QuotaSnapshotData struct {
	CreditsTotal  int64
	DebitsPosted  int64
	DebitsPending int64
}

// UpdateQuotaSnapshots writes the latest TB balances into PostgreSQL quota_snapshots.
// This is the idempotent projection: if re-run, it overwrites with the same data.
func (a *PGActivities) UpdateQuotaSnapshots(ctx context.Context, input UpdateQuotaSnapshotsInput) error {
	q := sqlcgen.New(a.db)
	tenantUUID, err := uuid.Parse(input.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant UUID: %w", err)
	}

	for resourceType, snap := range input.Snapshots {
		err = q.UpsertQuotaSnapshot(ctx, tenantUUID, resourceType, snap.CreditsTotal, snap.DebitsPosted, snap.DebitsPending)
		if err != nil {
			return fmt.Errorf("UpsertQuotaSnapshot %s: %w", resourceType, err)
		}
	}
	return nil
}

// InsertCreditAdjustmentInput records a credit adjustment for audit.
type InsertCreditAdjustmentInput struct {
	TenantID     string
	ResourceType string
	Amount       int64
	Reason       string
	TransferID   [16]byte
}

// InsertCreditAdjustment persists a credit adjustment audit record.
func (a *PGActivities) InsertCreditAdjustment(ctx context.Context, input InsertCreditAdjustmentInput) error {
	q := sqlcgen.New(a.db)
	tenantUUID, err := uuid.Parse(input.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant UUID: %w", err)
	}
	idCopy := input.TransferID
	return q.InsertCreditAdjustment(ctx, tenantUUID, input.ResourceType, input.Amount,
		sql.NullString{String: input.Reason, Valid: input.Reason != ""},
		idCopy[:],
	)
}
