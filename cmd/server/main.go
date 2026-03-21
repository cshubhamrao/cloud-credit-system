package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"net"
	"strconv"

	glide "github.com/valkey-io/valkey-glide/go/v2"
	glideconfig "github.com/valkey-io/valkey-glide/go/v2/config"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"connectrpc.com/connect"

	"github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1/creditsystemv1connect"
	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/compress"
	"github.com/cshubhamrao/cloud-credit-system/internal/config"
	"github.com/cshubhamrao/cloud-credit-system/internal/db"
	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
	"github.com/cshubhamrao/cloud-credit-system/internal/gateway"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
	"github.com/cshubhamrao/cloud-credit-system/internal/webui"

	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ─── PostgreSQL ──────────────────────────────────────────────────────────
	pool, err := db.NewPool(ctx, cfg.PostgresDSN)
	must(err, "postgres")
	defer pool.Close()
	log.Info("postgres connected", "dsn", cfg.PostgresDSN)

	// ─── TigerBeetle ────────────────────────────────────────────────────────
	tbClient, err := ledger.NewClient(cfg.TigerBeetleCluster, cfg.TigerBeetleAddr)
	must(err, "tigerbeetle")
	defer tbClient.Close()
	log.Info("tigerbeetle connected", "addr", cfg.TigerBeetleAddr)

	// Create global operator + sink accounts (idempotent).
	globalAccountUint128, err := ledger.CreateGlobalAccounts(tbClient, nil)
	must(err, "create global accounts")

	// Convert to [16]byte map for handlers.
	globalAccounts := make(map[ledger.GlobalAccountKey][16]byte)
	for k, id := range globalAccountUint128 {
		globalAccounts[k] = ledger.Uint128ToBytes(id)
	}
	log.Info("global TB accounts ready", "count", len(globalAccounts))

	// ─── Temporal ────────────────────────────────────────────────────────────
	temporalClient, err := accounting.NewClient(cfg.TemporalHost, cfg.TemporalNamespace)
	must(err, "temporal client")
	defer temporalClient.Close()

	tbActs := accounting.NewTBActivities(tbClient)
	pgActs := accounting.NewPGActivities(pool)
	w := accounting.StartWorker(temporalClient, tbActs, pgActs)
	if err := w.Start(); err != nil {
		log.Error("temporal worker start failed", "error", err)
		os.Exit(1)
	}
	defer w.Stop()
	log.Info("temporal worker started", "taskQueue", accounting.TaskQueueAccounting)

	// ─── Debug HTTP server (:6061) ───────────────────────────────────────────
	startTime := time.Now()
	debugMux := http.NewServeMux()
	debugMux.HandleFunc("/debug/pprof/", pprof.Index)
	debugMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	debugMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	debugMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	debugMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// /debug/stats is wired up after streamMgr and hbHandler are created (below).
	var streamMgrRef *gateway.StreamManager
	var hbHandlerRef *gateway.HeartbeatHandler

	debugMux.HandleFunc("/debug/stats", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		activeStreams := 0
		if streamMgrRef != nil {
			activeStreams = len(streamMgrRef.ActiveClusters())
		}
		var msgRate float64
		var msgTotal uint64
		var p50Us, p99Us int64
		if hbHandlerRef != nil {
			msgRate, msgTotal = hbHandlerRef.MsgRate()
			p50Us, p99Us = hbHandlerRef.Percentiles()
		}
		stats := struct {
			Goroutines    int     `json:"goroutines"`
			HeapAllocMB   float64 `json:"heap_alloc_mb"`
			HeapSysMB     float64 `json:"heap_sys_mb"`
			GCRuns        uint32  `json:"gc_runs"`
			GCPauseUS     uint64  `json:"gc_pause_us"`
			UptimeS       int     `json:"uptime_s"`
			ActiveStreams  int     `json:"active_streams"`
			MsgPerSec     float64 `json:"msg_per_sec"`
			MsgTotal      uint64  `json:"msg_total"`
			SignalP50Us   int64   `json:"signal_p50_us"`
			SignalP99Us   int64   `json:"signal_p99_us"`
		}{
			Goroutines:   runtime.NumGoroutine(),
			HeapAllocMB:  float64(ms.HeapAlloc) / 1024 / 1024,
			HeapSysMB:    float64(ms.HeapSys) / 1024 / 1024,
			GCRuns:       ms.NumGC,
			GCPauseUS:    ms.PauseNs[(ms.NumGC+255)%256] / 1000,
			UptimeS:      int(time.Since(startTime).Seconds()),
			ActiveStreams: activeStreams,
			MsgPerSec:    msgRate,
			MsgTotal:     msgTotal,
			SignalP50Us:  p50Us,
			SignalP99Us:  p99Us,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	debugSrv := &http.Server{Addr: ":6061", Handler: debugMux}
	go func() {
		if err := debugSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("debug server error", "error", err)
		}
	}()
	log.Info("debug server listening", "addr", ":6061")

	// ─── Valkey/Redis (shared dedup across pods) ─────────────────────────────
	host, portStr, err := net.SplitHostPort(cfg.RedisAddr)
	must(err, "parse redis addr")
	port, _ := strconv.Atoi(portStr)
	valkeyClient, err := glide.NewClient(glideconfig.NewClientConfiguration().
		WithAddress(&glideconfig.NodeAddress{Host: host, Port: port}))
	if err != nil {
		log.Warn("valkey unreachable, falling back to in-memory dedup", "addr", cfg.RedisAddr, "error", err)
	} else {
		log.Info("valkey connected", "addr", cfg.RedisAddr)
	}
	if valkeyClient != nil {
		defer valkeyClient.Close()
	}

	// ─── ConnectRPC Handlers ─────────────────────────────────────────────────
	var dedupCache gateway.Deduplicator
	if valkeyClient != nil {
		dedupCache = gateway.NewRedisDedup(valkeyClient, 5*time.Minute)
	} else {
		dedupCache = gateway.NewMemDedup(5 * time.Minute)
	}
	streamMgr := gateway.NewStreamManager()
	streamMgrRef = streamMgr

	// Register zstd on every handler so the server can both receive and send
	// zstd-compressed messages. gzip remains available as the built-in fallback
	// for clients that don't advertise zstd.
	zstdHandler := connect.WithCompression(
		compress.Name,
		func() connect.Decompressor { return compress.NewDecompressor() },
		func() connect.Compressor { return compress.NewCompressor() },
	)

	mux := http.NewServeMux()

	hbHandler := gateway.NewHeartbeatHandler(temporalClient, pool, dedupCache, streamMgr)
	hbHandlerRef = hbHandler
	mux.Handle(creditsystemv1connect.NewHeartbeatServiceHandler(hbHandler, zstdHandler))

	adminHandler := gateway.NewAdminHandler(temporalClient, pool)
	mux.Handle(creditsystemv1connect.NewAdminServiceHandler(adminHandler, zstdHandler))

	// Build global account map for provisioning handler.
	globalForProvisioning := make(map[ledger.GlobalAccountKey][16]byte)
	for _, r := range domain.AllResources {
		for _, at := range []domain.AccountType{domain.AccountTypeOperator, domain.AccountTypeSink} {
			k := ledger.GlobalAccountKey{Resource: r, AccountType: at}
			if id, ok := globalAccounts[k]; ok {
				globalForProvisioning[k] = id
			}
		}
	}
	provHandler := gateway.NewProvisioningHandler(temporalClient, globalForProvisioning)
	mux.Handle(creditsystemv1connect.NewProvisioningServiceHandler(provHandler, zstdHandler))

	// ─── Web UI ──────────────────────────────────────────────────────────────
	uiHandler := webui.NewHandler(pool, startTime, hbHandler, streamMgr)
	mux.Handle("/ui", uiHandler)
	mux.Handle("/api/ui/stats", uiHandler)
	mux.Handle("/api/ui/report", uiHandler)

	// ─── HTTP Server (h2c — HTTP/2 cleartext for gRPC bidi without TLS) ─────
	h2s := &http2.Server{}
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: h2c.NewHandler(mux, h2s),
	}

	log.Info("server listening", "addr", cfg.ListenAddr)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("server shutdown error", "error", err)
	}
	_ = debugSrv.Shutdown(shutdownCtx)
	log.Info("server stopped")
}

func must(err error, label string) {
	if err != nil {
		slog.Error("startup error", "component", label, "error", err)
		os.Exit(1)
	}
}

// Ensure types import is used for TB ID zero-value.
var _ types.Uint128
