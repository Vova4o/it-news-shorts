package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/editor"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

const editorUsage = `usage: cli editor <command> [flags]

commands:
  run       run DailyEditorialWorkflow (or --dry-run) for a snapshot date
  top       show selected TOP-3 verified stories for a date
  explain   explain clustering, claims, verification, and score for a story
  stats     show editorial run and LLM cost statistics for a date
`

func runEditor(args []string) {
	if len(args) == 0 {
		fmt.Print(editorUsage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "run":
		runEditorRun(ctx, args[1:])
	case "top":
		runEditorTop(ctx, args[1:])
	case "explain":
		runEditorExplain(ctx, args[1:])
	case "stats":
		runEditorStats(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Print(editorUsage)
	default:
		fmt.Fprintf(os.Stderr, "unknown editor command %q\n%s", args[0], editorUsage)
		os.Exit(1)
	}
}

func parseDateFlag(raw string) time.Time {
	if strings.TrimSpace(raw) == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(raw))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --date %q (expected YYYY-MM-DD): %v\n", raw, err)
		os.Exit(1)
	}
	return t.UTC()
}

func runEditorRun(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("editor run", flag.ExitOnError)
	dateStr := fs.String("date", "", "target snapshot date YYYY-MM-DD (default today UTC)")
	dryRun := fs.Bool("dry-run", false, "run without writing results to PostgreSQL")
	asJSON := fs.Bool("json", false, "output JSON")
	_ = fs.Parse(args)

	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	targetDate := parseDateFlag(*dateStr)
	logger := observability.NewLogger(cfg.App.LogLevel, "cli", os.Stderr)

	// If --dry-run and no live LLM API key is configured, execute an offline
	// dry-run against the real PostgreSQL snapshot without calling an external LLM
	// or mutating the database.
	if *dryRun && cfg.LLM.APIKey == "" {
		report, err := runOfflineDryRun(ctx, cfg, targetDate, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "offline dry-run failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(report)
			return
		}
		fmt.Fprintf(os.Stderr, "NOTE: LLM_API_KEY is not set; executed offline dry-run against real PostgreSQL snapshot (no live LLM calls, no DB writes).\n")
		printEditorialReport(report)
		return
	}

	if cfg.LLM.APIKey == "" {
		fmt.Fprintln(os.Stderr, "error: LLM_API_KEY is required for live editorial runs (use --dry-run for offline snapshot pipeline check)")
		os.Exit(1)
	}

	tc, err := workflows.DialTemporal(ctx, cfg.Temporal, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial temporal failed: %v\n", err)
		os.Exit(1)
	}
	defer tc.Close()

	wfID := fmt.Sprintf("daily-editorial-%s-%s", targetDate.Format("2006-01-02"), uuid.NewString()[:8])
	we, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        wfID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.DailyEditorialWorkflowName, workflows.DailyEditorialInput{
		Date:   targetDate,
		DryRun: *dryRun,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start DailyEditorialWorkflow failed: %v\n", err)
		os.Exit(1)
	}

	var report editor.EditorialReport
	if err := we.Get(ctx, &report); err != nil {
		fmt.Fprintf(os.Stderr, "DailyEditorialWorkflow failed: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	printEditorialReport(report)
}

func runOfflineDryRun(ctx context.Context, cfg config.Config, targetDate time.Time, _ any) (editor.EditorialReport, error) {
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		return editor.EditorialReport{}, err
	}
	defer pool.Close()

	prompts, err := ai.LoadEmbeddedPrompts()
	if err != nil {
		return editor.EditorialReport{}, err
	}

	storyIDRe := regexp.MustCompile(`story-[a-f0-9]+`)
	claimIDRe := regexp.MustCompile(`story-[a-f0-9]+-claim-\d+`)
	titleRe := regexp.MustCompile(`(?m)^title:\s*(.+)$`)
	urlRe := regexp.MustCompile(`(?m)^url:\s*(https?://\S+)$`)

	mockLLM := ai.NewMockLLMProvider("dry-run-offline", nil, func(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
		switch req.Operation {
		case "clustering":
			raw, _ := json.Marshal(map[string]any{
				"same_event":      false,
				"relationship":    "DIFFERENT_EVENT",
				"confidence":      0.9,
				"reason":          "Dry-run offline mode keeps distinct articles unmerged unless identical",
				"canonical_title": "Dry-run story",
				"category":        "OTHER_IT",
			})
			return ai.StructuredResponse{RequestID: "dry-cluster", Model: "dry-run-offline", PromptVersion: req.PromptVersion, RawJSON: raw}, nil
		case "fact-extraction":
			sid := storyIDRe.FindString(req.UserPrompt)
			if sid == "" {
				sid = "story-dry"
			}
			title := "Dry-run article"
			if m := titleRe.FindStringSubmatch(req.UserPrompt); len(m) > 1 {
				title = strings.TrimSpace(m[1])
			}
			artRe := regexp.MustCompile(`(?m)^article_id:\s*(\S+)$`)
			var artIDs []string
			for _, m := range artRe.FindAllStringSubmatch(req.UserPrompt, -1) {
				if len(m) > 1 {
					artIDs = append(artIDs, m[1])
				}
			}
			var claims []map[string]any
			if len(artIDs) > 0 && title != "" {
				claims = append(claims, map[string]any{
					"text":               title,
					"type":               "EVENT",
					"source_article_ids": artIDs,
					"entities":           []string{},
					"numbers":            []string{},
					"evidence_quote":     title,
					"confidence":         0.85,
				})
			}
			raw, _ := json.Marshal(map[string]any{"story_id": sid, "claims": claims})
			return ai.StructuredResponse{RequestID: "dry-extract", Model: "dry-run-offline", PromptVersion: req.PromptVersion, RawJSON: raw}, nil
		case "fact-verification":
			sid := storyIDRe.FindString(req.UserPrompt)
			if sid == "" {
				sid = "story-dry"
			}
			var urls []string
			for _, m := range urlRe.FindAllStringSubmatch(req.UserPrompt, -1) {
				if len(m) > 1 {
					urls = append(urls, m[1])
				}
			}
			var verdicts []map[string]any
			for _, cid := range claimIDRe.FindAllString(req.UserPrompt, -1) {
				verdicts = append(verdicts, map[string]any{
					"claim_id":           cid,
					"status":             "UNVERIFIED",
					"reason":             "Offline dry-run without live LLM verification",
					"cited_urls":         urls,
					"contradiction_note": "",
				})
			}
			raw, _ := json.Marshal(map[string]any{"story_id": sid, "verdicts": verdicts})
			return ai.StructuredResponse{RequestID: "dry-verify", Model: "dry-run-offline", PromptVersion: req.PromptVersion, RawJSON: raw}, nil
		case "scoring":
			sid := storyIDRe.FindString(req.UserPrompt)
			if sid == "" {
				sid = "story-dry"
			}
			raw, _ := json.Marshal(map[string]any{
				"story_id":          sid,
				"category":          "OTHER_IT",
				"impact":            50,
				"interest":          50,
				"freshness":         50,
				"novelty":           50,
				"reasoning_summary": "Offline dry-run placeholder",
			})
			return ai.StructuredResponse{RequestID: "dry-score", Model: "dry-run-offline", PromptVersion: req.PromptVersion, RawJSON: raw}, nil
		}
		return ai.StructuredResponse{}, fmt.Errorf("unexpected op %s", req.Operation)
	})

	acts := &workflows.EditorialActivities{
		NewsRepo:     postgres.NewNewsRepository(pool, cfg.Postgres.QueryTimeout),
		EditorRepo:   postgres.NewEditorRepository(pool, cfg.Postgres.QueryTimeout),
		LLM:          mockLLM,
		Prompts:      prompts,
		LLMConfig:    config.LLMConfig{Model: "dry-run-offline"},
		EditorConfig: cfg.Editor,
	}

	started := time.Now().UTC()
	runID := "dryrun-" + targetDate.Format("2006-01-02")
	snap, err := acts.LoadDailySnapshot(ctx, workflows.LoadDailySnapshotInput{RunID: runID, Date: targetDate, DryRun: true})
	if err != nil {
		return editor.EditorialReport{}, err
	}
	prep, err := acts.PreprocessArticles(ctx, workflows.PreprocessArticlesInput{
		Articles:    snap.Articles,
		WindowStart: snap.WindowStart,
		WindowEnd:   snap.WindowEnd,
	})
	if err != nil {
		return editor.EditorialReport{}, err
	}
	clust, err := acts.ClusterStories(ctx, workflows.ClusterStoriesInput{RunID: runID, Articles: prep.Articles, Now: started})
	if err != nil {
		return editor.EditorialReport{}, err
	}
	ext, err := acts.ExtractClaims(ctx, workflows.ExtractClaimsInput{RunID: runID, Stories: clust.Stories})
	if err != nil {
		return editor.EditorialReport{}, err
	}
	ver, err := acts.VerifyClaims(ctx, workflows.VerifyClaimsInput{RunID: runID, Stories: ext.Stories, Now: started})
	if err != nil {
		return editor.EditorialReport{}, err
	}
	filt, err := acts.FilterEligibleStories(ctx, workflows.FilterEligibleStoriesInput{Stories: ver.Stories})
	if err != nil {
		return editor.EditorialReport{}, err
	}
	var scored workflows.ScoreStoriesOutput
	if len(filt.Eligible) > 0 {
		scored, err = acts.ScoreStories(ctx, workflows.ScoreStoriesInput{RunID: runID, Stories: filt.Eligible, Now: started})
		if err != nil {
			return editor.EditorialReport{}, err
		}
	}
	sel, err := acts.SelectTopStories(ctx, workflows.SelectTopStoriesInput{
		Candidates:           scored.Stories,
		PreviousFingerprints: snap.PreviousFingerprints,
	})
	if err != nil {
		return editor.EditorialReport{}, err
	}
	var allUsage []editor.LLMUsageRecord
	allUsage = append(allUsage, clust.Usage...)
	allUsage = append(allUsage, ext.Usage...)
	allUsage = append(allUsage, ver.Usage...)
	allUsage = append(allUsage, scored.Usage...)

	return acts.GenerateEditorialReport(ctx, workflows.GenerateEditorialReportInput{
		Date:              targetDate,
		RunID:             runID,
		SnapshotID:        snap.SnapshotID,
		WorkflowID:        "cli-dry-run",
		DryRun:            true,
		ArticlesProcessed: len(prep.Articles),
		TotalStories:      len(clust.Stories),
		VerifiedStories:   len(filt.Eligible),
		RejectedStories:   len(filt.Rejected),
		Selection:         sel,
		Usage:             allUsage,
		DurationMS:        time.Since(started).Milliseconds(),
	})
}

func runEditorTop(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("editor top", flag.ExitOnError)
	dateStr := fs.String("date", "", "target date YYYY-MM-DD (default today UTC)")
	asJSON := fs.Bool("json", false, "output JSON")
	_ = fs.Parse(args)

	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect postgres failed: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	repo := postgres.NewEditorRepository(pool, cfg.Postgres.QueryTimeout)
	view, err := repo.GetDailySelection(ctx, parseDateFlag(*dateStr))
	if err != nil {
		if errors.Is(err, postgres.ErrEditorialNotFound) {
			fmt.Println("no daily selection found for date")
			return
		}
		fmt.Fprintf(os.Stderr, "get daily selection failed: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(view)
		return
	}
	fmt.Printf("Date: %s | Selection: %s | Run: %s | Snapshot: %s\n", view.Date, view.SelectionID, view.RunID, view.SnapshotID)
	fmt.Printf("Summary: %s\n", view.Summary)
	for _, it := range view.Items {
		fmt.Printf("#%d [%.2f] [%s] %s (%s)\n   reason: %s\n", it.Rank, it.Score, it.Category, it.Title, it.StoryID, it.SelectionReason)
	}
}

func runEditorExplain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("editor explain", flag.ExitOnError)
	storyID := fs.String("story-id", "", "story ID to explain")
	asJSON := fs.Bool("json", false, "output JSON")
	_ = fs.Parse(args)

	if strings.TrimSpace(*storyID) == "" {
		fmt.Fprintln(os.Stderr, "error: --story-id is required")
		os.Exit(1)
	}
	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect postgres failed: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	repo := postgres.NewEditorRepository(pool, cfg.Postgres.QueryTimeout)
	view, err := repo.ExplainStory(ctx, *storyID)
	if err != nil {
		if errors.Is(err, postgres.ErrEditorialNotFound) {
			fmt.Fprintf(os.Stderr, "story %q not found\n", *storyID)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "explain story failed: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(view)
		return
	}
	fmt.Printf("Story: %s\nTitle: %s\nCategory: %s | Verification: %s\nCluster Confidence: %.2f | Reason: %s\n",
		view.StoryID, view.Title, view.Category, view.VerificationStatus, view.ClusterConfidence, view.ClusterReason)
	if view.RejectionReason != "" {
		fmt.Printf("Rejection Reason: %s\n", view.RejectionReason)
	}
	if view.Score != nil {
		fmt.Printf("Score: %.2f (Impact=%.1f Interest=%.1f Freshness=%.1f Novelty=%.1f)\nReasoning: %s\n",
			view.Score.FinalScore, view.Score.Impact, view.Score.Interest, view.Score.Freshness, view.Score.Novelty, view.Score.ReasoningSummary)
	}
	fmt.Printf("Articles (%d): %v\n", len(view.ArticleIDs), view.ArticleIDs)
	fmt.Printf("Claims (%d):\n", len(view.Claims))
	for _, cl := range view.Claims {
		fmt.Printf("  - [%s] (%s) %s\n", cl.VerificationStatus, cl.Type, cl.Text)
	}
}

func runEditorStats(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("editor stats", flag.ExitOnError)
	dateStr := fs.String("date", "", "target date YYYY-MM-DD (default today UTC)")
	asJSON := fs.Bool("json", false, "output JSON")
	_ = fs.Parse(args)

	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect postgres failed: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	repo := postgres.NewEditorRepository(pool, cfg.Postgres.QueryTimeout)
	stats, err := repo.GetEditorialStats(ctx, parseDateFlag(*dateStr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "get editorial stats failed: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(stats)
		return
	}
	fmt.Printf("Date: %s | Runs: %d\nArticles: %d | Stories: %d (Verified: %d, Rejected: %d) | Selected: %d\nLLM Requests: %d | Tokens: %d in / %d out | Total Cost: $%.6f\n",
		stats.Date, stats.Runs, stats.TotalArticles, stats.TotalStories, stats.VerifiedStories,
		stats.RejectedStories, stats.SelectedCount, stats.LLMRequests, stats.InputTokens, stats.OutputTokens, stats.TotalCostUSD)
}

func printEditorialReport(r editor.EditorialReport) {
	fmt.Printf("Editorial Run: %s | Date: %s | Snapshot: %s | Status: %s\n", r.EditorialRunID, r.Date, r.SnapshotID, r.Status)
	fmt.Printf("Articles: %d | Unique Stories: %d | Verified: %d | Rejected: %d | Selected: %d\n",
		r.ArticlesProcessed, r.UniqueStories, r.VerifiedStories, r.RejectedStories, len(r.Selected))
	fmt.Printf("LLM Requests: %d | Tokens: %d in / %d out | Cost: $%.6f | Duration: %dms\n",
		r.LLMRequests, r.InputTokens, r.OutputTokens, r.TotalCostUSD, r.DurationMS)
	for _, it := range r.Selected {
		fmt.Printf("  #%d [%.2f] [%s] %s (%s)\n", it.Rank, it.Score, it.Category, it.Title, it.StoryID)
	}
}
