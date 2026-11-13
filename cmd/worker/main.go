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

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
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

	defaultSources, err := news.DefaultSources(cfg.News)
	if err != nil {
		logger.Error("load news sources failed", "error", err.Error())
		os.Exit(1)
	}
	newsRepo := postgres.NewNewsRepository(pgPool, cfg.Postgres.QueryTimeout)
	if err := newsRepo.EnsureSources(ctx, defaultSources); err != nil {
		logger.Error("ensure news sources failed", "error", err.Error())
		os.Exit(1)
	}
	newsHTTP := news.NewHTTPClient(news.HTTPClientOptions{
		Timeout:        cfg.News.HTTPTimeout,
		MaxBytes:       cfg.News.HTTPMaxBytes,
		MaxConcurrency: cfg.News.MaxConcurrency,
		UserAgent:      cfg.News.UserAgent,
	})
	newsActivities := &workflows.NewsActivities{
		Repo:       newsRepo,
		HTTPClient: newsHTTP,
		NewsConfig: cfg.News,
		Sources:    defaultSources,
		Logger:     logger,
		Metrics:    metrics,
	}
	prompts, err := ai.LoadEmbeddedPrompts()
	if err != nil {
		logger.Error("load embedded prompts failed", "error", err.Error())
		os.Exit(1)
	}
	editorRepo := postgres.NewEditorRepository(pgPool, cfg.Postgres.QueryTimeout)
	budget := ai.NewBudgetTracker(cfg.LLM.DailyBudgetUSD)
	var llmProvider ai.LLMProvider
	if cfg.LLM.APIKey != "" {
		llmProvider = ai.NewOpenAIProvider(cfg.LLM, budget, logger)
	} else {
		logger.Warn("llm api key not configured; DailyEditorialWorkflow will reject live LLM runs until LLM_API_KEY is set")
	}
	editorialActivities := &workflows.EditorialActivities{
		NewsRepo:     newsRepo,
		EditorRepo:   editorRepo,
		LLM:          llmProvider,
		Prompts:      prompts,
		Budget:       budget,
		LLMConfig:    cfg.LLM,
		EditorConfig: cfg.Editor,
		Logger:       logger,
		Metrics:      metrics,
	}

	w := worker.New(temporalClient, cfg.Temporal.TaskQueue, worker.Options{
		WorkerStopTimeout: cfg.Worker.ShutdownTimeout,
	})
	w.RegisterWorkflowWithOptions(workflows.InfrastructureSmokeWorkflow, workflow.RegisterOptions{
		Name: workflows.SmokeWorkflowName,
	})
	w.RegisterWorkflowWithOptions(workflows.NewsIngestionWorkflow, workflow.RegisterOptions{
		Name: workflows.NewsIngestionWorkflowName,
	})
	w.RegisterWorkflowWithOptions(workflows.DailyNewsSnapshotWorkflow, workflow.RegisterOptions{
		Name: workflows.DailyNewsSnapshotWorkflowName,
	})
	w.RegisterWorkflowWithOptions(workflows.DailyEditorialWorkflow, workflow.RegisterOptions{
		Name: workflows.DailyEditorialWorkflowName,
	})
	w.RegisterActivityWithOptions(activities.CreateSmokeRun, activity.RegisterOptions{Name: "CreateSmokeRun"})
	w.RegisterActivityWithOptions(activities.UploadAndVerifyArtifact, activity.RegisterOptions{Name: "UploadAndVerifyArtifact"})
	w.RegisterActivityWithOptions(activities.PersistMediaAsset, activity.RegisterOptions{Name: "PersistMediaAsset"})
	w.RegisterActivityWithOptions(activities.CompleteSmokeRun, activity.RegisterOptions{Name: "CompleteSmokeRun"})
	w.RegisterActivityWithOptions(activities.FailSmokeRun, activity.RegisterOptions{Name: "FailSmokeRun"})
	w.RegisterActivityWithOptions(newsActivities.ListEnabledSources, activity.RegisterOptions{Name: "ListEnabledSources"})
	w.RegisterActivityWithOptions(newsActivities.FetchAndPersistSource, activity.RegisterOptions{Name: "FetchAndPersistSource"})
	w.RegisterActivityWithOptions(newsActivities.CreateDailySnapshot, activity.RegisterOptions{Name: "CreateDailySnapshot"})
	w.RegisterActivityWithOptions(editorialActivities.LoadDailySnapshot, activity.RegisterOptions{Name: "LoadDailySnapshot"})
	w.RegisterActivityWithOptions(editorialActivities.PreprocessArticles, activity.RegisterOptions{Name: "PreprocessArticles"})
	w.RegisterActivityWithOptions(editorialActivities.ClusterStories, activity.RegisterOptions{Name: "ClusterStories"})
	w.RegisterActivityWithOptions(editorialActivities.ExtractClaims, activity.RegisterOptions{Name: "ExtractClaims"})
	w.RegisterActivityWithOptions(editorialActivities.VerifyClaims, activity.RegisterOptions{Name: "VerifyClaims"})
	w.RegisterActivityWithOptions(editorialActivities.FilterEligibleStories, activity.RegisterOptions{Name: "FilterEligibleStories"})
	w.RegisterActivityWithOptions(editorialActivities.ScoreStories, activity.RegisterOptions{Name: "ScoreStories"})
	w.RegisterActivityWithOptions(editorialActivities.SelectTopStories, activity.RegisterOptions{Name: "SelectTopStories"})
	w.RegisterActivityWithOptions(editorialActivities.PersistEditorialResults, activity.RegisterOptions{Name: "PersistEditorialResults"})
	w.RegisterActivityWithOptions(editorialActivities.GenerateEditorialReport, activity.RegisterOptions{Name: "GenerateEditorialReport"})

	if err := workflows.EnsureNewsSchedules(ctx, temporalClient, cfg.Temporal.TaskQueue, cfg.News, logger); err != nil {
		logger.Error("ensure news schedules failed", "error", err.Error())
		os.Exit(1)
	}

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
