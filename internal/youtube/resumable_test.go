package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func makeTestUpload(t *testing.T, content []byte, key string) (UploadInput, *Journal) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	return UploadInput{
		FilePath:       file,
		Title:          "Test Video",
		Description:    "Integration test",
		IdempotencyKey: key,
	}.Normalize(), journal
}

func TestUploadVideo_SuccessAndIdempotentReplay(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("A"), minChunkSize*2)
	in, journal := makeTestUpload(t, payload, "key-success-1")

	var inserts atomic.Int32
	var received bytes.Buffer
	fixedNow := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/videos":
			inserts.Add(1)
			w.Header().Set("Location", "http://"+r.Host+"/session/1")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.URL.Path == "/session/1":
			body, _ := io.ReadAll(r.Body)
			received.Write(body)
			if received.Len() < len(payload) {
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", received.Len()-1))
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"vid-123","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(nil,
		WithHTTPClient(server.Client()),
		WithUploadURL(server.URL),
		WithJournal(journal),
		WithChunkSize(minChunkSize),
		WithClock(func() time.Time { return fixedNow }, func(context.Context, time.Duration) error { return nil }),
	)

	res1, err := client.UploadVideo(context.Background(), in)
	if err != nil {
		t.Fatalf("first UploadVideo failed: %v", err)
	}
	if res1.VideoID != "vid-123" || res1.UploadStatus != "uploaded" || res1.PrivacyStatus != "private" {
		t.Fatalf("unexpected first result: %+v", res1)
	}
	if !res1.UploadedAt.Equal(fixedNow) {
		t.Fatalf("unexpected UploadedAt: got %v, want %v", res1.UploadedAt, fixedNow)
	}
	if !bytes.Equal(received.Bytes(), payload) {
		t.Fatalf("uploaded bytes mismatch: got %d, want %d", received.Len(), len(payload))
	}

	// Second call with same idempotency key must replay without calling server.
	res2, err := client.UploadVideo(context.Background(), in)
	if err != nil {
		t.Fatalf("replay UploadVideo failed: %v", err)
	}
	if !res2.Resumed || res2.VideoID != res1.VideoID || res2.UploadStatus != "uploaded" || !res2.UploadedAt.Equal(fixedNow) {
		t.Fatalf("replay mismatch: res2=%+v res1=%+v", res2, res1)
	}
	if inserts.Load() != 1 {
		t.Fatalf("expected 1 insert, got %d", inserts.Load())
	}

	// Replay must also succeed without creating a duplicate even if the local
	// source file was cleaned up after completion.
	if err := os.Remove(in.FilePath); err != nil {
		t.Fatal(err)
	}
	res3, err := client.UploadVideo(context.Background(), in)
	if err != nil || !res3.Resumed || res3.VideoID != "vid-123" || inserts.Load() != 1 {
		t.Fatalf("replay after file removal failed: res3=%+v err=%v inserts=%d", res3, err, inserts.Load())
	}
}

func TestUploadVideo_RejectsNilJournalInvalidChunkSizeNonRegularAndDifferentFile(t *testing.T) {
	t.Parallel()

	in, journal := makeTestUpload(t, []byte("original-content"), "key-file-check")

	clientNoJournal := NewClient(nil)
	if _, err := clientNoJournal.UploadVideo(context.Background(), in); err == nil {
		t.Fatal("expected error when journal is nil")
	}

	clientBadChunk := NewClient(nil, WithJournal(journal), WithChunkSize(1024))
	if _, err := clientBadChunk.UploadVideo(context.Background(), in); err == nil {
		t.Fatal("expected error when chunk size is not a multiple of 256 KiB")
	}

	clientDir := NewClient(nil, WithJournal(journal))
	badInput := in
	badInput.FilePath = filepath.Dir(in.FilePath)
	if _, err := clientDir.UploadVideo(context.Background(), badInput); err == nil {
		t.Fatal("expected error when upload path is a directory")
	}

	// FIFO must be rejected immediately without blocking.
	fifoPath := filepath.Join(t.TempDir(), "pipe.mp4")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	fifoInput := in
	fifoInput.FilePath = fifoPath
	done := make(chan error, 1)
	go func() {
		_, err := clientDir.UploadVideo(context.Background(), fifoInput)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for FIFO upload path")
		}
	case <-time.After(time.Second):
		t.Fatal("UploadVideo blocked on FIFO instead of failing immediately")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "http://"+r.Host+"/session/1")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"vid-orig","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
	}))
	defer server.Close()

	client := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(journal))
	if _, err := client.UploadVideo(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	// Overwrite file with different content and reuse same key.
	if err := os.WriteFile(in.FilePath, []byte("different-bytes!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadVideo(context.Background(), in); err == nil {
		t.Fatal("expected error when reusing idempotency key for a different file")
	}
}

type contextAwareBody struct {
	ctx    context.Context
	reader io.Reader
	closed *atomic.Bool
}

func (b contextAwareBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.reader.Read(p)
}

func (b contextAwareBody) Close() error {
	if b.closed != nil {
		b.closed.Store(true)
	}
	return nil
}

func TestUploadVideo_ChunkResponseRemainsReadable(t *testing.T) {
	t.Parallel()

	t.Run("context-aware body remains readable and is closed", func(t *testing.T) {
		t.Parallel()
		in, journal := makeTestUpload(t, []byte("file-content"), "key-ctx-body")
		var queries atomic.Int32
		var bodyClosed atomic.Bool

		transport := deadlineCheckTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Location": []string{"http://local.invalid/session"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}
			if strings.HasPrefix(r.Header.Get("Content-Range"), "bytes */") {
				queries.Add(1)
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: contextAwareBody{
					ctx:    r.Context(),
					reader: strings.NewReader(`{"id":"video-ok","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`),
					closed: &bodyClosed,
				},
				Request: r,
			}, nil
		})

		client := NewClient(nil, WithHTTPClient(&http.Client{Transport: transport}), WithJournal(journal), WithRetryPolicy(1, time.Millisecond, time.Millisecond))
		res, err := client.UploadVideo(context.Background(), in)
		if err != nil || res.VideoID != "video-ok" || queries.Load() != 0 || !bodyClosed.Load() {
			t.Fatalf("unexpected result: res=%+v queries=%d closed=%v err=%v", res, queries.Load(), bodyClosed.Load(), err)
		}
	})

	t.Run("streaming httptest server flushes headers before body", func(t *testing.T) {
		t.Parallel()
		in, journal := makeTestUpload(t, []byte("streamed-content"), "key-stream-body")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "http://"+r.Host+"/session/stream")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(15 * time.Millisecond)
			fmt.Fprint(w, `{"id":"video-streamed","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		}))
		defer server.Close()

		client := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(journal), WithTimeout(time.Second))
		res, err := client.UploadVideo(context.Background(), in)
		if err != nil || res.VideoID != "video-streamed" {
			t.Fatalf("streamed chunk decode failed: res=%+v err=%v", res, err)
		}
	})
}

func TestUploadVideo_ResumeAfterTransientFailureAnd308Offset(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("R"), minChunkSize*2)
	in, journal := makeTestUpload(t, payload, "key-resume-308")

	var inserts atomic.Int32
	var putCalls atomic.Int32
	var gotRanges []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/videos":
			inserts.Add(1)
			w.Header().Set("Location", "http://"+r.Host+"/session/1")
		case r.Method == http.MethodPut && r.URL.Path == "/session/1":
			cr := r.Header.Get("Content-Range")
			gotRanges = append(gotRanges, cr)
			n := putCalls.Add(1)
			switch n {
			case 1:
				// First chunk succeeds partially (server confirms first half of chunk 1).
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", minChunkSize/2-1))
				w.WriteHeader(http.StatusPermanentRedirect)
			case 2:
				// Next chunk returns 503.
				w.WriteHeader(http.StatusServiceUnavailable)
			case 3:
				// querySession returns 308 confirming full first chunk already received.
				if cr != fmt.Sprintf("bytes */%d", len(payload)) {
					t.Errorf("expected querySession Content-Range bytes */%d, got %q", len(payload), cr)
				}
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", minChunkSize-1))
				w.WriteHeader(http.StatusPermanentRedirect)
			case 4:
				// Client must resume from minChunkSize.
				wantCR := fmt.Sprintf("bytes %d-%d/%d", minChunkSize, len(payload)-1, len(payload))
				if cr != wantCR {
					t.Errorf("expected resumed Content-Range %q, got %q", wantCR, cr)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"vid-resumed","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
			}
		}
	}))
	defer server.Close()

	client := NewClient(nil,
		WithHTTPClient(server.Client()),
		WithUploadURL(server.URL),
		WithJournal(journal),
		WithChunkSize(minChunkSize),
		WithRetryPolicy(3, time.Millisecond, 5*time.Millisecond),
		WithClock(time.Now, func(context.Context, time.Duration) error { return nil }),
	)

	res, err := client.UploadVideo(context.Background(), in)
	if err != nil {
		t.Fatalf("UploadVideo failed: %v (ranges=%v)", err, gotRanges)
	}
	if res.VideoID != "vid-resumed" {
		t.Fatalf("got VideoID %q, want vid-resumed", res.VideoID)
	}
	if inserts.Load() != 1 {
		t.Fatalf("expected 1 insert, got %d", inserts.Load())
	}
}

func TestUploadVideo_308ProgressHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	payload := bytes.Repeat([]byte("x"), minChunkSize*3)

	t.Run("seconds and HTTP date on 308 progress", func(t *testing.T) {
		t.Parallel()
		in, journal := makeTestUpload(t, payload, "key-308-retry-after")
		var waits []time.Duration
		var chunks atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "http://"+r.Host+"/session/308")
				return
			}
			n := chunks.Add(1)
			switch n {
			case 1:
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", minChunkSize-1))
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusPermanentRedirect)
			case 2:
				if len(waits) != 1 || waits[0] != 120*time.Second {
					t.Errorf("expected 120s wait before chunk 2, got %v", waits)
				}
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", minChunkSize*2-1))
				w.Header().Set("Retry-After", now.Add(95*time.Second).Format(http.TimeFormat))
				w.WriteHeader(http.StatusPermanentRedirect)
			case 3:
				if len(waits) != 2 || waits[1] != 95*time.Second {
					t.Errorf("expected 95s HTTP-date wait before chunk 3, got %v", waits)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"vid-308-waited"}`)
			}
		}))
		defer server.Close()

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithUploadURL(server.URL),
			WithJournal(journal),
			WithChunkSize(minChunkSize),
			WithClock(func() time.Time { return now }, func(_ context.Context, d time.Duration) error {
				waits = append(waits, d)
				return nil
			}),
		)
		res, err := client.UploadVideo(context.Background(), in)
		if err != nil || res.VideoID != "vid-308-waited" {
			t.Fatalf("unexpected result: res=%+v err=%v", res, err)
		}
	})

	t.Run("context cancellation during 308 Retry-After sleep", func(t *testing.T) {
		t.Parallel()
		in, journal := makeTestUpload(t, payload, "key-308-cancel")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "http://"+r.Host+"/session/308c")
				return
			}
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", minChunkSize-1))
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusPermanentRedirect)
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(context.Background())
		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithUploadURL(server.URL),
			WithJournal(journal),
			WithChunkSize(minChunkSize),
			WithClock(func() time.Time { return now }, func(c context.Context, _ time.Duration) error {
				cancel()
				return c.Err()
			}),
		)
		_, err := client.UploadVideo(ctx, in)
		if !errors.Is(err, ErrUploadUnknown) {
			t.Fatalf("expected ErrUploadUnknown on cancelled 308 sleep, got %v", err)
		}
	})
}

func TestUploadVideo_ConnectionDropAndCrashRecovery(t *testing.T) {
	t.Parallel()

	t.Run("crash after upload before persist resolves via querySession 200", func(t *testing.T) {
		t.Parallel()
		payload := []byte("crash-recovery-bytes")
		in, journal := makeTestUpload(t, payload, "key-crash-200")
		fileHash := mustSHA256(t, in.FilePath)

		var inserts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				inserts.Add(1)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"vid-after-crash","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		}))
		defer server.Close()

		if err := journal.Put(context.Background(), JournalEntry{
			Key:        in.IdempotencyKey,
			State:      UploadStateInProgress,
			SessionURI: server.URL + "/old",
			TotalBytes: int64(len(payload)),
			FileSHA256: fileHash,
		}); err != nil {
			t.Fatal(err)
		}

		client := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(journal))
		res, err := client.UploadVideo(context.Background(), in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.VideoID != "vid-after-crash" || !res.Resumed || inserts.Load() != 0 {
			t.Fatalf("unexpected result=%+v inserts=%d", res, inserts.Load())
		}
	})

	t.Run("recoverable UNKNOWN with session URI confirms completion without new insert", func(t *testing.T) {
		t.Parallel()
		payload := []byte("recoverable-unknown-bytes")
		in, journal := makeTestUpload(t, payload, "key-unknown-200")
		fileHash := mustSHA256(t, in.FilePath)

		var inserts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				inserts.Add(1)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"vid-recovered-unknown","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		}))
		defer server.Close()

		if err := journal.Put(context.Background(), JournalEntry{
			Key:        in.IdempotencyKey,
			State:      UploadStateUnknown,
			SessionURI: server.URL + "/unknown-session",
			TotalBytes: int64(len(payload)),
			FileSHA256: fileHash,
		}); err != nil {
			t.Fatal(err)
		}

		client := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(journal))
		res, err := client.UploadVideo(context.Background(), in)
		if err != nil || res.VideoID != "vid-recovered-unknown" || inserts.Load() != 0 {
			t.Fatalf("expected recovery from UNKNOWN without insert, got res=%+v err=%v inserts=%d", res, err, inserts.Load())
		}
	})

	t.Run("ambiguous 503 on existing session marks UNKNOWN without new insert", func(t *testing.T) {
		t.Parallel()
		payload := []byte("ambiguous-bytes")
		in, journal := makeTestUpload(t, payload, "key-crash-503")
		fileHash := mustSHA256(t, in.FilePath)

		var inserts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				inserts.Add(1)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		if err := journal.Put(context.Background(), JournalEntry{
			Key:        in.IdempotencyKey,
			State:      UploadStateInProgress,
			SessionURI: server.URL + "/old",
			TotalBytes: int64(len(payload)),
			FileSHA256: fileHash,
		}); err != nil {
			t.Fatal(err)
		}

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithUploadURL(server.URL),
			WithJournal(journal),
			WithRetryPolicy(1, time.Millisecond, time.Millisecond),
		)
		res, err := client.UploadVideo(context.Background(), in)
		if !errors.Is(err, ErrUploadUnknown) || res.State != UploadStateUnknown || inserts.Load() != 0 {
			t.Fatalf("expected ErrUploadUnknown and 0 inserts, got res=%+v err=%v inserts=%d", res, err, inserts.Load())
		}
		res2, err2 := client.UploadVideo(context.Background(), in)
		if !errors.Is(err2, ErrUploadUnknown) || res2.State != UploadStateUnknown || inserts.Load() != 0 {
			t.Fatalf("expected persistent UNKNOWN, got res=%+v err=%v inserts=%d", res2, err2, inserts.Load())
		}
	})

	t.Run("404 on existing session never creates new upload on replay", func(t *testing.T) {
		t.Parallel()
		payload := []byte("lost-session-bytes")
		in, journal := makeTestUpload(t, payload, "key-crash-404")
		fileHash := mustSHA256(t, in.FilePath)

		var inserts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				inserts.Add(1)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		if err := journal.Put(context.Background(), JournalEntry{
			Key:        in.IdempotencyKey,
			State:      UploadStateInProgress,
			SessionURI: server.URL + "/expired",
			TotalBytes: int64(len(payload)),
			FileSHA256: fileHash,
		}); err != nil {
			t.Fatal(err)
		}

		client := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(journal))
		for i := 0; i < 2; i++ {
			_, err := client.UploadVideo(context.Background(), in)
			if !errors.Is(err, ErrUploadUnknown) {
				t.Fatalf("attempt %d: expected ErrUploadUnknown, got %v", i+1, err)
			}
		}
		if inserts.Load() != 0 {
			t.Fatalf("lost session retried with %d new insert(s)", inserts.Load())
		}
	})

	t.Run("stale PENDING without session URI never creates new upload", func(t *testing.T) {
		t.Parallel()
		payload := []byte("stale-pending-bytes")
		in, journal := makeTestUpload(t, payload, "key-stale-pending")
		fileHash := mustSHA256(t, in.FilePath)

		var inserts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inserts.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		if err := journal.Put(context.Background(), JournalEntry{
			Key:        in.IdempotencyKey,
			State:      UploadStatePending,
			TotalBytes: int64(len(payload)),
			FileSHA256: fileHash,
		}); err != nil {
			t.Fatal(err)
		}

		client := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(journal))
		if _, err := client.UploadVideo(context.Background(), in); !errors.Is(err, ErrUploadUnknown) || inserts.Load() != 0 {
			t.Fatalf("expected ErrUploadUnknown and 0 inserts, got inserts=%d err=%v", inserts.Load(), err)
		}
	})

	t.Run("connection drop during chunk upload recovers via querySession", func(t *testing.T) {
		t.Parallel()
		payload := []byte("drop-mid-upload")
		in, journal := makeTestUpload(t, payload, "key-drop")

		var calls atomic.Int32
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", server.URL+"/session/drop")
				return
			}
			n := calls.Add(1)
			if n == 1 {
				hj, ok := w.(http.Hijacker)
				if !ok {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"vid-dropped-ok","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		}))
		defer server.Close()

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithUploadURL(server.URL),
			WithJournal(journal),
			WithRetryPolicy(2, time.Millisecond, time.Millisecond),
			WithClock(time.Now, func(context.Context, time.Duration) error { return nil }),
		)
		res, err := client.UploadVideo(context.Background(), in)
		if err != nil {
			t.Fatalf("expected recovery after drop, got %v", err)
		}
		if res.VideoID != "vid-dropped-ok" {
			t.Fatalf("got VideoID %q", res.VideoID)
		}
	})

	t.Run("unresolved status probe after partial progress does not replay data bytes", func(t *testing.T) {
		t.Parallel()
		payload := bytes.Repeat([]byte("P"), minChunkSize*2)
		in, journal := makeTestUpload(t, payload, "key-unresolved-probe")

		var dataRequests atomic.Int32
		var probes atomic.Int32
		transport := deadlineCheckTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Location": []string{"http://local.invalid/session"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}
			if strings.HasPrefix(r.Header.Get("Content-Range"), "bytes */") {
				probes.Add(1)
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}
			n := dataRequests.Add(1)
			if n == 1 {
				// First chunk partially accepted.
				return &http.Response{
					StatusCode: http.StatusPermanentRedirect,
					Header:     http.Header{"Range": []string{fmt.Sprintf("bytes=0-%d", minChunkSize-1)}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}
			if n == 2 {
				return nil, io.ErrUnexpectedEOF
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id":"should-not-happen"}`)),
				Request:    r,
			}, nil
		})

		client := NewClient(nil,
			WithHTTPClient(&http.Client{Transport: transport}),
			WithJournal(journal),
			WithChunkSize(minChunkSize),
			WithRetryPolicy(2, time.Millisecond, time.Millisecond),
			WithClock(time.Now, func(context.Context, time.Duration) error { return nil }),
		)
		res, err := client.UploadVideo(context.Background(), in)
		if !errors.Is(err, ErrUploadUnknown) || res.State != UploadStateUnknown {
			t.Fatalf("expected ErrUploadUnknown, got res=%+v err=%v", res, err)
		}
		if dataRequests.Load() != 2 {
			t.Fatalf("sent %d data requests after %d failed status probes (expected 2)", dataRequests.Load(), probes.Load())
		}
	})

	t.Run("completed session without video id persists UNKNOWN", func(t *testing.T) {
		t.Parallel()
		in, journal := makeTestUpload(t, []byte("no-vid-id"), "key-no-vid")
		var inserts atomic.Int32
		transport := deadlineCheckTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				inserts.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Location": []string{"http://local.invalid/session"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    r,
			}, nil
		})
		client := NewClient(nil, WithHTTPClient(&http.Client{Transport: transport}), WithJournal(journal))
		if _, err := client.UploadVideo(context.Background(), in); !errors.Is(err, ErrUploadUnknown) {
			t.Fatalf("expected ErrUploadUnknown, got %v", err)
		}
		if _, err := client.UploadVideo(context.Background(), in); !errors.Is(err, ErrUploadUnknown) || inserts.Load() != 1 {
			t.Fatalf("expected persistent UNKNOWN and 1 insert, got inserts=%d err=%v", inserts.Load(), err)
		}
	})
}

func TestUploadVideo_SessionCredentialsRedacted(t *testing.T) {
	t.Parallel()

	in, journal := makeTestUpload(t, []byte("file-content"), "key-redact")
	const sensitive = "session-secret-marker"
	const sensitiveSeg = "very-long-secret-capability-token-segment-123456"

	transport := deadlineCheckTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Location": []string{"http://local.invalid/session/" + sensitiveSeg + "?upload_id=" + sensitive}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		}
		return nil, io.ErrUnexpectedEOF
	})

	client := NewClient(nil,
		WithHTTPClient(&http.Client{Transport: transport}),
		WithJournal(journal),
		WithRetryPolicy(1, time.Millisecond, time.Millisecond),
	)
	_, err := client.UploadVideo(context.Background(), in)
	if err == nil {
		t.Fatal("expected transport error")
	}
	entry, found, getErr := journal.Get(context.Background(), in.IdempotencyKey)
	if getErr != nil || !found {
		t.Fatalf("journal.Get failed: found=%v err=%v", found, getErr)
	}
	if strings.Contains(err.Error(), sensitive) || strings.Contains(entry.Error, sensitive) ||
		strings.Contains(err.Error(), sensitiveSeg) || strings.Contains(entry.Error, sensitiveSeg) {
		t.Fatalf("sensitive session URI leaked in error: err=%q entry.Error=%q", err.Error(), entry.Error)
	}
	if !errors.Is(err, ErrUploadUnknown) || !errors.Is(err, ErrRetriesExhausted) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected ErrUploadUnknown -> ErrRetriesExhausted -> io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestUploadVideo_HTTPErrorCodesAndInvalidRange(t *testing.T) {
	t.Parallel()

	t.Run("401 returns APIError and 403 insufficientPermissions returns ErrInsufficientScope", func(t *testing.T) {
		t.Parallel()
		in401, j401 := makeTestUpload(t, []byte("bytes"), "key-401")
		srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":401,"message":"invalid token"}}`)
		}))
		c401 := NewClient(nil, WithHTTPClient(srv401.Client()), WithUploadURL(srv401.URL), WithJournal(j401))
		_, err401 := c401.UploadVideo(context.Background(), in401)
		srv401.Close()
		var apiErr401 *APIError
		if !errors.As(err401, &apiErr401) || apiErr401.StatusCode != http.StatusUnauthorized || errors.Is(err401, ErrInsufficientScope) {
			t.Fatalf("401 should be *APIError (not ErrInsufficientScope), got %v", err401)
		}

		in403, j403 := makeTestUpload(t, []byte("bytes"), "key-403")
		srv403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":403,"message":"insufficient","errors":[{"reason":"insufficientPermissions"}]}}`)
		}))
		c403 := NewClient(nil, WithHTTPClient(srv403.Client()), WithUploadURL(srv403.URL), WithJournal(j403))
		_, err403 := c403.UploadVideo(context.Background(), in403)
		srv403.Close()
		var apiErr403 *APIError
		if !errors.Is(err403, ErrInsufficientScope) || !errors.As(err403, &apiErr403) {
			t.Fatalf("403 insufficientPermissions should match ErrInsufficientScope and *APIError, got %v", err403)
		}
	})

	t.Run("429 respects Retry-After seconds and HTTP date without shortening", func(t *testing.T) {
		t.Parallel()
		in, journal := makeTestUpload(t, []byte("bytes"), "key-429")
		now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		var sleeps []time.Duration
		var attempts atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				n := attempts.Add(1)
				if n == 1 {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if n == 2 {
					w.Header().Set("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat))
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Location", "http://"+r.Host+"/session/ok")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"vid-429"}`)
		}))
		defer server.Close()

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithUploadURL(server.URL),
			WithJournal(journal),
			WithRetryPolicy(4, time.Millisecond, 30*time.Second),
			WithClock(func() time.Time { return now }, func(_ context.Context, d time.Duration) error {
				sleeps = append(sleeps, d)
				return nil
			}),
		)
		res, err := client.UploadVideo(context.Background(), in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.VideoID != "vid-429" {
			t.Fatalf("got %q", res.VideoID)
		}
		if len(sleeps) != 2 || sleeps[0] != 120*time.Second || sleeps[1] != 90*time.Second {
			t.Fatalf("expected Retry-After sleeps [120s 90s], got %v", sleeps)
		}
	})

	t.Run("invalid 308 Range header is rejected", func(t *testing.T) {
		t.Parallel()
		in, journal := makeTestUpload(t, bytes.Repeat([]byte("Z"), minChunkSize*2), "key-bad-range")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "http://"+r.Host+"/session/bad")
				return
			}
			if r.Method == http.MethodPut && strings.HasPrefix(r.Header.Get("Content-Range"), "bytes */") {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Range", "bytes=invalid-range")
			w.WriteHeader(http.StatusPermanentRedirect)
		}))
		defer server.Close()

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithUploadURL(server.URL),
			WithJournal(journal),
			WithChunkSize(minChunkSize),
			WithRetryPolicy(1, time.Millisecond, time.Millisecond),
		)
		_, err := client.UploadVideo(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "Range") {
			t.Fatalf("expected invalid Range error, got %v", err)
		}
	})
}

func TestUploadVideo_ConcurrentSameAndDifferentKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.mp4")
	fileB := filepath.Join(dir, "b.mp4")
	if err := os.WriteFile(fileA, []byte("video-A-payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("video-B-payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(dir, "journal.json")

	var insertsA, insertsB atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "Title A") {
				insertsA.Add(1)
				time.Sleep(20 * time.Millisecond)
				w.Header().Set("Location", "http://"+r.Host+"/session/A")
			} else {
				insertsB.Add(1)
				w.Header().Set("Location", "http://"+r.Host+"/session/B")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/A") {
			fmt.Fprint(w, `{"id":"vid-A","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		} else {
			fmt.Fprint(w, `{"id":"vid-B","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		}
	}))
	defer server.Close()

	var wg sync.WaitGroup
	resultsA := make([]UploadResult, 4)
	errsA := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			j, err := OpenJournal(journalPath)
			if err != nil {
				errsA[idx] = err
				return
			}
			c := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(j))
			resultsA[idx], errsA[idx] = c.UploadVideo(context.Background(), UploadInput{
				FilePath:       fileA,
				Title:          "Title A",
				IdempotencyKey: "same-key-A",
			})
		}(i)
	}
	wg.Add(1)
	var resB UploadResult
	var errB error
	go func() {
		defer wg.Done()
		j, err := OpenJournal(journalPath)
		if err != nil {
			errB = err
			return
		}
		c := NewClient(nil, WithHTTPClient(server.Client()), WithUploadURL(server.URL), WithJournal(j))
		resB, errB = c.UploadVideo(context.Background(), UploadInput{
			FilePath:       fileB,
			Title:          "Title B",
			IdempotencyKey: "other-key-B",
		})
	}()
	wg.Wait()

	for i, err := range errsA {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
		if resultsA[i].VideoID != "vid-A" {
			t.Fatalf("goroutine %d got VideoID %q", i, resultsA[i].VideoID)
		}
	}
	if errB != nil || resB.VideoID != "vid-B" {
		t.Fatalf("key B failed: res=%+v err=%v", resB, errB)
	}
	if insertsA.Load() != 1 {
		t.Fatalf("expected exactly 1 insert for same-key-A, got %d", insertsA.Load())
	}
	if insertsB.Load() != 1 {
		t.Fatalf("expected 1 insert for other-key-B, got %d", insertsB.Load())
	}

	jFinal, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := jFinal.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 journal entries without clobbering, got %d", len(entries))
	}
}

type deadlineCheckTransport func(*http.Request) (*http.Response, error)

func (f deadlineCheckTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUploadVideo_AllRequestsHaveDeadline(t *testing.T) {
	t.Parallel()

	in, journal := makeTestUpload(t, []byte("deadline-test"), "key-deadline")
	var missing atomic.Int32
	transport := deadlineCheckTransport(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			missing.Add(1)
		}
		headers := make(http.Header)
		body := `{"id":"vid-deadline"}`
		if r.Method == http.MethodPost {
			headers.Set("Location", "http://example.invalid/session")
			body = ""
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     headers,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})
	client := NewClient(nil,
		WithHTTPClient(&http.Client{Transport: transport}),
		WithJournal(journal),
		WithTimeout(2*time.Second),
	)
	if _, err := client.UploadVideo(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if missing.Load() > 0 {
		t.Fatalf("%d upload request(s) had no deadline", missing.Load())
	}
}

func mustSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum, err := sha256File(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}
