package accounting

import (
	"context"
	"fmt"

	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// TBActivities holds TigerBeetle-facing Temporal activities.
type TBActivities struct {
	ledger *ledger.Client

	// Global account IDs (operator + sink per resource).
	// Populated at server startup and passed in.
	globalAccounts map[ledger.GlobalAccountKey]interface{}
}

// TBAccountMap is a map of resource type to TB account IDs for one tenant.
type TBAccountMap struct {
	OperatorIDs    map[string][16]byte
	TenantQuotaIDs map[string][16]byte
	SinkIDs        map[string][16]byte
}

// TBBatchInput is the input to SubmitTBBatch.
type TBBatchInput struct {
	TenantID   string
	TenantUUID [16]byte
	AccountMap TBAccountMap
	Heartbeats []HeartbeatSignal
	// GlobalOperatorIDs maps resource_type string → 16-byte TB account ID
	GlobalOperatorIDs map[string][16]byte
	// GlobalSinkIDs maps resource_type string → 16-byte TB account ID
	GlobalSinkIDs map[string][16]byte
	// PendingGaugeIDs holds in-flight pending TB transfer IDs for gauge resources.
	// Maps clusterID → resource_type → 16-byte pending transfer ID.
	// Used to void the old pending before creating the new one.
	PendingGaugeIDs map[string]map[string][16]byte
	// NewGaugePendingIDs carries pre-generated deterministic IDs for the new pending
	// transfer per (clusterID, resource_type). Generated in the workflow before the
	// activity is scheduled so Temporal retries reuse the same IDs (I-5).
	NewGaugePendingIDs map[string]map[string][16]byte
	// GaugeVoidIDs carries pre-generated deterministic IDs for the void transfer
	// per (clusterID, resource_type). Same idempotency rationale as NewGaugePendingIDs.
	GaugeVoidIDs map[string]map[string][16]byte
}

// TBBatchResult is returned by SubmitTBBatch.
type TBBatchResult struct {
	// PerResource maps resource_type → per-transfer result.
	PerResource map[string]ResourceBatchResult
	// GaugeUpdates maps clusterID → resource_type → new pending transfer ID.
	// A zero [16]byte means the gauge was released (voided without a new pending).
	GaugeUpdates map[string]map[string][16]byte
}

// ResourceBatchResult captures the outcome for a single resource in a batch.
type ResourceBatchResult struct {
	Accepted       bool
	ExceedsCredits bool
	Exists         bool
}

// CreateTenantTBAccountsInput is input for CreateTenantTBAccounts.
type CreateTenantTBAccountsInput struct {
	TenantUUID [16]byte
}

// CreateTenantTBAccountsResult holds the generated TB account IDs.
type CreateTenantTBAccountsResult struct {
	// AccountIDs maps resource_type → 16-byte TB account ID.
	AccountIDs map[string][16]byte
}

// SubmitAllocationInput carries the data for initial quota allocation transfers.
type SubmitAllocationInput struct {
	TenantUUID        [16]byte
	TenantQuotaIDs    map[string][16]byte
	GlobalOperatorIDs map[string][16]byte
	Credits           map[string]int64
	PeriodStartNs     int64
	// TransferIDs carries pre-generated deterministic IDs keyed by resource_type.
	// Generated in the workflow before the activity is scheduled so Temporal retries
	// reuse the same IDs and TigerBeetle returns TransferExists instead of
	// creating a duplicate credit (I-5). If nil, RandomID() fallback is used.
	TransferIDs map[string][16]byte
}

func NewTBActivities(c *ledger.Client) *TBActivities {
	return &TBActivities{ledger: c}
}

// SubmitTBBatch builds and submits usage transfers for a batch of heartbeats.
//
// Cumulative resources (CPU, memory): one posted UsageTransfer per heartbeat.
// Transfer IDs are deterministic from (clusterID, seq, resource) — idempotent.
//
// Gauge resources (active_nodes): one pending reservation per cluster, updated to
// the latest value in the batch. The old pending is voided before the new one lands.
func (a *TBActivities) SubmitTBBatch(ctx context.Context, input TBBatchInput) (TBBatchResult, error) {
	result := TBBatchResult{
		PerResource:  make(map[string]ResourceBatchResult),
		GaugeUpdates: make(map[string]map[string][16]byte),
	}

	// --- Cumulative resources: posted transfers, one per (heartbeat, resource) ---
	var transfers []ledger.UsageTransfer
	var transferResources []string

	for _, hb := range input.Heartbeats {
		for _, r := range domain.AllResources {
			if r.IsGauge() {
				continue // handled separately below
			}
			amount := heartbeatAmount(hb, r)
			if amount == 0 {
				continue
			}
			operatorBytes, ok := input.GlobalOperatorIDs[string(r)]
			if !ok {
				continue
			}
			quotaBytes, ok := input.AccountMap.TenantQuotaIDs[string(r)]
			if !ok {
				continue
			}
			sinkBytes, ok := input.GlobalSinkIDs[string(r)]
			if !ok {
				continue
			}
			transfers = append(transfers, ledger.UsageTransfer{
				Resource:      r,
				OperatorID:    ledger.UUIDToUint128(operatorBytes),
				TenantQuotaID: ledger.UUIDToUint128(quotaBytes),
				SinkID:        ledger.UUIDToUint128(sinkBytes),
				Amount:        uint64(amount),
				TransferID:    ledger.DeriveTransferID(hb.ClusterUUID, hb.SequenceNumber, r.LedgerID()),
				ClusterUUID:   hb.ClusterUUID,
				HeartbeatTsNs: uint64(hb.HeartbeatTimestampNs),
			})
			transferResources = append(transferResources, string(r))
		}
	}

	if len(transfers) > 0 {
		tbResults, err := ledger.SubmitUsageBatch(a.ledger, transfers)
		if err != nil {
			return result, fmt.Errorf("SubmitUsageBatch: %w", err)
		}
		for i, res := range tbResults {
			result.PerResource[transferResources[i]] = ResourceBatchResult{
				Accepted:       res.Accepted,
				ExceedsCredits: res.ExceedsCredits,
				Exists:         res.Exists,
			}
		}
	}

	// --- Gauge resources: pending reservations, one per (cluster, resource) ---
	// Take the latest heartbeat per cluster (highest SequenceNumber) as the
	// authoritative current state for gauge values.
	latestByCluster := make(map[string]HeartbeatSignal)
	for _, hb := range input.Heartbeats {
		if prev, ok := latestByCluster[hb.ClusterID]; !ok || hb.SequenceNumber > prev.SequenceNumber {
			latestByCluster[hb.ClusterID] = hb
		}
	}

	type gaugeKey struct{ clusterID, resource string }
	var gaugeUpdates []ledger.GaugePendingUpdate
	var gaugeKeys []gaugeKey

	for clusterID, hb := range latestByCluster {
		for _, r := range domain.AllResources {
			if !r.IsGauge() {
				continue
			}
			quotaBytes, ok := input.AccountMap.TenantQuotaIDs[string(r)]
			if !ok {
				continue
			}
			sinkBytes, ok := input.GlobalSinkIDs[string(r)]
			if !ok {
				continue
			}

			var oldPendingID *[16]byte
			if clusterMap, ok := input.PendingGaugeIDs[clusterID]; ok {
				if pendingBytes, ok := clusterMap[string(r)]; ok {
					id := pendingBytes // copy to take address
					oldPendingID = &id
				}
			}

			gu := ledger.GaugePendingUpdate{
				Resource:      r,
				TenantQuotaID: ledger.UUIDToUint128(quotaBytes),
				SinkID:        ledger.UUIDToUint128(sinkBytes),
				OldPendingID:  oldPendingID,
				NewAmount:     uint64(heartbeatAmount(hb, r)),
				ClusterUUID:   hb.ClusterUUID,
				HeartbeatTsNs: uint64(hb.HeartbeatTimestampNs),
			}
			if input.NewGaugePendingIDs != nil {
				if clusterMap, ok := input.NewGaugePendingIDs[clusterID]; ok {
					if idBytes, ok := clusterMap[string(r)]; ok {
						gu.NewPendingID = ledger.UUIDToUint128(idBytes)
					}
				}
			}
			if input.GaugeVoidIDs != nil {
				if clusterMap, ok := input.GaugeVoidIDs[clusterID]; ok {
					if idBytes, ok := clusterMap[string(r)]; ok {
						gu.VoidTransferID = ledger.UUIDToUint128(idBytes)
					}
				}
			}
			gaugeUpdates = append(gaugeUpdates, gu)
			gaugeKeys = append(gaugeKeys, gaugeKey{clusterID, string(r)})
		}
	}

	if len(gaugeUpdates) > 0 {
		gaugeResults, err := ledger.SubmitGaugePendingUpdates(a.ledger, gaugeUpdates)
		if err != nil {
			return result, fmt.Errorf("SubmitGaugePendingUpdates: %w", err)
		}
		for i, gr := range gaugeResults {
			k := gaugeKeys[i]
			result.PerResource[k.resource] = ResourceBatchResult{
				Accepted:       gr.Accepted,
				ExceedsCredits: gr.ExceedsCredits,
			}
			// Record the new pending ID (zero bytes = released, no new pending).
			if result.GaugeUpdates[k.clusterID] == nil {
				result.GaugeUpdates[k.clusterID] = make(map[string][16]byte)
			}
			result.GaugeUpdates[k.clusterID][k.resource] = ledger.Uint128ToBytes(gr.NewPendingID)
		}
	}

	return result, nil
}

// CreateTenantTBAccounts creates quota accounts for a new tenant.
func (a *TBActivities) CreateTenantTBAccounts(ctx context.Context, input CreateTenantTBAccountsInput) (CreateTenantTBAccountsResult, error) {
	ids, err := ledger.CreateTenantQuotaAccounts(a.ledger, input.TenantUUID, nil)
	if err != nil {
		return CreateTenantTBAccountsResult{}, fmt.Errorf("CreateTenantQuotaAccounts: %w", err)
	}

	out := CreateTenantTBAccountsResult{AccountIDs: make(map[string][16]byte)}
	for r, id := range ids {
		out.AccountIDs[string(r)] = ledger.Uint128ToBytes(id)
	}
	return out, nil
}

// SubmitAllocationTransfers credits the tenant's quota wallets with initial plan amounts.
func (a *TBActivities) SubmitAllocationTransfers(ctx context.Context, input SubmitAllocationInput) error {
	var allocations []ledger.AllocationTransfer
	for r, credits := range input.Credits {
		resource := domain.ResourceType(r)
		opBytes, ok := input.GlobalOperatorIDs[r]
		if !ok {
			continue
		}
		quotaBytes, ok := input.TenantQuotaIDs[r]
		if !ok {
			continue
		}
		t := ledger.AllocationTransfer{
			Resource:      resource,
			OperatorID:    ledger.UUIDToUint128(opBytes),
			TenantQuotaID: ledger.UUIDToUint128(quotaBytes),
			Amount:        uint64(credits),
			Code:          domain.CodeQuotaAllocation,
			TenantUUID:    input.TenantUUID,
			PeriodStartNs: uint64(input.PeriodStartNs),
		}
		if input.TransferIDs != nil {
			if idBytes, ok := input.TransferIDs[r]; ok {
				t.TransferID = ledger.UUIDToUint128(idBytes)
			}
		}
		allocations = append(allocations, t)
	}

	if len(allocations) == 0 {
		return nil
	}

	_, err := ledger.SubmitAllocations(a.ledger, allocations)
	return err
}

// LookupAccountBalancesInput is the input for LookupAccountBalances.
type LookupAccountBalancesInput struct {
	// TenantQuotaIDs maps resource_type string → 16-byte TB account ID.
	TenantQuotaIDs map[string][16]byte
}

// LookupAccountBalancesResult maps resource_type → current balance snapshot from TB.
type LookupAccountBalancesResult struct {
	Balances map[string]QuotaSnapshotData
}

// LookupAccountBalances fetches live balances from TigerBeetle for the given accounts.
// This is used to populate the PG quota_snapshots projection after a flush (I-4: PG is
// a projection of TB state; this activity reads from the authoritative source).
func (a *TBActivities) LookupAccountBalances(ctx context.Context, input LookupAccountBalancesInput) (LookupAccountBalancesResult, error) {
	result := LookupAccountBalancesResult{Balances: make(map[string]QuotaSnapshotData)}

	balances, err := ledger.LookupBalances(a.ledger, input.TenantQuotaIDs)
	if err != nil {
		return result, fmt.Errorf("LookupBalances: %w", err)
	}

	for r, b := range balances {
		result.Balances[r] = QuotaSnapshotData{
			CreditsTotal:  int64(b.CreditsPosted),
			DebitsPosted:  int64(b.DebitsPosted),
			DebitsPending: int64(b.DebitsPending),
		}
	}
	return result, nil
}

// heartbeatAmount extracts the usage delta for a resource from a heartbeat signal.
func heartbeatAmount(hb HeartbeatSignal, r domain.ResourceType) int64 {
	switch r {
	case domain.ResourceCPUHours:
		return hb.CPUMillisecondsDelta
	case domain.ResourceMemoryGBHours:
		return hb.MemoryMBSecondsDelta
	case domain.ResourceActiveNodes:
		return int64(hb.ActiveNodes)
	default:
		return 0
	}
}
