package webui

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed page.html
var pageHTML []byte

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
	pool      *pgxpool.Pool // kept for future server-side use; currently unused
	startTime time.Time
	hb        HeartbeatMetrics
	sm        StreamMetrics
}

func NewHandler(pool *pgxpool.Pool, startTime time.Time, hb HeartbeatMetrics, sm StreamMetrics) *Handler {
	return &Handler{pool: pool, startTime: startTime, hb: hb, sm: sm}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/ui/stats" {
		h.serveStats(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(pageHTML)
}

type runtimeStats struct {
	Goroutines   int     `json:"goroutines"`
	HeapAllocMB  float64 `json:"heap_alloc_mb"`
	HeapSysMB    float64 `json:"heap_sys_mb"`
	GCRuns       uint32  `json:"gc_runs"`
	UptimeS      int     `json:"uptime_s"`
	ActiveStreams int     `json:"active_streams"`
	MsgPerSec    float64 `json:"msg_per_sec"`
	MsgTotal     uint64  `json:"msg_total"`
	SignalP50Us  int64   `json:"signal_p50_us"`
	SignalP99Us  int64   `json:"signal_p99_us"`
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
