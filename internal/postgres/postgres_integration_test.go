//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/youtube"
)

func testPGConfig(t *testing.T) config.PostgresConfig {
	t.Helper()
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if os.Getenv("POSTGRES_DSN") == "" && os.Getenv("POSTGRES_PASSWORD") == "" {
		cfg.Postgres.DSN = fmt.Sprintf(
			"postgres://%s:%s@%s:%s/%s?sslmode=disable",
			"it_news_app", "it_news_local_dev", "127.0.0.1", "5433", "it_news",
		)
	}
	return cfg.Postgres
}

func TestMigrationsUpAndIdempotent(t *testing.T) {
	cfg := testPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("first MigrateUp() error = %v", err)
	}
	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("second MigrateUp() error = %v", err)
	}

	pool, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	var count int
	err = pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.tables
		WHERE table_schema = 'public'
		  AND table_name IN (
			'sources', 'articles', 'stories', 'story_evidence',
			'daily_runs', 'video_jobs', 'scripts', 'media_assets',
			'publications', 'generation_costs', 'audit_events', 'youtube_upload_journal'
		  )
	`).Scan(&count)
	if err != nil {
		t.Fatalf("query tables error = %v", err)
	}
	if count != 12 {
		t.Fatalf("table count = %d, want 12", count)
	}
}

func TestConnectionError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := postgres.Connect(ctx, config.PostgresConfig{
		DSN:          "postgres://invalid:invalid@127.0.0.1:59999/nonexistent?sslmode=disable",
		MaxConns:     2,
		QueryTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

func TestRepositoriesCRUDConcurrencyAndConstraints(t *testing.T) {
	cfg := testPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	runRepo := postgres.NewDailyRunRepository(pool, cfg.QueryTimeout)
	storyRepo := postgres.NewStoryRepository(pool, cfg.QueryTimeout)
	jobRepo := postgres.NewVideoJobRepository(pool, cfg.QueryTimeout)
	assetRepo := postgres.NewMediaAssetRepository(pool, cfg.QueryTimeout)
	pubRepo := postgres.NewPublicationRepository(pool, cfg.QueryTimeout)

	suffix := uuid.NewString()[:8]
	now := time.Now().UTC().Truncate(time.Microsecond)
	runID := domain.DailyRunID("int-run-" + suffix)
	winEnd := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	run := domain.DailyRun{
		ID:         runID,
		RunDate:    winEnd,
		Window:     domain.TimeWindow{Start: winEnd.Add(-24 * time.Hour), End: winEnd},
		State:      domain.DailyRunRunning,
		MaxStories: 2,
		CreatedAt:  now,
	}

	rec, created, err := runRepo.CreateIdempotent(ctx, run, "wf-"+suffix)
	if err != nil || !created {
		t.Fatalf("CreateIdempotent() created=%v err=%v", created, err)
	}
	if rec.Run.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt location = %v, want UTC", rec.Run.CreatedAt.Location())
	}

	// Idempotent repeat.
	_, created2, err := runRepo.CreateIdempotent(ctx, run, "wf-"+suffix)
	if err != nil || created2 {
		t.Fatalf("repeat CreateIdempotent() created=%v err=%v", created2, err)
	}

	// Story & VideoJob.
	storyID := domain.StoryID("int-story-" + suffix)
	if err := storyRepo.EnsureSmokeStory(ctx, domain.Story{
		ID:         storyID,
		DailyRunID: runID,
		Title:      "Integration Story",
		Summary:    "Summary",
		Score:      0.9,
		Position:   1,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("EnsureSmokeStory() error = %v", err)
	}

	jobID := domain.VideoJobID("int-job-" + suffix)
	job, err := domain.NewVideoJob(domain.NewVideoJobParams{
		ID:             jobID,
		DailyRunID:     runID,
		StoryID:        storyID,
		IdempotencyKey: domain.IdempotencyKey("int/" + suffix + "/v1"),
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("NewVideoJob() error = %v", err)
	}
	storedJob, createdJob, err := jobRepo.CreateIdempotent(ctx, job, "wf-"+suffix)
	if err != nil || !createdJob {
		t.Fatalf("VideoJob CreateIdempotent() created=%v err=%v", createdJob, err)
	}

	// Invalid transition must fail and roll back.
	if _, err := jobRepo.Transition(ctx, storedJob.ID(), domain.StatePublished, now.Add(time.Second), nil); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("invalid Transition() error = %v, want ErrInvalidTransition", err)
	}

	// Real concurrent optimistic concurrency test with barrier: 2 goroutines race
	// to transition the same VideoJob from ver1 -> SCRIPTED; 1 wins, 1 loses.
	_, ver1, err := jobRepo.GetByID(ctx, jobID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	barrier := make(chan struct{})
	var raceWG sync.WaitGroup
	raceErrs := make([]error, 2)
	for i := 0; i < 2; i++ {
		raceWG.Add(1)
		go func(idx int) {
			defer raceWG.Done()
			<-barrier
			_, raceErrs[idx] = jobRepo.TransitionWithExpectedVersion(ctx, jobID, ver1, domain.StateScripted, now.Add(2*time.Second), nil)
		}(i)
	}
	close(barrier)
	raceWG.Wait()

	var wins, conflicts int
	for _, rErr := range raceErrs {
		switch {
		case rErr == nil:
			wins++
		case errors.Is(rErr, postgres.ErrConcurrentModification):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent transition error: %v", rErr)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected 1 winner and 1 ErrConcurrentModification, got wins=%d conflicts=%d errs=%v", wins, conflicts, raceErrs)
	}
	afterRace, ver2, err := jobRepo.GetByID(ctx, jobID)
	if err != nil || ver2 != ver1+1 || afterRace.State() != domain.StateScripted {
		t.Fatalf("post-race job state=%s ver=%d (want SCRIPTED, ver=%d) err=%v", afterRace.State(), ver2, ver1+1, err)
	}

	// MediaAsset CRUD.
	asset, createdAsset, err := assetRepo.CreateIdempotent(ctx, domain.MediaAsset{
		ID:         domain.MediaAssetID("int-asset-" + suffix),
		VideoJobID: jobID,
		Kind:       domain.MediaRendered,
		URI:        "s3://it-news-media/test/" + suffix + ".mp4",
		MIMEType:   "video/mp4",
		SizeBytes:  1024,
		Checksum:   "abc123",
		Provider:   "minio",
		CreatedAt:  now,
	}, "it-news-media", "test/"+suffix+".mp4")
	if err != nil || !createdAsset || asset.SizeBytes != 1024 {
		t.Fatalf("MediaAsset CreateIdempotent() created=%v err=%v", createdAsset, err)
	}

	// Foreign key violation on Publication with non-existent VideoJobID.
	_, _, err = pubRepo.CreateIdempotent(ctx, domain.Publication{
		ID:             domain.PublicationID("pub-fk-" + suffix),
		VideoJobID:     domain.VideoJobID("nonexistent-job"),
		Status:         domain.PublicationPending,
		IdempotencyKey: domain.IdempotencyKey("pub-fk/" + suffix),
		CreatedAt:      now,
	}, "youtube")
	if !errors.Is(err, postgres.ErrForeignKey) {
		t.Fatalf("FK violation error = %v, want ErrForeignKey", err)
	}

	// Duplicate publication for same video_job_id with different key must conflict.
	pub1 := domain.Publication{
		ID:             domain.PublicationID("pub-1-" + suffix),
		VideoJobID:     jobID,
		YouTubeVideoID: "yt-1-" + suffix,
		Status:         domain.PublicationPending,
		IdempotencyKey: domain.IdempotencyKey("pub-1/" + suffix),
		CreatedAt:      now,
	}
	if _, createdPub, err := pubRepo.CreateIdempotent(ctx, pub1, "youtube"); err != nil || !createdPub {
		t.Fatalf("Publication CreateIdempotent() created=%v err=%v", createdPub, err)
	}
	pub2 := domain.Publication{
		ID:             domain.PublicationID("pub-2-" + suffix),
		VideoJobID:     jobID,
		YouTubeVideoID: "yt-2-" + suffix,
		Status:         domain.PublicationPending,
		IdempotencyKey: domain.IdempotencyKey("pub-2/" + suffix),
		CreatedAt:      now,
	}
	if _, _, err := pubRepo.CreateIdempotent(ctx, pub2, "youtube"); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("duplicate publication error = %v, want ErrConflict", err)
	}

	// Complete DailyRun.
	completed, err := runRepo.UpdateStatus(ctx, runID, domain.DailyRunCompleted, "wf-"+suffix, "", now.Add(5*time.Second))
	if err != nil || completed.Run.State != domain.DailyRunCompleted {
		t.Fatalf("UpdateStatus(COMPLETED) state=%s err=%v", completed.Run.State, err)
	}
}

func TestPGJournalNoPoolStarvationAndMigration(t *testing.T) {
	cfg := testPGConfig(t)
	cfg.MaxConns = 2 // Intentionally tiny pool to prove lock holders never starve queries.
	cfg.MinConns = 1

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	pgJournal := postgres.NewPGJournal(pool, cfg.QueryTimeout)
	suffix := uuid.NewString()[:8]

	// Hold 2 advisory locks concurrently (equal to MaxConns=2!) while performing Get/Put.
	rel1, err := pgJournal.AcquireKey(ctx, "lock-a-"+suffix)
	if err != nil {
		t.Fatalf("AcquireKey(a) error = %v", err)
	}
	defer rel1()

	rel2, err := pgJournal.AcquireKey(ctx, "lock-b-"+suffix)
	if err != nil {
		t.Fatalf("AcquireKey(b) error = %v", err)
	}
	defer rel2()

	if err := pgJournal.Put(ctx, youtube.JournalEntry{
		Key:   "lock-c-" + suffix,
		State: youtube.UploadStatePending,
	}); err != nil {
		t.Fatalf("Put() while locks held failed (pool starvation!): %v", err)
	}
	if _, found, err := pgJournal.Get(ctx, "lock-c-"+suffix); err != nil || !found {
		t.Fatalf("Get() while locks held failed: found=%v err=%v", found, err)
	}

	// Context cancellation while waiting for a held lock.
	waitCtx, waitCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer waitCancel()
	if _, err := pgJournal.AcquireKey(waitCtx, "lock-a-"+suffix); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireKey() on held lock error = %v, want DeadlineExceeded", err)
	}

	// File journal -> PostgreSQL migration preserving UNKNOWN & IN_PROGRESS.
	dir := t.TempDir()
	fileJournal, err := youtube.OpenJournal(filepath.Join(dir, "uploads.json"))
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	fixtures := []youtube.JournalEntry{
		{
			Key:        "mig-completed-" + suffix,
			State:      youtube.UploadStateCompleted,
			VideoID:    "yt-done-" + suffix,
			FileSHA256: strings.Repeat("a", 64),
			TotalBytes: 100,
			SentBytes:  100,
			UploadedAt: now,
		},
		{
			Key:        "mig-unknown-" + suffix,
			State:      youtube.UploadStateUnknown,
			SessionURI: "https://upload.example.com/sess-unknown",
			FileSHA256: strings.Repeat("b", 64),
			TotalBytes: 200,
			SentBytes:  100,
			Error:      "ambiguous network drop",
		},
		{
			Key:        "mig-inprog-" + suffix,
			State:      youtube.UploadStateInProgress,
			SessionURI: "https://upload.example.com/sess-inprog",
			FileSHA256: strings.Repeat("c", 64),
			TotalBytes: 300,
			SentBytes:  150,
		},
	}
	for _, e := range fixtures {
		if err := fileJournal.Put(ctx, e); err != nil {
			t.Fatalf("fileJournal.Put() error = %v", err)
		}
	}

	rep1, err := postgres.MigrateFileJournalToPG(ctx, fileJournal, pgJournal)
	if err != nil {
		t.Fatalf("MigrateFileJournalToPG() error = %v", err)
	}
	if rep1.Imported != 3 {
		t.Fatalf("Imported = %d, want 3", rep1.Imported)
	}

	// Idempotent re-run.
	rep2, err := postgres.MigrateFileJournalToPG(ctx, fileJournal, pgJournal)
	if err != nil {
		t.Fatalf("second MigrateFileJournalToPG() error = %v", err)
	}
	if rep2.Imported != 0 || rep2.Skipped != 3 {
		t.Fatalf("second migration = %+v, want Imported=0 Skipped=3", rep2)
	}

	// Verify UNKNOWN entry in PG never triggers a new videos.insert.
	var insertCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		insertCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tmpVideo := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(tmpVideo, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	ytClient := youtube.NewClient(nil,
		youtube.WithHTTPClient(srv.Client()),
		youtube.WithBaseURL(srv.URL),
		youtube.WithUploadURL(srv.URL),
		youtube.WithJournal(pgJournal),
	)
	_, upErr := ytClient.UploadVideo(ctx, youtube.UploadInput{
		FilePath:       tmpVideo,
		Title:          "Unknown Check",
		Privacy:        youtube.PrivacyPrivate,
		IdempotencyKey: "mig-unknown-" + suffix,
	})
	if upErr == nil {
		t.Fatal("expected error for UNKNOWN upload state, got nil")
	}
	if insertCalled {
		t.Fatal("UNKNOWN state triggered a new HTTP upload request")
	}
	_ = sync.Mutex{}
}

func TestBackupAndRestoreToSeparateTestDB(t *testing.T) {
	// Ensure no stale sync.Mutex dummy exists; real concurrency is tested above
	// and below.
	// Backup & restore test into a separate test database (never over live DB).
	composeProject := os.Getenv("COMPOSE_PROJECT_NAME")
	if composeProject == "" {
		composeProject = "it-news-shorts"
	}
	adminUser := os.Getenv("POSTGRES_ADMIN_USER")
	if adminUser == "" {
		adminUser = "postgres"
	}
	appUser := os.Getenv("POSTGRES_USER")
	if appUser == "" {
		appUser = "it_news_app"
	}
	dbName := os.Getenv("POSTGRES_DB")
	if dbName == "" {
		dbName = "it_news"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("IT_NEWS_ALLOW_SKIP_BACKUP") == "1" {
			t.Skip("docker not available for pg_dump/psql backup test")
		}
		t.Fatalf("docker CLI required for mandatory backup/restore test: %v", err)
	}
	cfg := testPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	// Ensure at least one DailyRun exists before backup.
	runRepo := postgres.NewDailyRunRepository(pool, cfg.QueryTimeout)
	suffix := uuid.NewString()[:8]
	runID := domain.DailyRunID("backup-run-" + suffix)
	winEnd := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if _, _, err := runRepo.CreateIdempotent(ctx, domain.DailyRun{
		ID:         runID,
		RunDate:    winEnd,
		Window:     domain.TimeWindow{Start: winEnd.Add(-24 * time.Hour), End: winEnd},
		State:      domain.DailyRunCompleted,
		MaxStories: 1,
		CreatedAt:  time.Now().UTC(),
	}, "wf-backup-"+suffix); err != nil {
		t.Fatalf("CreateIdempotent() error = %v", err)
	}

	composeFile := filepath.Join("..", "..", "deployments", "docker-compose.yml")
	dumpFile := filepath.Join(t.TempDir(), "backup.sql")

	dumpCmd := exec.CommandContext(ctx, "docker", "compose", "-p", composeProject, "-f", composeFile, "exec", "-T", "postgres",
		"pg_dump", "-U", adminUser, "--clean", "--if-exists", dbName)
	dumpOut, err := dumpCmd.Output()
	if err != nil {
		if os.Getenv("IT_NEWS_ALLOW_SKIP_BACKUP") == "1" {
			t.Skipf("compose postgres container not running for backup test: %v", err)
		}
		t.Fatalf("pg_dump via compose project %q failed: %v", composeProject, err)
	}
	if err := os.WriteFile(dumpFile, dumpOut, 0o600); err != nil {
		t.Fatal(err)
	}

	restoreDB := "it_news_restore_test_" + suffix
	createCmd := exec.CommandContext(ctx, "docker", "compose", "-p", composeProject, "-f", composeFile, "exec", "-T", "postgres",
		"psql", "-U", adminUser, "-d", "postgres", "-c", fmt.Sprintf("CREATE DATABASE %s OWNER %s;", restoreDB, appUser))
	if out, err := createCmd.CombinedOutput(); err != nil {
		t.Fatalf("create restore test db failed: %v (%s)", err, string(out))
	}
	defer func() {
		dropCmd := exec.Command(
			"docker", "compose", "-p", composeProject, "-f", composeFile, "exec", "-T", "postgres",
			"psql", "-U", adminUser, "-d", "postgres", "-c", fmt.Sprintf("DROP DATABASE IF EXISTS %s;", restoreDB),
		)
		_ = dropCmd.Run()
	}()

	restoreCmd := exec.CommandContext(ctx, "docker", "compose", "-p", composeProject, "-f", composeFile, "exec", "-T", "postgres",
		"psql", "-U", adminUser, "-v", "ON_ERROR_STOP=1", "-d", restoreDB)
	f, err := os.Open(dumpFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	restoreCmd.Stdin = f
	if out, err := restoreCmd.CombinedOutput(); err != nil {
		t.Fatalf("restore into %s failed: %v (%s)", restoreDB, err, string(out))
	}

	verifyCmd := exec.CommandContext(ctx, "docker", "compose", "-p", composeProject, "-f", composeFile, "exec", "-T", "postgres",
		"psql", "-U", appUser, "-d", restoreDB, "-tAc",
		fmt.Sprintf("SELECT status FROM daily_runs WHERE id = '%s';", runID))
	gotStatus, err := verifyCmd.Output()
	if err != nil {
		t.Fatalf("verify restored row failed: %v", err)
	}
	if string(bytesTrimSpace(gotStatus)) != "COMPLETED" {
		t.Fatalf("restored status = %q, want COMPLETED", string(gotStatus))
	}
}

func TestPGJournalFencingOnLockConnectionLoss(t *testing.T) {
	cfg := testPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	j1 := postgres.NewPGJournal(pool, cfg.QueryTimeout)
	j2 := postgres.NewPGJournal(pool, cfg.QueryTimeout)
	key := "fencing-" + uuid.NewString()
	t.Cleanup(func() {
		cCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = pool.Exec(cCtx, `DELETE FROM youtube_upload_journal WHERE idempotency_key = $1`, key)
		_, _ = pool.Exec(cCtx, `DELETE FROM youtube_journal_locks WHERE idempotency_key = $1`, key)
	})

	release1, err := j1.AcquireKey(ctx, key)
	if err != nil {
		t.Fatalf("j1.AcquireKey() error = %v", err)
	}
	defer release1()

	if err := j1.Put(ctx, youtube.JournalEntry{
		Key:        key,
		State:      youtube.UploadStateInProgress,
		SessionURI: "https://upload.example.com/sess-1",
		FileSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatalf("j1.Put(initial) error = %v", err)
	}

	// Terminate the advisory lock connection held by j1 inside PostgreSQL.
	var terminated int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT pg_terminate_backend(pid)
			FROM pg_locks
			WHERE locktype = 'advisory'
			  AND pid <> pg_backend_pid()
			  AND (classid::bigint << 32 | objid::bigint) = hashtextextended($1, 0)
		) t
	`, key).Scan(&terminated); err != nil {
		t.Fatalf("terminate advisory lock backend error = %v", err)
	}
	if terminated == 0 {
		t.Fatal("expected at least 1 advisory lock backend to terminate")
	}

	// New owner j2 acquires the lock and transitions the entry to COMPLETED.
	release2, err := j2.AcquireKey(ctx, key)
	if err != nil {
		t.Fatalf("j2.AcquireKey() error = %v", err)
	}
	if err := j2.Put(ctx, youtube.JournalEntry{
		Key:        key,
		State:      youtube.UploadStateCompleted,
		VideoID:    "yt-winner-" + key,
		FileSHA256: strings.Repeat("a", 64),
	}); err != nil {
		release2()
		t.Fatalf("j2.Put(winner) error = %v", err)
	}
	release2()

	// Stale owner j1 must fail closed with ErrLockLost and MUST NOT overwrite COMPLETED.
	err = j1.Put(ctx, youtube.JournalEntry{
		Key:        key,
		State:      youtube.UploadStateFailed,
		Error:      "stale overwrite attempt",
		FileSHA256: strings.Repeat("a", 64),
	})
	if !errors.Is(err, postgres.ErrLockLost) {
		t.Fatalf("stale j1.Put() error = %v, want ErrLockLost", err)
	}

	got, ok, err := j2.Get(ctx, key)
	if err != nil || !ok || got.State != youtube.UploadStateCompleted || got.VideoID != "yt-winner-"+key {
		t.Fatalf("winner state corrupted: ok=%v got=%+v err=%v", ok, got, err)
	}
}

func TestPGJournalInvariantsAndSQLConstraints(t *testing.T) {
	cfg := testPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	journal := postgres.NewPGJournal(pool, cfg.QueryTimeout)
	key := "inv-" + uuid.NewString()
	t.Cleanup(func() {
		cCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = pool.Exec(cCtx, `DELETE FROM youtube_upload_journal WHERE idempotency_key = $1`, key)
	})

	if err := journal.Put(ctx, youtube.JournalEntry{Key: key, State: youtube.UploadStateCompleted}); err == nil {
		t.Fatal("PGJournal.Put accepted COMPLETED without VideoID")
	}
	if err := journal.Put(ctx, youtube.JournalEntry{Key: key, State: youtube.UploadStateInProgress}); err == nil {
		t.Fatal("PGJournal.Put accepted IN_PROGRESS without SessionURI")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO youtube_upload_journal (idempotency_key, state) VALUES ($1, 'IN_PROGRESS')`, key); err == nil {
		t.Fatal("SQL CHECK constraint allowed IN_PROGRESS without session_uri")
	}
}

func TestMigrationsUpDownUpInSeparateEmptyDB(t *testing.T) {
	composeProject := os.Getenv("COMPOSE_PROJECT_NAME")
	if composeProject == "" {
		composeProject = "it-news-shorts"
	}
	adminUser := os.Getenv("POSTGRES_ADMIN_USER")
	if adminUser == "" {
		adminUser = "postgres"
	}
	appUser := os.Getenv("POSTGRES_USER")
	if appUser == "" {
		appUser = "it_news_app"
	}

	cfg := testPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := uuid.NewString()[:8]
	testDB := "it_news_mig_test_" + suffix
	composeFile := filepath.Join("..", "..", "deployments", "docker-compose.yml")

	createCmd := exec.CommandContext(ctx, "docker", "compose", "-p", composeProject, "-f", composeFile, "exec", "-T", "postgres",
		"psql", "-U", adminUser, "-d", "postgres", "-c", fmt.Sprintf("CREATE DATABASE %s OWNER %s;", testDB, appUser))
	if out, err := createCmd.CombinedOutput(); err != nil {
		t.Fatalf("create migration test db failed: %v (%s)", err, string(out))
	}
	defer func() {
		dropCmd := exec.Command("docker", "compose", "-p", composeProject, "-f", composeFile, "exec", "-T", "postgres",
			"psql", "-U", adminUser, "-d", "postgres", "-c", fmt.Sprintf("DROP DATABASE IF EXISTS %s;", testDB))
		_ = dropCmd.Run()
	}()

	testDSN := strings.Replace(cfg.DSN, "/it_news?", "/"+testDB+"?", 1)
	if testDSN == cfg.DSN {
		t.Fatalf("could not derive test DSN from %s", config.RedactDSN(cfg.DSN))
	}

	// Concurrent MigrateUp in isolated empty DB must succeed without global state races.
	var wg sync.WaitGroup
	migErrs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			migErrs[idx] = postgres.MigrateUp(ctx, testDSN)
		}(i)
	}
	wg.Wait()
	for i, err := range migErrs {
		if err != nil {
			t.Fatalf("concurrent MigrateUp(%d) error = %v", i, err)
		}
	}

	if err := postgres.MigrateDown(ctx, testDSN); err != nil {
		t.Fatalf("MigrateDown(2->1) error = %v", err)
	}
	if err := postgres.MigrateDown(ctx, testDSN); err != nil {
		t.Fatalf("MigrateDown(1->0) error = %v", err)
	}
	if err := postgres.MigrateUp(ctx, testDSN); err != nil {
		t.Fatalf("MigrateUp(0->2) error = %v", err)
	}
}

func TestInitDBsScriptQuoting(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "deployments", "postgres", "init-dbs.sh")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	containerName := "it-news-init-test-" + suffix
	adminUser := "postgres"
	adminPass := "ephemeral_admin_" + suffix
	testRole := "init_role_" + suffix
	testTempRole := "init_trole_" + suffix
	testDB := "init_db_" + suffix
	// Synthetic passwords containing apostrophe, double quote, backslash, and dollar sign.
	appPass := `p'ass"w\d$1!`
	tempPass := `t'emp"p\w$2!`

	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", "-v", containerName).CombinedOutput(); err != nil {
			t.Errorf("remove ephemeral postgres container %s: %v (%s)", containerName, err, string(out))
		}
	})

	startCmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", containerName,
		"-e", "POSTGRES_USER="+adminUser,
		"-e", "POSTGRES_PASSWORD="+adminPass,
		"-e", "POSTGRES_DB=postgres",
		"postgres:16.16-alpine",
	)
	if out, err := startCmd.CombinedOutput(); err != nil {
		t.Fatalf("start ephemeral postgres container: %v (%s)", err, string(out))
	}

	ready := false
	for i := 0; i < 60; i++ {
		if err := exec.CommandContext(ctx, "docker", "exec", containerName,
			"pg_isready", "-h", "127.0.0.1", "-U", adminUser, "-d", "postgres").Run(); err == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for ephemeral postgres readiness: %v", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatal("ephemeral postgres did not become ready")
	}

	runInitScript := func() {
		t.Helper()
		scriptBytes, err := os.ReadFile(scriptPath)
		if err != nil {
			t.Fatalf("read init-dbs.sh: %v", err)
		}
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i",
			"-e", "POSTGRES_USER="+adminUser,
			"-e", "POSTGRES_DB=postgres",
			"-e", "APP_DB_USER="+testRole,
			"-e", "APP_DB_PASSWORD="+appPass,
			"-e", "APP_DB_NAME="+testDB,
			"-e", "TEMPORAL_DB_USER="+testTempRole,
			"-e", "TEMPORAL_DB_PASSWORD="+tempPass,
			containerName, "sh")
		cmd.Stdin = strings.NewReader(string(scriptBytes))
		out, err := cmd.CombinedOutput()
		outStr := string(out)
		if strings.Contains(outStr, appPass) || strings.Contains(outStr, tempPass) {
			t.Fatal("init-dbs.sh leaked synthetic password into stdout/stderr")
		}
		if err != nil {
			t.Fatalf("init-dbs.sh failed: %v (%s)", err, outStr)
		}
	}

	// First run creates roles and databases; second run verifies idempotency.
	runInitScript()
	runInitScript()

	// Verify real TCP login inside the ephemeral container with special-character passwords.
	for _, check := range []struct {
		user string
		pass string
		db   string
	}{
		{user: testRole, pass: appPass, db: testDB},
		{user: testTempRole, pass: tempPass, db: "temporal"},
		{user: testTempRole, pass: tempPass, db: "temporal_visibility"},
	} {
		loginCmd := exec.CommandContext(ctx, "docker", "exec",
			"-e", "PGPASSWORD="+check.pass,
			containerName,
			"psql", "-h", "127.0.0.1", "-U", check.user, "-d", check.db, "-tAc", "SELECT 1;")
		out, err := loginCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("login verification failed for user %s on db %s: %v (%s)", check.user, check.db, err, string(out))
		}
		if strings.TrimSpace(string(out)) != "1" {
			t.Fatalf("login query for user %s on db %s = %q, want 1", check.user, check.db, strings.TrimSpace(string(out)))
		}
	}
}

func bytesTrimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
