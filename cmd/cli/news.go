package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

const newsUsage = `usage: cli news <command> [flags]

commands:
  sources    list configured news sources and fetch status
  collect    run NewsIngestionWorkflow via Temporal
  list       list collected articles from PostgreSQL
  snapshot   create a 24h daily snapshot (09:00 Europe/Moscow)
  stats      show ingestion statistics and coverage
`

func runNews(args []string) {
	if len(args) == 0 {
		fmt.Print(newsUsage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, rest := args[0], args[1:]
	var err error
	switch command {
	case "sources":
		err = newsSources(ctx, rest)
	case "collect":
		err = newsCollect(ctx, rest)
	case "list":
		err = newsList(ctx, rest)
	case "snapshot":
		err = newsSnapshot(ctx, rest)
	case "stats":
		err = newsStats(ctx, rest)
	case "help", "-h", "--help":
		fmt.Print(newsUsage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown news command %q\n\n", command)
		fmt.Print(newsUsage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newsSources(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("news sources", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	repo := postgres.NewNewsRepository(pool, cfg.Postgres.QueryTimeout)
	defaults, err := news.DefaultSources(cfg.News)
	if err != nil {
		return err
	}
	if err := repo.EnsureSources(ctx, defaults); err != nil {
		return err
	}
	sources, err := repo.ListSources(ctx)
	if err != nil {
		return err
	}

	if *jsonOut {
		return printJSON(sources)
	}
	for _, s := range sources {
		lastSuccess := "never"
		if s.State.LastSuccessAt != nil {
			lastSuccess = s.State.LastSuccessAt.UTC().Format(time.RFC3339)
		}
		lastErr := s.State.LastError
		if lastErr == "" {
			lastErr = "-"
		}
		fmt.Printf("%-14s %-12s enabled=%-5v last_success=%-22s last_error=%s\n",
			s.Config.ID, s.Config.Type, s.Config.Enabled, lastSuccess, lastErr)
	}
	return nil
}

func newsCollect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("news collect", flag.ContinueOnError)
	sourceID := fs.String("source", "", "filter to a single source ID")
	limit := fs.Int("limit", 0, "override per-source fetch limit")
	dryRun := fs.Bool("dry-run", false, "fetch and normalize without writing to PostgreSQL")
	timeout := fs.Duration("timeout", 3*time.Minute, "workflow execution timeout")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}

	wfCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	logger := observability.NewLogger("cli", cfg.App.LogLevel, os.Stderr)
	if err := workflows.EnsureNamespace(wfCtx, cfg.Temporal.Address, cfg.Temporal.Namespace, 7*24*time.Hour, logger); err != nil {
		return err
	}
	tc, err := workflows.DialTemporal(wfCtx, cfg.Temporal, logger)
	if err != nil {
		return err
	}
	defer tc.Close()

	runID := "news-" + uuid.NewString()
	we, err := tc.ExecuteWorkflow(wfCtx, client.StartWorkflowOptions{
		ID:        runID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.NewsIngestionWorkflowName, workflows.NewsIngestionInput{
		RunID:          runID,
		SourceID:       *sourceID,
		Limit:          *limit,
		DryRun:         *dryRun,
		MaxConcurrency: cfg.News.MaxConcurrency,
	})
	if err != nil {
		return fmt.Errorf("start NewsIngestionWorkflow: %w", err)
	}

	var res workflows.NewsIngestionResult
	if err := we.Get(wfCtx, &res); err != nil {
		return fmt.Errorf("NewsIngestionWorkflow %s failed: %w", we.GetID(), err)
	}

	if *jsonOut {
		return printJSON(res)
	}
	fmt.Printf("workflow_id=%s status=%s dry_run=%v fetched=%d inserted=%d duplicates=%d duration_ms=%d\n",
		res.WorkflowID, res.Status, *dryRun, res.FetchedCount, res.InsertedCount, res.DuplicateCount, res.DurationMS)
	for _, r := range res.Results {
		fmt.Printf("  %-14s status=%-12s fetched=%-3d inserted=%-3d dupes=%-3d errors=%-2d duration_ms=%d",
			r.SourceID, r.Status, r.FetchedCount, r.InsertedCount, r.DuplicateCount, r.ErrorCount, r.DurationMS)
		if r.Error != "" {
			fmt.Printf(" error=%q", r.Error)
		}
		fmt.Println()
	}
	return nil
}

func newsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("news list", flag.ContinueOnError)
	last := fs.Duration("last", 24*time.Hour, "lookback duration")
	sourceID := fs.String("source", "", "filter by source ID")
	limit := fs.Int("limit", 50, "maximum articles to return")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	repo := postgres.NewNewsRepository(pool, cfg.Postgres.QueryTimeout)
	since := time.Now().UTC().Add(-*last)
	articles, err := repo.ListRecentArticles(ctx, since, *sourceID, *limit)
	if err != nil {
		return err
	}

	if *jsonOut {
		return printJSON(articles)
	}
	fmt.Printf("articles=%d since=%s\n", len(articles), since.Format(time.RFC3339))
	for i, a := range articles {
		dateStr := "no-published-at"
		if a.PublishedAt != nil {
			dateStr = a.PublishedAt.UTC().Format(time.RFC3339)
		} else if a.SourceSubmittedAt != nil {
			dateStr = "hn:" + a.SourceSubmittedAt.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%2d. [%s] (%s) %s\n    %s\n", i+1, a.SourceID, dateStr, a.Title, a.CanonicalURL)
	}
	return nil
}

func newsSnapshot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("news snapshot", flag.ContinueOnError)
	dateStr := fs.String("date", "", "snapshot date YYYY-MM-DD in Europe/Moscow (default today)")
	timeout := fs.Duration("timeout", 1*time.Minute, "workflow execution timeout")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}

	wfCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	logger := observability.NewLogger("cli", cfg.App.LogLevel, os.Stderr)
	tc, err := workflows.DialTemporal(wfCtx, cfg.Temporal, logger)
	if err != nil {
		return err
	}
	defer tc.Close()

	wfID := "snapshot-" + uuid.NewString()
	we, err := tc.ExecuteWorkflow(wfCtx, client.StartWorkflowOptions{
		ID:        wfID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.DailyNewsSnapshotWorkflowName, workflows.DailyNewsSnapshotInput{
		SnapshotDate:             *dateStr,
		IncludeSubmittedFallback: true,
	})
	if err != nil {
		return fmt.Errorf("start DailyNewsSnapshotWorkflow: %w", err)
	}

	var res workflows.DailySnapshotResult
	if err := we.Get(wfCtx, &res); err != nil {
		return fmt.Errorf("DailyNewsSnapshotWorkflow %s failed: %w", we.GetID(), err)
	}

	if *jsonOut {
		return printJSON(res)
	}
	fmt.Printf("snapshot_id=%s date=%s coverage=%s articles=%d window=[%s, %s)\n",
		res.SnapshotID, res.SnapshotDate, res.CoverageStatus, res.ArticleCount,
		res.WindowStart.UTC().Format(time.RFC3339), res.WindowEnd.UTC().Format(time.RFC3339))
	for src, cnt := range res.SourceStats {
		fmt.Printf("  %-14s %d\n", src, cnt)
	}
	return nil
}

func newsStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("news stats", flag.ContinueOnError)
	last := fs.Duration("last", 24*time.Hour, "lookback duration")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	repo := postgres.NewNewsRepository(pool, cfg.Postgres.QueryTimeout)
	stats, err := repo.GetIngestionStats(ctx, time.Now().UTC().Add(-*last))
	if err != nil {
		return err
	}

	if *jsonOut {
		return printJSON(stats)
	}
	fmt.Printf("sources=%d (enabled=%d) fetched=%d inserted=%d duplicates=%d errors=%d last_24h=%d coverage=%s duration_ms=%d\n",
		stats.TotalSources, stats.EnabledSources, stats.FetchedCount, stats.InsertedCount,
		stats.DuplicateCount, stats.ErrorCount, stats.ArticlesLast24h, stats.CoverageStatus, stats.TotalDurationMS)
	for src, cnt := range stats.ArticlesBySource {
		fmt.Printf("  %-14s %d\n", src, cnt)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
