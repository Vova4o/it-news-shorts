package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

const infraUsage = `usage: cli infra <command> [flags]

commands:
  smoke-test               run InfrastructureSmokeWorkflow and verify PG + MinIO
  status                   check PostgreSQL, MinIO, and Temporal availability
  check-db                 verify PostgreSQL connectivity and applied schema
  check-storage            verify MinIO connectivity and bucket availability
  migrate-up               apply PostgreSQL migrations
  migrate-down             roll back the latest PostgreSQL migration
  migrate-youtube-journal  import local YouTube upload journal into PostgreSQL
`

func runInfra(args []string) {
	if len(args) == 0 {
		fmt.Print(infraUsage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, rest := args[0], args[1:]
	var err error
	switch command {
	case "smoke-test":
		err = infraSmokeTest(ctx, rest)
	case "status":
		err = infraStatus(ctx, rest)
	case "check-db":
		err = infraCheckDB(ctx, rest)
	case "check-storage":
		err = infraCheckStorage(ctx, rest)
	case "migrate-up":
		err = infraMigrateUp(ctx, rest)
	case "migrate-down":
		err = infraMigrateDown(ctx, rest)
	case "migrate-youtube-journal":
		err = infraMigrateYouTubeJournal(ctx, rest)
	case "help", "-h", "--help":
		fmt.Print(infraUsage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown infra command %q\n\n", command)
		fmt.Print(infraUsage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func infraSmokeTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("infra smoke-test", flag.ContinueOnError)
	runID := fs.String("run-id", "", "deterministic business run id (defaults to generated UUID)")
	testName := fs.String("test-name", "cli-smoke", "smoke test name")
	content := fs.String("content", "it-news-shorts infrastructure smoke artifact", "artifact content")
	delay := fs.Duration("delay", 0, "optional delay inside UploadAndVerifyArtifact")
	timeout := fs.Duration("timeout", 2*time.Minute, "workflow execution timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}

	id := *runID
	if id == "" {
		id = "smoke-" + uuid.NewString()
	}

	wfCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	cliLogger := observability.NewLogger("cli", cfg.App.LogLevel, os.Stderr)

	if err := workflows.EnsureNamespace(wfCtx, cfg.Temporal.Address, cfg.Temporal.Namespace, 7*24*time.Hour, cliLogger); err != nil {
		return err
	}
	tc, err := workflows.DialTemporal(wfCtx, cfg.Temporal, cliLogger)
	if err != nil {
		return err
	}
	defer tc.Close()

	workflowID := "smoke-" + id
	we, err := tc.ExecuteWorkflow(wfCtx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.SmokeWorkflowName, workflows.InfrastructureSmokeInput{
		RunID:           id,
		TestName:        *testName,
		ArtifactContent: *content,
		Delay:           *delay,
		ActivityTimeout: cfg.Temporal.ActivityTimeout,
	})
	if err != nil {
		return fmt.Errorf("execute smoke workflow: %w", err)
	}

	var wfRes workflows.InfrastructureSmokeResult
	if err := we.Get(wfCtx, &wfRes); err != nil {
		return fmt.Errorf("wait for smoke workflow: %w", err)
	}

	// Independently verify PostgreSQL and MinIO state after workflow completion.
	pgPool, err := postgres.Connect(wfCtx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pgPool.Close()

	runRepo := postgres.NewDailyRunRepository(pgPool, cfg.Postgres.QueryTimeout)
	runRec, err := runRepo.GetByID(wfCtx, domain.DailyRunID(id))
	if err != nil {
		return fmt.Errorf("verify daily_run in postgres: %w", err)
	}
	if runRec.Run.State != domain.DailyRunCompleted {
		return fmt.Errorf("unexpected daily_run state %s, want COMPLETED", runRec.Run.State)
	}

	assetRepo := postgres.NewMediaAssetRepository(pgPool, cfg.Postgres.QueryTimeout)
	asset, err := assetRepo.GetByObjectKey(wfCtx, cfg.MinIO.Bucket, wfRes.ArtifactKey)
	if err != nil {
		return fmt.Errorf("verify media_asset in postgres: %w", err)
	}

	s3Store, err := storage.NewS3Store(wfCtx, cfg.MinIO)
	if err != nil {
		return err
	}
	body, stat, err := s3Store.GetObject(wfCtx, cfg.MinIO.Bucket, wfRes.ArtifactKey)
	if err != nil {
		return fmt.Errorf("verify object in minio: %w", err)
	}
	defer body.Close()

	h := sha256.New()
	n, err := io.Copy(h, body)
	if err != nil {
		return fmt.Errorf("read object from minio: %w", err)
	}
	actualSHA := hex.EncodeToString(h.Sum(nil))
	if actualSHA != asset.Checksum || n != asset.SizeBytes || stat.SizeBytes != asset.SizeBytes {
		return fmt.Errorf("checksum/size mismatch: minio=(%s,%d) postgres=(%s,%d)", actualSHA, n, asset.Checksum, asset.SizeBytes)
	}

	summary := map[string]any{
		"workflow_id":        we.GetID(),
		"temporal_run_id":    we.GetRunID(),
		"business_run_id":    wfRes.RunID,
		"status":             wfRes.Status,
		"artifact_key":       wfRes.ArtifactKey,
		"checksum":           actualSHA,
		"size_bytes":         n,
		"checksum_verified":  wfRes.ChecksumVerified,
		"database_persisted": wfRes.DatabasePersisted,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(summary)
}

func infraStatus(ctx context.Context, _ []string) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	fmt.Printf("PostgreSQL: ")
	if err := infraCheckDB(ctx, nil); err != nil {
		fmt.Printf("ERROR (%v)\n", err)
		return err
	}
	fmt.Printf("MinIO:      ")
	if err := infraCheckStorage(ctx, nil); err != nil {
		fmt.Printf("ERROR (%v)\n", err)
		return err
	}
	cliLogger := observability.NewLogger("cli", cfg.App.LogLevel, os.Stderr)
	fmt.Printf("Temporal:   ")
	if err := workflows.CheckTemporalReady(ctx, cfg.Temporal.Address, cfg.Temporal.Namespace, 5*time.Second, cliLogger); err != nil {
		fmt.Printf("ERROR (%v)\n", err)
		return err
	}
	fmt.Printf("OK (address=%s namespace=%s queue=%s)\n", cfg.Temporal.Address, cfg.Temporal.Namespace, cfg.Temporal.TaskQueue)
	return nil
}

func infraCheckDB(ctx context.Context, _ []string) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	qCtx, cancel := context.WithTimeout(ctx, cfg.Postgres.QueryTimeout)
	defer cancel()

	var count int
	if err := pool.QueryRow(qCtx, `
		SELECT count(*)
		FROM information_schema.tables
		WHERE table_schema = 'public'
		  AND table_name IN ('daily_runs', 'video_jobs', 'media_assets', 'publications', 'youtube_upload_journal')
	`).Scan(&count); err != nil {
		return fmt.Errorf("check schema tables: %w", err)
	}
	fmt.Printf("OK (dsn=%s core_tables=%d/5)\n", config.RedactDSN(cfg.Postgres.DSN), count)
	return nil
}

func infraCheckStorage(ctx context.Context, _ []string) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	store, err := storage.NewS3Store(ctx, cfg.MinIO)
	if err != nil {
		return err
	}
	if err := store.Ping(ctx); err != nil {
		return err
	}
	fmt.Printf("OK (endpoint=%s bucket=%s)\n", cfg.MinIO.Endpoint, cfg.MinIO.Bucket)
	return nil
}

func infraMigrateUp(ctx context.Context, _ []string) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	if err := postgres.MigrateUp(ctx, cfg.Postgres.DSN); err != nil {
		return err
	}
	fmt.Printf("migrations applied to %s\n", config.RedactDSN(cfg.Postgres.DSN))
	return nil
}

func infraMigrateDown(ctx context.Context, _ []string) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	if err := postgres.MigrateDown(ctx, cfg.Postgres.DSN); err != nil {
		return err
	}
	fmt.Printf("latest migration rolled back on %s\n", config.RedactDSN(cfg.Postgres.DSN))
	return nil
}

func infraMigrateYouTubeJournal(ctx context.Context, _ []string) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	fileJournal, err := openJournal(os.Getenv)
	if err != nil {
		return err
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	pgJournal := postgres.NewPGJournal(pool, cfg.Postgres.QueryTimeout)
	report, err := postgres.MigrateFileJournalToPG(ctx, fileJournal, pgJournal)
	if err != nil {
		return err
	}
	fmt.Printf("youtube journal migrated: total=%d imported=%d skipped=%d\n", report.Total, report.Imported, report.Skipped)
	return nil
}
