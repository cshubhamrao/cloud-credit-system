package gateway

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"go.temporal.io/sdk/client"

	creditsystemv1 "github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1"
	"github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1/creditsystemv1connect"
	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/db/sqlcgen"
	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
)

// HeartbeatHandler implements the ConnectRPC HeartbeatService.
type HeartbeatHandler struct {
	creditsystemv1connect.UnimplementedHeartbeatServiceHandler

	temporal      client.Client
	db            *sql.DB
	dedup         Deduplicator
	streamManager *StreamManager
	// clusterToTenant caches cluster UUID string → tenant UUID string lookups.
	clusterToTenant sync.Map
	log             *slog.Logger

	// inboundMsgs counts every heartbeat message received (bidi + unary).
	inboundMsgs atomic.Uint64
	rateMu      sync.Mutex
	rateLastVal uint64
	rateLastAt  time.Time

	// latRing is a fixed ring buffer of the last latRingSize signalHeartbeat
	// durations in microseconds, used to compute p50/p99.
	latMu   sync.Mutex
	latRing [2048]int64
	latPos  int
	latFull bool
}

// MsgRate returns (msgs/sec since last call, total msgs).
func (h *HeartbeatHandler) MsgRate() (rate float64, total uint64) {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	now := time.Now()
	cur := h.inboundMsgs.Load()
	if elapsed := now.Sub(h.rateLastAt).Seconds(); elapsed > 0 {
		rate = float64(cur-h.rateLastVal) / elapsed
	}
	h.rateLastVal = cur
	h.rateLastAt = now
	return rate, cur
}

// recordLatency adds one signalHeartbeat duration to the ring buffer.
func (h *HeartbeatHandler) recordLatency(d time.Duration) {
	us := d.Microseconds()
	h.latMu.Lock()
	h.latRing[h.latPos] = us
	h.latPos++
	if h.latPos >= len(h.latRing) {
		h.latPos = 0
		h.latFull = true
	}
	h.latMu.Unlock()
}

// Percentiles returns p50 and p99 signal latency in microseconds.
func (h *HeartbeatHandler) Percentiles() (p50, p99 int64) {
	h.latMu.Lock()
	n := h.latPos
	if h.latFull {
		n = len(h.latRing)
	}
	if n == 0 {
		h.latMu.Unlock()
		return 0, 0
	}
	buf := make([]int64, n)
	copy(buf, h.latRing[:n])
	h.latMu.Unlock()

	sort.Slice(buf, func(i, j int) bool { return buf[i] < buf[j] })
	p50 = buf[int(float64(n)*0.50)]
	p99 = buf[int(float64(n)*0.99)]
	return p50, p99
}

func NewHeartbeatHandler(tc client.Client, pool *pgxpool.Pool, dedup Deduplicator, sm *StreamManager) *HeartbeatHandler {
	return &HeartbeatHandler{
		temporal:      tc,
		db:            stdlib.OpenDBFromPool(pool),
		dedup:         dedup,
		streamManager: sm,
		log:           slog.Default().With("handler", "heartbeat"),
	}
}

// HeartbeatStream handles the primary bidi streaming RPC.
// Each cluster maintains one long-lived stream; the server sends async ACKs
// as Temporal commits usage transfers.
func (h *HeartbeatHandler) HeartbeatStream(
	ctx context.Context,
	stream *connect.BidiStream[creditsystemv1.HeartbeatRequest, creditsystemv1.HeartbeatResponse],
) error {
	// Extract cluster ID from the first message before registering.
	var clusterID string

	h.log.Info("bidi stream opened")
	defer func() {
		if clusterID != "" {
			h.streamManager.Deregister(clusterID)
			h.log.Info("bidi stream closed", "clusterID", clusterID)
		}
	}()

	// sigCh decouples receive from Temporal signaling.
	// Buffer sized for ~2s of burst at max cluster rate (40 msg/s × 2s = 80).
	sigCh := make(chan *creditsystemv1.HeartbeatRequest, 256)

	// Recv goroutine: read off the wire, dedup, enqueue — never blocks on Temporal.
	go func() {
		defer close(sigCh)
		for {
			req, err := stream.Receive()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
					h.log.Warn("stream.Receive error", "error", err)
				}
				return
			}

			if clusterID == "" {
				clusterID = req.ClusterId
				h.streamManager.Register(clusterID)
				h.log.Info("cluster connected", "clusterID", clusterID)
			}

			h.inboundMsgs.Add(1)
			h.streamManager.Touch(clusterID)

			if h.dedup.IsDuplicate(clusterID, req.SequenceNumber) {
				h.log.Debug("gateway dedup: skipping heartbeat", "cluster", clusterID, "seq", req.SequenceNumber)
				continue
			}

			select {
			case sigCh <- req:
			default:
				h.log.Warn("signal channel full, dropping heartbeat", "cluster", clusterID, "seq", req.SequenceNumber)
			}
		}
	}()

	// Signal dispatcher: drains sigCh and calls Temporal — one goroutine per stream.
	go func() {
		for req := range sigCh {
			t0 := time.Now()
			if err := h.signalHeartbeat(ctx, req); err != nil {
				h.log.Error("signalHeartbeat error", "error", err, "cluster", clusterID)
			}
			h.recordLatency(time.Since(t0))
		}
	}()

	// Send loop: poll Temporal for ack_sequence and send responses.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if clusterID == "" {
				continue
			}
			ack, quotas, err := h.queryWorkflowState(ctx, clusterID)
			if err != nil {
				h.log.Debug("queryWorkflowState", "error", err)
				continue
			}
			resp := &creditsystemv1.HeartbeatResponse{
				AckSequence: ack,
				Quotas:      quotas,
			}
			resp.Status = overallStatus(quotas)
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// Heartbeat handles the unary fallback RPC.
func (h *HeartbeatHandler) Heartbeat(
	ctx context.Context,
	req *connect.Request[creditsystemv1.HeartbeatRequest],
) (*connect.Response[creditsystemv1.HeartbeatResponse], error) {
	r := req.Msg
	clusterID := r.ClusterId
	h.inboundMsgs.Add(1)

	if h.dedup.IsDuplicate(clusterID, r.SequenceNumber) {
		h.log.Debug("gateway dedup: unary duplicate", "cluster", clusterID, "seq", r.SequenceNumber)
	} else {
		t0 := time.Now()
		err := h.signalHeartbeat(ctx, r)
		h.recordLatency(time.Since(t0))
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	ack, quotas, err := h.queryWorkflowState(ctx, clusterID)
	if err != nil {
		h.log.Debug("queryWorkflowState for unary", "error", err)
	}

	resp := &creditsystemv1.HeartbeatResponse{
		AckSequence: ack,
		Quotas:      quotas,
	}
	resp.Status = overallStatus(quotas)
	return connect.NewResponse(resp), nil
}

// signalHeartbeat parses and dispatches a heartbeat to the tenant accounting workflow.
func (h *HeartbeatHandler) signalHeartbeat(ctx context.Context, req *creditsystemv1.HeartbeatRequest) error {
	clusterUUID, err := uuidToBytes(req.ClusterId)
	if err != nil {
		return fmt.Errorf("parse cluster UUID: %w", err)
	}

	var tsNs int64
	if req.Timestamp != nil {
		tsNs = req.Timestamp.AsTime().UnixNano()
	}

	sig := accounting.HeartbeatSignal{
		ClusterID:            req.ClusterId,
		ClusterUUID:          clusterUUID,
		SequenceNumber:       req.SequenceNumber,
		CPUMillisecondsDelta: req.CpuMillisecondsDelta,
		MemoryMBSecondsDelta: req.MemoryMbSecondsDelta,
		ActiveNodes:          req.ActiveNodes,
		HeartbeatTimestampNs: tsNs,
	}

	tenantID, err := h.resolveTenantID(ctx, req.ClusterId)
	if err != nil {
		return fmt.Errorf("resolve tenant for cluster %s: %w", req.ClusterId, err)
	}

	tenantWorkflowID := accounting.AccountingWorkflowID(tenantID)
	return h.temporal.SignalWorkflow(ctx, tenantWorkflowID, "", accounting.SignalHeartbeat, sig)
}

// queryWorkflowState fetches the latest ack sequence and quota info from Temporal + PG.
func (h *HeartbeatHandler) queryWorkflowState(ctx context.Context, clusterID string) (uint64, []*creditsystemv1.QuotaInfo, error) {
	tenantID, err := h.resolveTenantID(ctx, clusterID)
	if err != nil {
		return 0, nil, err
	}

	tenantWorkflowID := accounting.AccountingWorkflowID(tenantID)
	qresp, err := h.temporal.QueryWorkflow(ctx, tenantWorkflowID, "", accounting.QueryLastTBAck)
	if err != nil {
		return 0, nil, err
	}
	var ack uint64
	if err := qresp.Get(&ack); err != nil {
		return 0, nil, err
	}

	// Query which resources TigerBeetle has rejected as exceeding credits.
	// This catches the "crumbs" case: remaining balance > 0 but smaller than
	// any feasible heartbeat delta, so all transfers are rejected.
	var exceededResources map[string]bool
	if qr, err := h.temporal.QueryWorkflow(ctx, tenantWorkflowID, "", accounting.QueryExceededResources); err == nil {
		_ = qr.Get(&exceededResources)
	}

	// Read quota snapshots from PG for display (I-4: advisory only, never enforcement).
	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		return ack, nil, nil
	}
	q := sqlcgen.New(h.db)
	snapshots, err := q.GetQuotaSnapshots(ctx, tenantUUID)
	if err != nil {
		h.log.Debug("GetQuotaSnapshots", "error", err)
		return ack, nil, nil
	}

	quotas := make([]*creditsystemv1.QuotaInfo, 0, len(snapshots))
	for _, s := range snapshots {
		available := s.Available.Int64
		used := s.DebitsPosted
		if domain.ResourceType(s.ResourceType).IsGauge() {
			used = s.DebitsPending
		}
		status := creditsystemv1.Status_STATUS_OK
		// QUOTA_EXCEEDED: either balance is zero, or TigerBeetle rejected the last
		// transfer for this resource (balance too small to accept any new heartbeat).
		if !s.Available.Valid || available <= 0 || exceededResources[s.ResourceType] {
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

	return ack, quotas, nil
}

// resolveTenantID resolves a cluster ID string to its tenant ID string.
// Results are cached in-memory after the first PG lookup.
func (h *HeartbeatHandler) resolveTenantID(ctx context.Context, clusterID string) (string, error) {
	if v, ok := h.clusterToTenant.Load(clusterID); ok {
		return v.(string), nil
	}

	clusterUUID, err := uuid.Parse(clusterID)
	if err != nil {
		return "", fmt.Errorf("cluster ID is not a valid UUID: %w", err)
	}

	q := sqlcgen.New(h.db)
	cluster, err := q.GetCluster(ctx, clusterUUID)
	if err != nil {
		return "", fmt.Errorf("GetCluster %s: %w", clusterID, err)
	}

	tenantID := cluster.TenantID.String()
	h.clusterToTenant.Store(clusterID, tenantID)
	return tenantID, nil
}

func uuidToBytes(id string) ([16]byte, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		// For the PoC simulator which may send non-UUID cluster IDs,
		// derive from hex or pad.
		var b [16]byte
		decoded, _ := hex.DecodeString(id)
		copy(b[:], decoded)
		return b, nil
	}
	return [16]byte(u), nil
}

func overallStatus(quotas []*creditsystemv1.QuotaInfo) creditsystemv1.Status {
	worst := creditsystemv1.Status_STATUS_UNSPECIFIED
	for _, q := range quotas {
		if q.Status > worst {
			worst = q.Status
		}
	}
	if worst == creditsystemv1.Status_STATUS_UNSPECIFIED {
		return creditsystemv1.Status_STATUS_OK
	}
	return worst
}
