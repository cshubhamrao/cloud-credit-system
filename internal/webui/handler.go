package webui

import (
	_ "embed"
	"database/sql"
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/cshubhamrao/cloud-credit-system/internal/db/sqlcgen"
)

//go:embed simulator.html
var simHTML []byte

// HeartbeatMetrics is satisfied by *gateway.HeartbeatHandler.
type HeartbeatMetrics interface {
	MsgRate() (float64, uint64)
	Percentiles() (int64, int64)
}

// StreamMetrics is satisfied by *gateway.StreamManager.
type StreamMetrics interface {
	ActiveClusters() []string
}

// Handler serves the web UI page (/ui) and runtime stats JSON (/api/ui/stats).
// Tenant/cluster/quota data is fetched by the browser directly via ConnectRPC.
type Handler struct {
	db        *sql.DB
	startTime time.Time
	hb        HeartbeatMetrics
	sm        StreamMetrics
}

func NewHandler(pool *pgxpool.Pool, startTime time.Time, hb HeartbeatMetrics, sm StreamMetrics) *Handler {
	return &Handler{
		db:        stdlib.OpenDBFromPool(pool),
		startTime: startTime,
		hb:        hb,
		sm:        sm,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/ui/stats":
		h.serveStats(w, r)
	case "/api/ui/report":
		h.serveReport(w, r)
	case "/ui":
		http.Redirect(w, r, "/sim", http.StatusMovedPermanently)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(simHTML)
	}
}

type runtimeStats struct {
	Goroutines  int     `json:"goroutines"`
	HeapAllocMB float64 `json:"heap_alloc_mb"`
	HeapSysMB   float64 `json:"heap_sys_mb"`
	GCRuns      uint32  `json:"gc_runs"`
	UptimeS     int     `json:"uptime_s"`
	ActiveStreams int    `json:"active_streams"`
	MsgPerSec   float64 `json:"msg_per_sec"`
	MsgTotal    uint64  `json:"msg_total"`
	SignalP50Us int64   `json:"signal_p50_us"`
	SignalP99Us int64   `json:"signal_p99_us"`
}

func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	activeStreams := 0
	if h.sm != nil {
		activeStreams = len(h.sm.ActiveClusters())
	}
	var msgRate float64
	var msgTotal uint64
	var p50, p99 int64
	if h.hb != nil {
		msgRate, msgTotal = h.hb.MsgRate()
		p50, p99 = h.hb.Percentiles()
	}

	stats := runtimeStats{
		Goroutines:   runtime.NumGoroutine(),
		HeapAllocMB:  float64(ms.HeapAlloc) / 1024 / 1024,
		HeapSysMB:    float64(ms.HeapSys) / 1024 / 1024,
		GCRuns:       ms.NumGC,
		UptimeS:      int(time.Since(h.startTime).Seconds()),
		ActiveStreams: activeStreams,
		MsgPerSec:    msgRate,
		MsgTotal:     msgTotal,
		SignalP50Us:  p50,
		SignalP99Us:  p99,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

// quotaSnapshotJSON is the JSON shape for one quota snapshot row.
type quotaSnapshotJSON struct {
	TenantID     string `json:"tenant_id"`
	ResourceType string `json:"resource_type"`
	CreditsTotal int64  `json:"credits_total"`
	DebitsPosted int64  `json:"debits_posted"`
	DebitsPending int64 `json:"debits_pending"`
	Available    int64  `json:"available"`
	SnapshotAt   string `json:"snapshot_at"`
}

// staleClusterJSON is the JSON shape for one stale cluster row.
type staleClusterJSON struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenant_id"`
	CloudProvider string `json:"cloud_provider"`
	Region        string `json:"region"`
	Status        string `json:"status"`
	LastHeartbeat string `json:"last_heartbeat"` // empty if never seen
}

type reportPayload struct {
	QuotaSnapshots []quotaSnapshotJSON `json:"quota_snapshots"`
	StaleClusters  []staleClusterJSON  `json:"stale_clusters"`
}

// serveReport returns a cross-tenant quota snapshot and a list of clusters that
// have not sent a heartbeat in the last 5 minutes. Intended for dashboard polling.
func (h *Handler) serveReport(w http.ResponseWriter, r *http.Request) {
	q := sqlcgen.New(h.db)

	snapshots, err := q.GetAllQuotaSnapshots(r.Context())
	if err != nil {
		http.Error(w, "GetAllQuotaSnapshots: "+err.Error(), http.StatusInternalServerError)
		return
	}

	staleThreshold := sql.NullTime{Time: time.Now().Add(-5 * time.Minute), Valid: true}
	staleClusters, err := q.ListStaleClusters(r.Context(), staleThreshold)
	if err != nil {
		http.Error(w, "ListStaleClusters: "+err.Error(), http.StatusInternalServerError)
		return
	}

	payload := reportPayload{
		QuotaSnapshots: make([]quotaSnapshotJSON, 0, len(snapshots)),
		StaleClusters:  make([]staleClusterJSON, 0, len(staleClusters)),
	}

	for _, s := range snapshots {
		available := int64(0)
		if s.Available.Valid {
			available = s.Available.Int64
		}
		payload.QuotaSnapshots = append(payload.QuotaSnapshots, quotaSnapshotJSON{
			TenantID:      s.TenantID.String(),
			ResourceType:  s.ResourceType,
			CreditsTotal:  s.CreditsTotal,
			DebitsPosted:  s.DebitsPosted,
			DebitsPending: s.DebitsPending,
			Available:     available,
			SnapshotAt:    s.SnapshotAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	for _, c := range staleClusters {
		sc := staleClusterJSON{
			ID:            c.ID.String(),
			TenantID:      c.TenantID.String(),
			CloudProvider: c.CloudProvider,
			Region:        c.Region,
			Status:        c.Status,
		}
		if c.LastHeartbeat.Valid {
			sc.LastHeartbeat = c.LastHeartbeat.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		payload.StaleClusters = append(payload.StaleClusters, sc)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
