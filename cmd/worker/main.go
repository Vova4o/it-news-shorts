// Command worker hosts the Temporal worker executing infrastructure and pipeline
// workflows.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("load config failed", "error", err.Error())
		os.Exit(1)
	}

	logger := observability.NewLogger("worker", cfg.App.LogLevel, os.Stdout)
	metrics := observability.NewMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pgPool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		logger.Error("connect postgres failed", "error", err.Error())
		os.Exit(1)
	}
	defer pgPool.Close()

	if os.Getenv("AUTO_MIGRATE") == "true" {
		if err := postgres.MigrateUp(ctx, cfg.Postgres.DSN); err != nil {
			logger.Error("auto migrate failed", "error", err.Error())
			os.Exit(1)
		}
		logger.Info("database migrations verified")
	}

	s3Store, err := storage.NewS3Store(ctx, cfg.MinIO)
	if err != nil {
		logger.Error("create s3 store failed", "error", err.Error())
		os.Exit(1)
	}
	if err := s3Store.EnsureBucket(ctx, cfg.MinIO.Bucket); err != nil {
		logger.Error("ensure minio bucket failed", "bucket", cfg.MinIO.Bucket, "error", err.Error())
		os.Exit(1)
	}

	if err := workflows.EnsureNamespace(ctx, cfg.Temporal.Address, cfg.Temporal.Namespace, 7*24*time.Hour, logger); err != nil {
		logger.Error("ensure temporal namespace failed", "namespace", cfg.Temporal.Namespace, "error", err.Error())
		os.Exit(1)
	}

	temporalClient, err := workflows.DialTemporal(ctx, cfg.Temporal, logger)
	if err != nil {
		logger.Error("dial temporal failed", "error", err.Error())
		os.Exit(1)
	}
	defer temporalClient.Close()

	activities := &workflows.SmokeActivities{
		DailyRuns:   postgres.NewDailyRunRepository(pgPool, cfg.Postgres.QueryTimeout),
		Stories:     postgres.NewStoryRepository(pgPool, cfg.Postgres.QueryTimeout),
		VideoJobs:   postgres.NewVideoJobRepository(pgPool, cfg.Postgres.QueryTimeout),
		MediaAssets: postgres.NewMediaAssetRepository(pgPool, cfg.Postgres.QueryTimeout),
		Storage:     s3Store,
		Logger:      logger,
		Metrics:     metrics,
	}

	w := worker.New(temporalClient, cfg.Temporal.TaskQueue, worker.Options{
		WorkerStopTimeout: cfg.Worker.ShutdownTimeout,
	})
	w.RegisterWorkflowWithOptions(workflows.InfrastructureSmokeWorkflow, workflow.RegisterOptions{
		Name: workflows.SmokeWorkflowName,
	})
	w.RegisterActivityWithOptions(activities.CreateSmokeRun, activity.RegisterOptions{Name: "CreateSmokeRun"})
	w.RegisterActivityWithOptions(activities.UploadAndVerifyArtifact, activity.RegisterOptions{Name: "UploadAndVerifyArtifact"})
	w.RegisterActivityWithOptions(activities.PersistMediaAsset, activity.RegisterOptions{Name: "PersistMediaAsset"})
	w.RegisterActivityWithOptions(activities.CompleteSmokeRun, activity.RegisterOptions{Name: "CompleteSmokeRun"})
	w.RegisterActivityWithOptions(activities.FailSmokeRun, activity.RegisterOptions{Name: "FailSmokeRun"})

	metricsSrv := startMetricsServer(cfg.Worker.MetricsListenAddr, metrics, logger)
	defer func() {
		if metricsSrv != nil {
			sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsSrv.Shutdown(sCtx)
		}
	}()

	if err := w.Start(); err != nil {
		logger.Error("start temporal worker failed", "error", err.Error())
		os.Exit(1)
	}
	logger.Info("temporal worker started",
		"namespace", cfg.Temporal.Namespace,
		"task_queue", cfg.Temporal.TaskQueue,
		"temporal_address", cfg.Temporal.Address,
	)

	<-ctx.Done()
	logger.Info("temporal worker shutting down")
	w.Stop()
	logger.Info("temporal worker stopped")
}

func startMetricsServer(addr string, metrics *observability.Metrics, logger *slog.Logger) *http.Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","service":"worker"}`))
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker metrics server failed", "error", err.Error())
		}
	}()
	return srv
}
