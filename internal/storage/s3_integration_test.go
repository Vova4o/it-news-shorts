//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
)

func testMinIOConfig(t *testing.T) config.MinIOConfig {
	t.Helper()
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if os.Getenv("MINIO_ACCESS_KEY") == "" {
		cfg.MinIO.AccessKey = "minioadmin_dev"
	}
	if os.Getenv("MINIO_SECRET_KEY") == "" {
		cfg.MinIO.SecretKey = "minioadmin_dev_pass"
	}
	return cfg.MinIO
}

func TestS3StoreLifecycleAndFailures(t *testing.T) {
	cfg := testMinIOConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		t.Fatalf("NewS3Store() error = %v", err)
	}
	if err := store.EnsureBucket(ctx, cfg.Bucket); err != nil {
		t.Fatalf("first EnsureBucket() error = %v", err)
	}
	if err := store.EnsureBucket(ctx, cfg.Bucket); err != nil {
		t.Fatalf("second EnsureBucket() error = %v", err)
	}

	suffix := uuid.NewString()[:8]
	textKey := "integration-tests/" + suffix + "/hello.txt"
	textPayload := []byte("hello minio integration " + suffix)

	putRes, err := store.PutObject(ctx, storage.Object{
		Bucket:      cfg.Bucket,
		Key:         textKey,
		ContentType: "text/plain",
		Size:        int64(len(textPayload)),
	}, bytes.NewReader(textPayload))
	if err != nil {
		t.Fatalf("PutObject(text) error = %v", err)
	}
	sum := sha256.Sum256(textPayload)
	if putRes.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %s, want %s", putRes.SHA256, hex.EncodeToString(sum[:]))
	}

	// Binary upload & download.
	binKey := "integration-tests/" + suffix + "/blob.bin"
	binPayload := []byte{0x00, 0xFF, 0x10, 0x20, 0x30, 0x42, 0x99}
	if _, err := store.PutObject(ctx, storage.Object{
		Bucket:      cfg.Bucket,
		Key:         binKey,
		ContentType: "application/octet-stream",
		Size:        int64(len(binPayload)),
	}, bytes.NewReader(binPayload)); err != nil {
		t.Fatalf("PutObject(bin) error = %v", err)
	}
	defer func() { _ = store.DeleteObject(context.Background(), cfg.Bucket, binKey) }()

	rc, stat, err := store.GetObject(ctx, cfg.Bucket, binKey)
	if err != nil {
		t.Fatalf("GetObject(bin) error = %v", err)
	}
	gotBin, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(gotBin, binPayload) || stat.SizeBytes != int64(len(binPayload)) {
		t.Fatalf("downloaded binary mismatch: got=%x err=%v", gotBin, err)
	}

	// Presigned URL works; anonymous direct HTTP GET is forbidden (private bucket).
	presignedURL, err := store.PresignGetObject(ctx, cfg.Bucket, textKey, 2*time.Minute)
	if err != nil {
		t.Fatalf("PresignGetObject() error = %v", err)
	}
	resp, err := http.Get(presignedURL)
	if err != nil {
		t.Fatalf("GET presigned URL error = %v", err)
	}
	presignedBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(presignedBody, textPayload) {
		t.Fatalf("presigned GET status=%d body=%q", resp.StatusCode, string(presignedBody))
	}

	anonResp, err := http.Get("http://" + cfg.Endpoint + "/" + cfg.Bucket + "/" + textKey)
	if err == nil {
		_ = anonResp.Body.Close()
		if anonResp.StatusCode == http.StatusOK {
			t.Fatal("anonymous GET succeeded on private bucket")
		}
	}

	// ObjectExists & DeleteObject idempotency.
	exists, err := store.ObjectExists(ctx, cfg.Bucket, textKey)
	if err != nil || !exists {
		t.Fatalf("ObjectExists() = %v, err=%v", exists, err)
	}
	if err := store.DeleteObject(ctx, cfg.Bucket, textKey); err != nil {
		t.Fatalf("DeleteObject() error = %v", err)
	}
	if err := store.DeleteObject(ctx, cfg.Bucket, textKey); err != nil {
		t.Fatalf("repeated DeleteObject() error = %v", err)
	}

	// Missing object returns ErrObjectNotFound.
	if _, _, err := store.GetObject(ctx, cfg.Bucket, "integration-tests/missing-"+suffix); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing GetObject() error = %v, want ErrObjectNotFound", err)
	}

	// Bad credentials fail.
	badCredsCfg := cfg
	badCredsCfg.AccessKey = "wrong-key"
	badCredsCfg.SecretKey = "wrong-secret"
	badStore, err := storage.NewS3Store(ctx, badCredsCfg)
	if err != nil {
		t.Fatalf("NewS3Store(bad) error = %v", err)
	}
	if err := badStore.Ping(ctx); err == nil {
		t.Fatal("expected Ping() error with bad credentials, got nil")
	}

	// Unreachable MinIO fails.
	downCfg := cfg
	downCfg.Endpoint = "127.0.0.1:59998"
	downCfg.RequestTimeout = time.Second
	downStore, err := storage.NewS3Store(ctx, downCfg)
	if err != nil {
		t.Fatalf("NewS3Store(down) error = %v", err)
	}
	if err := downStore.Ping(ctx); err == nil {
		t.Fatal("expected Ping() error for unreachable MinIO, got nil")
	}

	// Context cancellation.
	cancelCtx, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if _, err := store.PutObject(cancelCtx, storage.Object{
		Bucket: cfg.Bucket,
		Key:    "integration-tests/canceled-" + suffix,
		Size:   4,
	}, bytes.NewReader([]byte("test"))); err == nil {
		t.Fatal("expected error on canceled context")
	}
}
