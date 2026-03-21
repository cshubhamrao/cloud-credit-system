package accounting

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// baseFlushInterval is the flush cadence after an active batch.
	baseFlushInterval = 2 * time.Second
	// maxFlushInterval is the cap when the workflow is idle (no heartbeats).
	// The interval doubles each idle cycle: 2s → 4s → 8s → … → 60s.
	maxFlushInterval = 60 * time.Second
	// batchSizeThreshold triggers an immediate flush at this batch size.
	// At 100 msg/s, 20 was too low (flush every ~200ms). 150 gives ~1.5s batches.
	batchSizeThreshold  = 150
	TaskQueueAccounting = "tenant-accounting"
)

// TenantAccountingState is the durable in-workflow state.
// All fields must be JSON-serialisable (Temporal checkpoints the workflow state).
type TenantAccountingState struct {
	TenantID   string
	TenantUUID [16]byte

	// processedSeqs tracks the highest sequence number processed per cluster.
	// Invariant I-2: one heartbeat sequence processed at most once.
	ProcessedSeqs map[string]uint64

	// AccountMap maps resource_type → 16-byte TB account ID for this tenant.
	AccountMap map[string][16]byte

	// GlobalOperatorIDs / GlobalSinkIDs are passed in at workflow start.
	GlobalOperatorIDs map[string][16]byte
	GlobalSinkIDs     map[string][16]byte

	// curBatch accumulates heartbeats between flushes.
	CurBatch []HeartbeatSignal

	// lastAck is the highest sequence number whose TB transfer has been committed.
	LastAck uint64

	// CurrentFlushInterval is the adaptive timer duration for the next flush.
	// Shrinks to baseFlushInterval after an active batch; doubles (up to
	// maxFlushInterval) after an idle cycle with no heartbeats.
	CurrentFlushInterval time.Duration

	// PendingGaugeIDs tracks live pending TB transfer IDs for gauge resources.
	// Maps clusterID → resource_type → 16-byte TB transfer ID.
	// These represent currently allocated gauge slots (e.g. active_nodes) and count
	// against debits_pending in TigerBeetle, enforcing the hard quota atomically.
	PendingGaugeIDs map[string]map[string][16]byte

	// ExceededResources tracks resource types where the last flush had at least
	// one transfer rejected by TigerBeetle with ExceedsCredits. This happens when
	// the remaining balance is smaller than any single heartbeat delta — the account
	// has "crumbs" that are arithmetically unreachable. Cleared on credit issuance.
	ExceededResources map[string]bool
}

// TenantAccountingInput is the workflow start parameter.
// On Continue-As-New the Carry* fields carry live state into the fresh run.
type TenantAccountingInput struct {
	TenantID          string
	TenantUUID        [16]byte
	AccountMap        map[string][16]byte
	GlobalOperatorIDs map[string][16]byte
	GlobalSinkIDs     map[string][16]byte
	// Carry* fields are zero on first start; populated on Continue-As-New.
	CarryProcessedSeqs    map[string]uint64
	CarryPendingGaugeIDs  map[string]map[string][16]byte
	CarryExceededResources map[string]bool
	CarryLastAck          uint64
}

// TenantAccountingWorkflow is the core long-running workflow.
// Invariant I-1: there is exactly one instance of this workflow per tenant.
// All TigerBeetle writes for a tenant pass through this workflow.
func TenantAccountingWorkflow(ctx workflow.Context, input TenantAccountingInput) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("TenantAccountingWorkflow started", "tenantID", input.TenantID)

	// Restore carried state on Continue-As-New; initialise fresh on first start.
	processedSeqs := input.CarryProcessedSeqs
	if processedSeqs == nil {
		processedSeqs = make(map[string]uint64)
	}
	pendingGaugeIDs := input.CarryPendingGaugeIDs
	if pendingGaugeIDs == nil {
		pendingGaugeIDs = make(map[string]map[string][16]byte)
	}
	exceededResources := input.CarryExceededResources
	if exceededResources == nil {
		exceededResources = make(map[string]bool)
	}

	state := &TenantAccountingState{
		TenantID:             input.TenantID,
		TenantUUID:           input.TenantUUID,
		AccountMap:           input.AccountMap,
		GlobalOperatorIDs:    input.GlobalOperatorIDs,
		GlobalSinkIDs:        input.GlobalSinkIDs,
		ProcessedSeqs:        processedSeqs,
		PendingGaugeIDs:      pendingGaugeIDs,
		ExceededResources:    exceededResources,
		LastAck:              input.CarryLastAck,
		CurrentFlushInterval: baseFlushInterval,
	}

	// Register query handler — gateway reads this to fill HeartbeatResponse.ack_sequence.
	if err := workflow.SetQueryHandler(ctx, QueryLastTBAck, func() (uint64, error) {
		return state.LastAck, nil
	}); err != nil {
		return fmt.Errorf("SetQueryHandler: %w", err)
	}

	// Register query handler — gateway reads this to set QUOTA_EXCEEDED status.
	if err := workflow.SetQueryHandler(ctx, QueryExceededResources, func() (map[string]bool, error) {
		return state.ExceededResources, nil
	}); err != nil {
		return fmt.Errorf("SetQueryHandler (exceeded): %w", err)
	}

	// Signal channels
	hbCh := workflow.GetSignalChannel(ctx, SignalHeartbeat)
	regCh := workflow.GetSignalChannel(ctx, SignalRegisterCluster)

	// The flush timer lives OUTSIDE the loop so heartbeat signals don't reset it.
	// It is only recreated after a flush (timer fires, credit update, or batch threshold).
	var timerCtx workflow.Context
	var cancelTimer workflow.CancelFunc
	var timerFuture workflow.Future

	resetTimer := func() {
		timerCtx, cancelTimer = workflow.WithCancel(ctx)
		timerFuture = workflow.NewTimer(timerCtx, state.CurrentFlushInterval)
	}
	resetTimer()

	// Register Update handler for credit issuance (IssueTenantCredit RPC).
	// Unlike signals, the caller blocks until the TB allocation is confirmed and
	// the new balance is returned — giving a real ack for financial operations.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateIssueCredit,
		func(ctx workflow.Context, sig QuotaAdjustmentSignal) (IssueCreditResult, error) {
			cancelTimer()
			result, err := flushAdjustment(ctx, state, sig)
			state.CurrentFlushInterval = baseFlushInterval
			resetTimer()
			return result, err
		},
		workflow.UpdateHandlerOptions{
			Validator: func(ctx workflow.Context, sig QuotaAdjustmentSignal) error {
				if sig.Amount <= 0 {
					return fmt.Errorf("amount must be positive")
				}
				if _, ok := state.AccountMap[sig.ResourceType]; !ok {
					return fmt.Errorf("unknown resource type: %s", sig.ResourceType)
				}
				return nil
			},
		},
	); err != nil {
		return fmt.Errorf("SetUpdateHandler: %w", err)
	}

	for {
		sel := workflow.NewSelector(ctx)

		// Heartbeat signal — accumulate into batch, do NOT touch the timer.
		// Exception: if the batch hits batchSizeThreshold, flush immediately to
		// bound memory usage during bursts and reset the interval to base.
		sel.AddReceive(hbCh, func(c workflow.ReceiveChannel, more bool) {
			var sig HeartbeatSignal
			c.Receive(ctx, &sig)

			// Dedup: skip if sequence already processed for this cluster.
			if prev, seen := state.ProcessedSeqs[sig.ClusterID]; seen && sig.SequenceNumber <= prev {
				logger.Info("dedup: skipping heartbeat", "cluster", sig.ClusterID, "seq", sig.SequenceNumber)
				return
			}
			state.CurBatch = append(state.CurBatch, sig)

			if len(state.CurBatch) >= batchSizeThreshold {
				cancelTimer()
				if err := flushBatch(ctx, state); err != nil {
					logger.Error("flushBatch error (threshold)", "error", err)
				}
				state.CurrentFlushInterval = baseFlushInterval
				resetTimer()
			}
		})

		// Register cluster signal — initialise processedSeqs entry.
		sel.AddReceive(regCh, func(c workflow.ReceiveChannel, more bool) {
			var sig RegisterClusterSignal
			c.Receive(ctx, &sig)
			if _, exists := state.ProcessedSeqs[sig.ClusterID]; !exists {
				state.ProcessedSeqs[sig.ClusterID] = 0
			}
		})

		// Flush timer fires — flush batch and adapt the next interval.
		// Active cycle (had heartbeats): reset to base interval.
		// Idle cycle (nothing to flush): double the interval up to max.
		sel.AddFuture(timerFuture, func(f workflow.Future) {
			hadActivity := len(state.CurBatch) > 0
			if err := flushBatch(ctx, state); err != nil {
				logger.Error("flushBatch error", "error", err)
			}
			if hadActivity {
				state.CurrentFlushInterval = baseFlushInterval
			} else {
				next := state.CurrentFlushInterval * 2
				if next > maxFlushInterval {
					next = maxFlushInterval
				}
				state.CurrentFlushInterval = next
			}
			resetTimer()
		})

		sel.Select(ctx)

		// Check for cancellation (for testing purposes).
		if ctx.Err() != nil {
			cancelTimer()
			return ctx.Err()
		}

		// Continue-As-New when the SDK signals that the Event History is
		// approaching the server-configured limit. We flush first so no
		// heartbeats are lost, then carry all live state into the fresh run.
		// Per SDK docs: return NewContinueAsNewError from the main loop,
		// not from within a signal/update handler, so all pending handlers
		// complete before the continuation.
		if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			logger.Info("continuing-as-new",
				"historyLen", workflow.GetInfo(ctx).GetCurrentHistoryLength())
			cancelTimer()
			if err := flushBatch(ctx, state); err != nil {
				logger.Error("flushBatch before continue-as-new", "error", err)
			}
			return workflow.NewContinueAsNewError(ctx, TenantAccountingWorkflow, TenantAccountingInput{
				TenantID:               state.TenantID,
				TenantUUID:             state.TenantUUID,
				AccountMap:             state.AccountMap,
				GlobalOperatorIDs:      state.GlobalOperatorIDs,
				GlobalSinkIDs:          state.GlobalSinkIDs,
				CarryProcessedSeqs:     state.ProcessedSeqs,
				CarryPendingGaugeIDs:   state.PendingGaugeIDs,
				CarryExceededResources: state.ExceededResources,
				CarryLastAck:           state.LastAck,
			})
		}
	}
}

// flushBatch submits the current heartbeat batch to TigerBeetle and updates PG snapshots.
func flushBatch(ctx workflow.Context, state *TenantAccountingState) error {
	if len(state.CurBatch) == 0 {
		return nil
	}

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 5,
			InitialInterval: time.Second,
		},
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	input := TBBatchInput{
		TenantID:          state.TenantID,
		TenantUUID:        state.TenantUUID,
		AccountMap:        TBAccountMap{TenantQuotaIDs: state.AccountMap},
		Heartbeats:        state.CurBatch,
		GlobalOperatorIDs: state.GlobalOperatorIDs,
		GlobalSinkIDs:     state.GlobalSinkIDs,
		PendingGaugeIDs:   state.PendingGaugeIDs,
	}

	var result TBBatchResult
	if err := workflow.ExecuteActivity(actCtx, tbActivities.SubmitTBBatch, input).Get(ctx, &result); err != nil {
		return fmt.Errorf("SubmitTBBatch: %w", err)
	}

	// Update ExceededResources from batch results.
	// A resource is exceeded when any transfer was rejected by TB due to quota exhaustion.
	// It is cleared only when a subsequent transfer succeeds (balance was restored externally).
	for resourceType, rr := range result.PerResource {
		if rr.ExceedsCredits {
			state.ExceededResources[resourceType] = true
		} else if rr.Accepted {
			delete(state.ExceededResources, resourceType)
		}
	}

	// Apply gauge pending ID updates from the activity result.
	// A zero [16]byte means the gauge was released (pending voided, no replacement).
	for clusterID, resources := range result.GaugeUpdates {
		if state.PendingGaugeIDs[clusterID] == nil {
			state.PendingGaugeIDs[clusterID] = make(map[string][16]byte)
		}
		for resource, id := range resources {
			if id == ([16]byte{}) {
				delete(state.PendingGaugeIDs[clusterID], resource)
			} else {
				state.PendingGaugeIDs[clusterID][resource] = id
			}
		}
	}

	// Update processedSeqs for all heartbeats in the batch.
	// A heartbeat is "processed" once the batch is submitted to TB, regardless of
	// whether individual transfers were accepted or exceeded credits (I-2).
	for _, hb := range state.CurBatch {
		if prev, seen := state.ProcessedSeqs[hb.ClusterID]; !seen || hb.SequenceNumber > prev {
			state.ProcessedSeqs[hb.ClusterID] = hb.SequenceNumber
		}
		if hb.SequenceNumber > state.LastAck {
			state.LastAck = hb.SequenceNumber
		}
	}
	state.CurBatch = nil

	// Fetch live balances from TB to populate the PG projection (I-4).
	// PG snapshots are advisory — failure here is non-fatal.
	balInput := LookupAccountBalancesInput{TenantQuotaIDs: state.AccountMap}
	var balResult LookupAccountBalancesResult
	if err := workflow.ExecuteActivity(actCtx, tbActivities.LookupAccountBalances, balInput).Get(ctx, &balResult); err != nil {
		workflow.GetLogger(ctx).Warn("LookupAccountBalances failed (non-fatal)", "error", err)
		return nil
	}

	pgInput := UpdateQuotaSnapshotsInput{
		TenantID:   state.TenantID,
		TenantUUID: state.TenantUUID,
		Snapshots:  balResult.Balances,
	}
	if err := workflow.ExecuteActivity(actCtx, pgActivities.UpdateQuotaSnapshots, pgInput).Get(ctx, nil); err != nil {
		// Non-fatal: PG snapshot lag is acceptable (I-4).
		workflow.GetLogger(ctx).Warn("UpdateQuotaSnapshots failed (non-fatal)", "error", err)
	}

	return nil
}

// flushAdjustment submits a quota credit to TigerBeetle immediately and returns
// the confirmed updated balances. Called from the UpdateIssueCredit handler —
// the caller blocks until this returns, giving a real ack for the financial operation.
func flushAdjustment(ctx workflow.Context, state *TenantAccountingState, sig QuotaAdjustmentSignal) (IssueCreditResult, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 5},
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	quotaBytes := state.AccountMap[sig.ResourceType]
	opBytes := state.GlobalOperatorIDs[sig.ResourceType]

	allocInput := SubmitAllocationInput{
		TenantUUID:        state.TenantUUID,
		TenantQuotaIDs:    map[string][16]byte{sig.ResourceType: quotaBytes},
		GlobalOperatorIDs: map[string][16]byte{sig.ResourceType: opBytes},
		Credits:           map[string]int64{sig.ResourceType: sig.Amount},
	}
	if err := workflow.ExecuteActivity(actCtx, tbActivities.SubmitAllocationTransfers, allocInput).Get(ctx, nil); err != nil {
		return IssueCreditResult{}, err
	}
	// Credits restored — clear exceeded flag for this resource so status returns to OK.
	delete(state.ExceededResources, sig.ResourceType)

	// Refresh PG projection and return live balances to the caller (I-4).
	balInput := LookupAccountBalancesInput{TenantQuotaIDs: state.AccountMap}
	var balResult LookupAccountBalancesResult
	if err := workflow.ExecuteActivity(actCtx, tbActivities.LookupAccountBalances, balInput).Get(ctx, &balResult); err != nil {
		workflow.GetLogger(ctx).Warn("LookupAccountBalances after adjustment failed (non-fatal)", "error", err)
		return IssueCreditResult{}, nil
	}

	pgInput := UpdateQuotaSnapshotsInput{
		TenantID:   state.TenantID,
		TenantUUID: state.TenantUUID,
		Snapshots:  balResult.Balances,
	}
	if err := workflow.ExecuteActivity(actCtx, pgActivities.UpdateQuotaSnapshots, pgInput).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Warn("UpdateQuotaSnapshots after adjustment failed (non-fatal)", "error", err)
	}

	return IssueCreditResult{Balances: balResult.Balances}, nil
}
