package storage_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
)

func TestBuildDailyObjectKey(t *testing.T) {
	date := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		jobID    domain.VideoJobID
		category string
		filename string
		want     string
		wantErr  bool
	}{
		{
			name:     "rejects traversal in filename",
			jobID:    "job-123",
			category: storage.CategoryFinal,
			filename: "../../my video (1).mp4",
			wantErr:  true,
		},
		{
			name:     "valid script key",
			jobID:    "job-456",
			category: storage.CategoryScripts,
			filename: "script_v1.txt",
			want:     "daily/2026-11-13/job-456/scripts/script_v1.txt",
		},
		{
			name:     "rejects unknown category",
			jobID:    "job-123",
			category: "secrets",
			filename: "a.txt",
			wantErr:  true,
		},
		{
			name:     "rejects traversal dotdot",
			jobID:    "job-123",
			category: storage.CategoryVideo,
			filename: "..",
			wantErr:  true,
		},
		{
			name:     "rejects jobID traversal",
			jobID:    "../../outside",
			category: storage.CategoryVideo,
			filename: "result.mp4",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := storage.BuildDailyObjectKey(date, tt.jobID, tt.category, tt.filename)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got key %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildDailyObjectKey() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	// Normalized filenames must not collide lossily.
	k1, err := storage.BuildDailyObjectKey(date, "job-123", storage.CategoryFinal, "my video (1).mp4")
	if err != nil {
		t.Fatalf("BuildDailyObjectKey(k1) error = %v", err)
	}
	k2, err := storage.BuildDailyObjectKey(date, "job-123", storage.CategoryFinal, "my_video_(1).mp4")
	if err != nil {
		t.Fatalf("BuildDailyObjectKey(k2) error = %v", err)
	}
	if k1 == k2 {
		t.Fatalf("lossy collision between distinct filenames: both produced %q", k1)
	}
}

func newTestS3Store(t *testing.T, endpoint string, timeout time.Duration) *storage.S3Store {
	t.Helper()
	store, err := storage.NewS3Store(context.Background(), config.MinIOConfig{
		Endpoint:       endpoint,
		Bucket:         "test-bucket",
		AccessKey:      "local-test-access",
		SecretKey:      "local-test-secret",
		Region:         "us-east-1",
		MaxObjectBytes: 1024,
		RequestTimeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestObjectKeyCannotEscapeDailyPrefix(t *testing.T) {
	key, err := storage.BuildDailyObjectKey(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), domain.VideoJobID("../../outside"), storage.CategoryVideo, "result.mp4")
	if err == nil && !strings.HasPrefix(key, "daily/2026-11-13/") {
		t.Fatalf("job id traversal escaped daily namespace: %q", key)
	}
}

type countingReader struct {
	reader io.Reader
	calls  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.calls++
	return r.reader.Read(p)
}

func TestCancelledPutDoesNotConsumeReader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancelled upload sent an HTTP request")
		w.WriteHeader(500)
	}))
	defer server.Close()
	store := newTestS3Store(t, server.URL, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &countingReader{reader: bytes.NewReader([]byte("payload"))}
	_, err := store.PutObject(ctx, storage.Object{Key: "test.txt", Size: 7}, reader)
	if err == nil {
		t.Fatal("cancelled upload returned success")
	}
	if reader.calls != 0 {
		t.Fatalf("cancelled operation consumed input in %d read calls", reader.calls)
	}
}

func TestGetHonorsConfiguredTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	store := newTestS3Store(t, server.URL, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	body, _, err := store.GetObject(ctx, "test-bucket", "test.txt")
	if body != nil {
		_ = body.Close()
	}
	if err == nil {
		t.Fatal("hanging server returned success")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("configured 20ms request timeout ignored; elapsed %s", elapsed)
	}
}

func TestPresignRejectsInvalidSigV4Expiration(t *testing.T) {
	store := newTestS3Store(t, "http://127.0.0.1:1", time.Second)
	if _, err := store.PresignGetObject(context.Background(), "test-bucket", "test.txt", 8*24*time.Hour); err == nil {
		t.Fatal("presigned URL accepted expiration beyond SigV4 seven-day limit")
	}
}
