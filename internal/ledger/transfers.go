package ledger

import (
	"fmt"

	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
)

// TransferResult captures the per-resource outcome of a batch submission.
type TransferResult struct {
	Resource domain.ResourceType
	// Accepted is true when TigerBeetle committed the transfer.
	Accepted bool
	// ExceedsCredits is true when the hard limit was hit.
	ExceedsCredits bool
	// Exists is true when the transfer ID already existed (idempotent duplicate).
	Exists bool
}

// UsageTransfer describes a single resource usage record to submit.
type UsageTransfer struct {
	Resource        domain.ResourceType
	OperatorID      types.Uint128
	TenantQuotaID   types.Uint128
	SinkID          types.Uint128
	Amount          uint64
	TransferID      types.Uint128 // deterministic ID for idempotency
	ClusterUUID     [16]byte      // stored in user_data_128
	HeartbeatTsNs   uint64        // stored in user_data_64
}

// AllocationTransfer describes a quota credit (plan allocation or adjustment).
type AllocationTransfer struct {
	Resource      domain.ResourceType
	OperatorID    types.Uint128
	TenantQuotaID types.Uint128
	Amount        uint64
	Code          domain.TransferCode
	TenantUUID    [16]byte
	PeriodStartNs uint64
}

// SubmitUsageBatch submits a batch of usage transfers and returns per-transfer results.
// Transfers that fail with ExceedsCredits are reported individually; the batch
// continues for remaining resources (independent transfers, not linked).
func SubmitUsageBatch(c *Client, transfers []UsageTransfer) ([]TransferResult, error) {
	tbTransfers := make([]types.Transfer, len(transfers))
	for i, t := range transfers {
		tbTransfers[i] = types.Transfer{
			ID:              t.TransferID,
			DebitAccountID:  t.TenantQuotaID,
			CreditAccountID: t.SinkID,
			Amount:          types.ToUint128(t.Amount),
			Ledger:          t.Resource.LedgerID(),
			Code:            uint16(domain.CodeUsageRecord),
			UserData128:     UUIDToUint128(t.ClusterUUID),
			UserData64:      t.HeartbeatTsNs,
		}
	}

	return submitBatch(c, tbTransfers, func(i int) domain.ResourceType {
		return transfers[i].Resource
	})
}

// SubmitAllocations submits quota allocation or adjustment transfers.
func SubmitAllocations(c *Client, allocations []AllocationTransfer) ([]TransferResult, error) {
	tbTransfers := make([]types.Transfer, len(allocations))
	for i, a := range allocations {
		tbTransfers[i] = types.Transfer{
			ID:              RandomID(),
			DebitAccountID:  a.OperatorID,
			CreditAccountID: a.TenantQuotaID,
			Amount:          types.ToUint128(a.Amount),
			Ledger:          a.Resource.LedgerID(),
			Code:            uint16(a.Code),
			UserData128:     UUIDToUint128(a.TenantUUID),
			UserData64:      a.PeriodStartNs,
		}
	}

	return submitBatch(c, tbTransfers, func(i int) domain.ResourceType {
		return allocations[i].Resource
	})
}

// GaugePendingUpdate describes a state transition for one gauge resource on one cluster.
// The caller voids the old pending (if any) and creates a new one in a single TB batch.
type GaugePendingUpdate struct {
	Resource      domain.ResourceType
	TenantQuotaID types.Uint128
	SinkID        types.Uint128
	// OldPendingID is non-nil when there is an existing pending transfer to void first.
	// Stored as a 16-byte array so callers don't need to import types.Uint128 directly.
	OldPendingID *[16]byte
	// NewAmount is the new gauge value. 0 = resource released, no new pending created.
	NewAmount     uint64
	ClusterUUID   [16]byte
	HeartbeatTsNs uint64
}

// GaugePendingResult is the per-update outcome of SubmitGaugePendingUpdates.
type GaugePendingResult struct {
	// NewPendingID is the ID of the newly created pending transfer; zero if NewAmount == 0.
	NewPendingID   types.Uint128
	Accepted       bool
	ExceedsCredits bool
}

// SubmitGaugePendingUpdates voids old pending gauge transfers and creates new ones.
// Void and new-pending for the same update are submitted in one batch so TB processes
// them in order — the void releases capacity before the new pending reserves it.
func SubmitGaugePendingUpdates(c *Client, updates []GaugePendingUpdate) ([]GaugePendingResult, error) {
	if len(updates) == 0 {
		return nil, nil
	}

	results := make([]GaugePendingResult, len(updates))
	for i := range results {
		results[i].Accepted = true // assume success; mark failures below
	}

	type meta struct {
		updateIdx int
		isVoid    bool
	}

	var tbTransfers []types.Transfer
	var metas []meta
	newPendingIDs := make([]types.Uint128, len(updates))
	for i := range updates {
		newPendingIDs[i] = RandomID()
	}

	for i, u := range updates {
		// Void old pending first so its capacity is released before the new one lands.
		if u.OldPendingID != nil {
			tbTransfers = append(tbTransfers, types.Transfer{
				ID:              RandomID(),
				PendingID:       UUIDToUint128(*u.OldPendingID),
				DebitAccountID:  u.TenantQuotaID,
				CreditAccountID: u.SinkID,
				Amount:          types.ToUint128(0), // 0 = void full pending amount
				Ledger:          u.Resource.LedgerID(),
				Code:            uint16(domain.CodeGaugeReservation),
				Flags:           types.TransferFlags{VoidPendingTransfer: true}.ToUint16(),
			})
			metas = append(metas, meta{updateIdx: i, isVoid: true})
		}
		// Create new pending reservation if the gauge is still non-zero.
		if u.NewAmount > 0 {
			tbTransfers = append(tbTransfers, types.Transfer{
				ID:              newPendingIDs[i],
				DebitAccountID:  u.TenantQuotaID,
				CreditAccountID: u.SinkID,
				Amount:          types.ToUint128(u.NewAmount),
				Ledger:          u.Resource.LedgerID(),
				Code:            uint16(domain.CodeGaugeReservation),
				Flags:           types.TransferFlags{Pending: true}.ToUint16(),
				UserData128:     UUIDToUint128(u.ClusterUUID),
				UserData64:      u.HeartbeatTsNs,
			})
			metas = append(metas, meta{updateIdx: i, isVoid: false})
		}
	}

	if len(tbTransfers) == 0 {
		return results, nil
	}

	tbResults, err := c.tb.CreateTransfers(tbTransfers)
	if err != nil {
		return nil, fmt.Errorf("CreateTransfers (gauge pending): %w", err)
	}

	for _, r := range tbResults {
		m := metas[r.Index]
		switch r.Result {
		case types.TransferExceedsCredits:
			if !m.isVoid {
				results[m.updateIdx].Accepted = false
				results[m.updateIdx].ExceedsCredits = true
			}
		case types.TransferExists,
			types.TransferExistsWithDifferentDebitAccountID,
			types.TransferExistsWithDifferentCreditAccountID,
			types.TransferExistsWithDifferentAmount:
			// Idempotent duplicate — treat as accepted.
		case types.TransferPendingTransferNotFound,
			types.TransferPendingTransferAlreadyVoided,
			types.TransferPendingTransferAlreadyPosted:
			// Old pending is already gone — not an error; new pending may still land.
			if !m.isVoid {
				return nil, fmt.Errorf("gauge transfer[%d]: unexpected result on pending create: %v", r.Index, r.Result)
			}
		default:
			return nil, fmt.Errorf("gauge transfer[%d]: unexpected result %v", r.Index, r.Result)
		}
	}

	// Assign new pending IDs for updates that created a new pending and were accepted.
	for _, m := range metas {
		if !m.isVoid && results[m.updateIdx].Accepted {
			results[m.updateIdx].NewPendingID = newPendingIDs[m.updateIdx]
		}
	}

	return results, nil
}

// submitBatch is the common transfer submission path with per-result classification.
func submitBatch(c *Client, transfers []types.Transfer, resourceAt func(int) domain.ResourceType) ([]TransferResult, error) {
	results := make([]TransferResult, len(transfers))
	for i, r := range transfers {
		results[i].Resource = resourceAt(i)
		_ = r
	}

	tbResults, err := c.tb.CreateTransfers(transfers)
	if err != nil {
		return nil, fmt.Errorf("CreateTransfers: %w", err)
	}

	// tbResults only contains entries for failed transfers.
	// Build a set of failed indices first.
	failed := make(map[uint32]types.CreateTransferResult)
	for _, r := range tbResults {
		failed[r.Index] = r.Result
	}

	for i := range results {
		tbResult, hadError := failed[uint32(i)]
		if !hadError {
			results[i].Accepted = true
			continue
		}
		switch tbResult {
		case types.TransferExceedsCredits:
			results[i].ExceedsCredits = true
		case types.TransferExists, types.TransferExistsWithDifferentDebitAccountID,
			types.TransferExistsWithDifferentCreditAccountID, types.TransferExistsWithDifferentAmount:
			// Treat all "exists" variants as idempotent — the transfer was already committed.
			results[i].Exists = true
			results[i].Accepted = true
		default:
			return nil, fmt.Errorf("transfer[%d] for %s: unexpected result %v", i, results[i].Resource, tbResult)
		}
	}

	return results, nil
}
