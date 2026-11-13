//go:build integration

package workflows_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

func liveConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if os.Getenv("POSTGRES_DSN") == "" && os.Getenv("POSTGRES_PASSWORD") == "" {
		cfg.Postgres.DSN = "postgres://it_news_app:it_news_local_dev@127.0.0.1:5433/it_news?sslmode=disable"
	}
	if os.Getenv("MINIO_ACCESS_KEY") == "" {
		cfg.MinIO.AccessKey = "minioadmin_dev"
	}
	if os.Getenv("MINIO_SECRET_KEY") == "" {
		cfg.MinIO.SecretKey = "minioadmin_dev_pass"
	}
	return cfg
}

func TestSmokeWorkflowLiveAndIdempotency(t *testing.T) {
	cfg := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.Postgres.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		t.Fatalf("postgres.Connect() error = %v", err)
	}
	defer pool.Close()

	s3Store, err := storage.NewS3Store(ctx, cfg.MinIO)
	if err != nil {
		t.Fatalf("NewS3Store() error = %v", err)
	}
	if err := s3Store.EnsureBucket(ctx, cfg.MinIO.Bucket); err != nil {
		t.Fatalf("EnsureBucket() error = %v", err)
	}

	tc, err := workflows.DialTemporal(ctx, cfg.Temporal, nil)
	if err != nil {
		t.Fatalf("DialTemporal() error = %v", err)
	}
	defer tc.Close()

	runID := "int-smoke-" + uuid.NewString()
	input := workflows.InfrastructureSmokeInput{
		RunID:           runID,
		TestName:        "integration-smoke",
		ArtifactContent: "smoke payload for " + runID,
	}

	we1, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "smoke-1-" + runID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.SmokeWorkflowName, input)
	if err != nil {
		t.Fatalf("ExecuteWorkflow(1) error = %v", err)
	}
	var res1 workflows.InfrastructureSmokeResult
	if err := we1.Get(ctx, &res1); err != nil {
		t.Fatalf("we1.Get() error = %v", err)
	}
	if res1.Status != "COMPLETED" || !res1.ChecksumVerified || !res1.DatabasePersisted {
		t.Fatalf("unexpected res1: %+v", res1)
	}

	// Second execution with the SAME business run_id must be idempotent.
	we2, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "smoke-2-" + runID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.SmokeWorkflowName, input)
	if err != nil {
		t.Fatalf("ExecuteWorkflow(2) error = %v", err)
	}
	var res2 workflows.InfrastructureSmokeResult
	if err := we2.Get(ctx, &res2); err != nil {
		t.Fatalf("we2.Get() error = %v", err)
	}

	var runCount, assetCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM daily_runs WHERE id = $1`, runID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_assets WHERE object_key = $1`, res1.ArtifactKey).Scan(&assetCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || assetCount != 1 {
		t.Fatalf("idempotency violated: runCount=%d assetCount=%d", runCount, assetCount)
	}

	runRepo := postgres.NewDailyRunRepository(pool, cfg.Postgres.QueryTimeout)
	rec, err := runRepo.GetByID(ctx, domain.DailyRunID(runID))
	if err != nil || rec.Run.State != domain.DailyRunCompleted {
		t.Fatalf("DailyRun state=%s err=%v", rec.Run.State, err)
	}

	// Sequential changed-payload replay with same business runID must fail with
	// non-retryable conflict BEFORE mutating MinIO object or DB metadata.
	badInput := input
	badInput.ArtifactContent = "CONFLICTING payload for " + runID
	weConflict, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "smoke-conflict-" + runID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.SmokeWorkflowName, badInput)
	if err != nil {
		t.Fatalf("ExecuteWorkflow(conflict) error = %v", err)
	}
	if err := weConflict.Get(ctx, nil); err == nil {
		t.Fatal("changed-payload replay succeeded; expected non-retryable idempotency conflict")
	}

	// Verify original MinIO bytes & SHA-256 and DB DailyRun/MediaAsset are intact.
	expectedSum := sha256.Sum256([]byte(input.ArtifactContent))
	expectedSHA := hex.EncodeToString(expectedSum[:])
	body, _, err := s3Store.GetObject(ctx, cfg.MinIO.Bucket, res1.ArtifactKey)
	if err != nil {
		t.Fatalf("GetObject after conflict error = %v", err)
	}
	gotBytes, _ := io.ReadAll(body)
	_ = body.Close()
	gotSum := sha256.Sum256(gotBytes)
	if string(gotBytes) != input.ArtifactContent || hex.EncodeToString(gotSum[:]) != expectedSHA {
		t.Fatalf("MinIO object corrupted by conflicting replay: got=%q", string(gotBytes))
	}
	recAfter, err := runRepo.GetByID(ctx, domain.DailyRunID(runID))
	if err != nil || recAfter.Run.State != domain.DailyRunCompleted || recAfter.WorkflowID != "smoke-1-"+runID {
		t.Fatalf("DailyRun mutated by conflicting replay: %+v err=%v", recAfter, err)
	}
	assetRepo := postgres.NewMediaAssetRepository(pool, cfg.Postgres.QueryTimeout)
	assetAfter, err := assetRepo.GetByObjectKey(ctx, cfg.MinIO.Bucket, res1.ArtifactKey)
	if err != nil || assetAfter.Checksum != expectedSHA {
		t.Fatalf("MediaAsset mutated by conflicting replay: %+v err=%v", assetAfter, err)
	}
}

func TestSmokeWorkflowConcurrentChangedPayloadRace(t *testing.T) {
	cfg := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.Postgres.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		t.Fatalf("postgres.Connect() error = %v", err)
	}
	defer pool.Close()

	s3Store, err := storage.NewS3Store(ctx, cfg.MinIO)
	if err != nil {
		t.Fatalf("NewS3Store() error = %v", err)
	}
	tc, err := workflows.DialTemporal(ctx, cfg.Temporal, nil)
	if err != nil {
		t.Fatalf("DialTemporal() error = %v", err)
	}
	defer tc.Close()

	runID := "int-race-" + uuid.NewString()
	payloads := []string{"race-payload-alpha-" + runID, "race-payload-beta-" + runID}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	results := make([]workflows.InfrastructureSmokeResult, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			we, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID:        "smoke-race-" + uuid.NewString(),
				TaskQueue: cfg.Temporal.TaskQueue,
			}, workflows.SmokeWorkflowName, workflows.InfrastructureSmokeInput{
				RunID:           runID,
				TestName:        "concurrent-race",
				ArtifactContent: payloads[idx],
			})
			if err != nil {
				errs[idx] = err
				return
			}
			errs[idx] = we.Get(ctx, &results[idx])
		}(i)
	}
	close(start)
	wg.Wait()

	var successes, failures int
	var winnerIdx int
	for i, err := range errs {
		if err == nil {
			successes++
			winnerIdx = i
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("expected exactly 1 winner and 1 conflict loser; got successes=%d failures=%d errs=%v", successes, failures, errs)
	}

	// Verify MinIO bytes match the winner's payload and MediaAsset matches.
	body, _, err := s3Store.GetObject(ctx, cfg.MinIO.Bucket, results[winnerIdx].ArtifactKey)
	if err != nil {
		t.Fatalf("GetObject(winner) error = %v", err)
	}
	gotBytes, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(gotBytes, []byte(payloads[winnerIdx])) {
		t.Fatalf("MinIO bytes=%q, want winner=%q", string(gotBytes), payloads[winnerIdx])
	}
}
