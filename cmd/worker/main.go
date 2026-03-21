package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/config"
	"github.com/cshubhamrao/cloud-credit-system/internal/db"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"

)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.PostgresDSN)
	must(err, "postgres")
	defer pool.Close()
	log.Info("postgres connected")

	tbClient, err := ledger.NewClient(cfg.TigerBeetleCluster, cfg.TigerBeetleAddr)
	must(err, "tigerbeetle")
	defer tbClient.Close()
	log.Info("tigerbeetle connected")

	temporalClient, err := accounting.NewClient(cfg.TemporalHost, cfg.TemporalNamespace)
	must(err, "temporal client")
	defer temporalClient.Close()

	tbActs := accounting.NewTBActivities(tbClient)
	pgActs := accounting.NewPGActivities(pool)
	w := accounting.StartWorker(temporalClient, tbActs, pgActs)
	if err := w.Start(); err != nil {
		log.Error("worker start failed", "error", err)
		os.Exit(1)
	}
	defer w.Stop()
	log.Info("temporal worker started", "taskQueue", accounting.TaskQueueAccounting)

	<-ctx.Done()
	log.Info("worker stopped")
}

func must(err error, label string) {
	if err != nil {
		slog.Error("startup error", "component", label, "error", err)
		os.Exit(1)
	}
}
