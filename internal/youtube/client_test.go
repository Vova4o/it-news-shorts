package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

func TestSchedulePublication_ValidDryRunRescheduleAndGuards(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)

	t.Run("valid future publish preserves mutable status fields", func(t *testing.T) {
		t.Parallel()
		var putBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				fmt.Fprint(w, `{"items":[{"id":"vid-1","status":{"privacyStatus":"private","uploadStatus":"uploaded","license":"youtube","embeddable":true,"publicStatsViewable":false,"selfDeclaredMadeForKids":false}}]}`)
				return
			}
			if r.Method == http.MethodPut {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &putBody)
				fmt.Fprint(w, `{}`)
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
		}))
		defer server.Close()

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithBaseURL(server.URL),
			WithClock(func() time.Time { return now }, nil),
		)
		plan, err := client.SchedulePublication(context.Background(), "vid-1", future, false)
		if err != nil {
			t.Fatalf("SchedulePublication failed: %v", err)
		}
		if !plan.Scheduled() || !plan.PublishAt.Equal(future) {
			t.Fatalf("unexpected plan: %+v", plan)
		}
		statusMap := putBody["status"].(map[string]any)
		if statusMap["privacyStatus"] != "private" ||
			statusMap["publishAt"] != future.Format(time.RFC3339) ||
			statusMap["license"] != "youtube" ||
			statusMap["embeddable"] != true ||
			statusMap["publicStatsViewable"] != false ||
			statusMap["selfDeclaredMadeForKids"] != false {
			t.Fatalf("mutable status fields not preserved: %+v", statusMap)
		}
	})

	t.Run("dry-run sends no PUT request", func(t *testing.T) {
		t.Parallel()
		var puts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				puts.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[{"id":"vid-dry","status":{"privacyStatus":"private"}}]}`)
		}))
		defer server.Close()

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithBaseURL(server.URL),
			WithClock(func() time.Time { return now }, nil),
		)
		plan, err := client.SchedulePublication(context.Background(), "vid-dry", future, true)
		if err != nil {
			t.Fatalf("dry-run failed: %v", err)
		}
		if !plan.DryRun || plan.Scheduled() || puts.Load() != 0 {
			t.Fatalf("dry-run sent %d PUT(s) or wrong flags: %+v", puts.Load(), plan)
		}
	})

	t.Run("reschedule already-scheduled private video", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				fmt.Fprintf(w, `{"items":[{"id":"vid-re","status":{"privacyStatus":"private","publishAt":%q}}]}`, now.Add(time.Hour).Format(time.RFC3339))
				return
			}
			fmt.Fprint(w, `{}`)
		}))
		defer server.Close()

		client := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithBaseURL(server.URL),
			WithClock(func() time.Time { return now }, nil),
		)
		if _, err := client.SchedulePublication(context.Background(), "vid-re", future, false); err != nil {
			t.Fatalf("reschedule failed: %v", err)
		}
	})

	t.Run("past time, already public, unlisted, and API error", func(t *testing.T) {
		t.Parallel()
		clientNoServer := NewClient(nil, WithClock(func() time.Time { return now }, nil))
		if _, err := clientNoServer.SchedulePublication(context.Background(), "vid", now.Add(-time.Minute), false); !errors.Is(err, ErrPublishAtNotFuture) {
			t.Fatalf("expected ErrPublishAtNotFuture, got %v", err)
		}

		for _, tc := range []struct {
			name    string
			privacy string
			putCode int
			wantErr error
		}{
			{name: "public", privacy: "public", wantErr: ErrAlreadyPublished},
			{name: "unlisted", privacy: "unlisted", wantErr: ErrVideoNotPrivate},
		} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"items":[{"id":"v","status":{"privacyStatus":%q}}]}`, tc.privacy)
			}))
			c := NewClient(nil, WithHTTPClient(server.Client()), WithBaseURL(server.URL), WithClock(func() time.Time { return now }, nil))
			_, err := c.SchedulePublication(context.Background(), "v", future, false)
			server.Close()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s: expected %v, got %v", tc.name, tc.wantErr, err)
			}
		}

		// API error on PUT.
		serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				fmt.Fprint(w, `{"items":[{"id":"v","status":{"privacyStatus":"private"}}]}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"invalid publishAt"}}`)
		}))
		defer serverErr.Close()
		cErr := NewClient(nil, WithHTTPClient(serverErr.Client()), WithBaseURL(serverErr.URL), WithClock(func() time.Time { return now }, nil))
		var apiErr *APIError
		if _, err := cErr.SchedulePublication(context.Background(), "v", future, false); !errors.As(err, &apiErr) {
			t.Fatalf("expected *APIError, got %v", err)
		}
	})
}

func TestWaitProcessing_AllStatesAndTimeout(t *testing.T) {
	t.Parallel()

	t.Run("uploading/processing then succeeded", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if calls.Add(1) == 1 {
				fmt.Fprint(w, `{"items":[{"id":"v1","status":{"uploadStatus":"uploaded","privacyStatus":"private"},"processingDetails":{"processingStatus":"processing"}}]}`)
				return
			}
			fmt.Fprint(w, `{"items":[{"id":"v1","status":{"uploadStatus":"processed","privacyStatus":"private"},"processingDetails":{"processingStatus":"succeeded"}}]}`)
		}))
		defer server.Close()

		c := NewClient(nil,
			WithHTTPClient(server.Client()),
			WithBaseURL(server.URL),
			WithClock(time.Now, func(context.Context, time.Duration) error { return nil }),
		)
		st, err := c.WaitProcessing(context.Background(), "v1", time.Millisecond, time.Second)
		if err != nil || !st.Processed() || calls.Load() != 2 {
			t.Fatalf("unexpected WaitProcessing result: st=%+v err=%v calls=%d", st, err, calls.Load())
		}
	})

	t.Run("terminal failure states return immediately with reason", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			body string
			want string
		}{
			{
				name: "processing failed",
				body: `{"items":[{"id":"v","processingDetails":{"processingStatus":"failed","processingFailureReason":"codecUnsupported"}}]}`,
				want: "codecUnsupported",
			},
			{
				name: "processing terminated",
				body: `{"items":[{"id":"v","processingDetails":{"processingStatus":"terminated","processingFailureReason":"transcodeAborted"}}]}`,
				want: "transcodeAborted",
			},
			{
				name: "video rejected",
				body: `{"items":[{"id":"v","status":{"uploadStatus":"rejected","rejectionReason":"duplicate"}}]}`,
				want: "duplicate",
			},
			{
				name: "upload failed",
				body: `{"items":[{"id":"v","status":{"uploadStatus":"failed","failureReason":"emptyFile"}}]}`,
				want: "emptyFile",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, tc.body)
				}))
				defer server.Close()

				c := NewClient(nil, WithHTTPClient(server.Client()), WithBaseURL(server.URL))
				_, err := c.WaitProcessing(context.Background(), "v", time.Second, 10*time.Second)
				if err == nil || !strings.Contains(err.Error(), tc.want) || calls.Load() != 1 {
					t.Fatalf("expected immediate error containing %q (calls=%d), got %v", tc.want, calls.Load(), err)
				}
			})
		}
	})

	t.Run("video not found and insufficient scope", func(t *testing.T) {
		t.Parallel()
		serverNotFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[]}`)
		}))
		defer serverNotFound.Close()
		c1 := NewClient(nil, WithHTTPClient(serverNotFound.Client()), WithBaseURL(serverNotFound.URL))
		if _, err := c1.GetVideo(context.Background(), "missing"); !errors.Is(err, ErrVideoNotFound) {
			t.Fatalf("expected ErrVideoNotFound, got %v", err)
		}

		serverForbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":403,"message":"insufficient","errors":[{"reason":"insufficientPermissions"}]}}`)
		}))
		defer serverForbidden.Close()
		c2 := NewClient(nil, WithHTTPClient(serverForbidden.Client()), WithBaseURL(serverForbidden.URL))
		if _, err := c2.GetVideo(context.Background(), "v"); !errors.Is(err, ErrInsufficientScope) {
			t.Fatalf("expected ErrInsufficientScope, got %v", err)
		}
	})

	t.Run("WaitProcessing timeout bounds real HTTP wait", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		defer server.Close()

		c := NewClient(nil, WithHTTPClient(server.Client()), WithBaseURL(server.URL))
		start := time.Now()
		_, err := c.WaitProcessing(context.Background(), "v", 10*time.Millisecond, 60*time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("WaitProcessing exceeded timeout: %s", time.Since(start))
		}
	})
}

func TestAdapter_UploadThenSchedulePreservesDomainIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	videoPath := filepath.Join(dir, "short.mp4")
	if err := os.WriteFile(videoPath, []byte("short-mp4-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/videos":
			w.Header().Set("Location", "http://"+r.Host+"/session/1")
		case r.Method == http.MethodPut && r.URL.Path == "/session/1":
			fmt.Fprint(w, `{"id":"yt-999","status":{"uploadStatus":"uploaded","privacyStatus":"private"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/videos":
			fmt.Fprint(w, `{"items":[{"id":"yt-999","status":{"privacyStatus":"private"}}]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/videos":
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	adapterClock := time.Date(2026, 11, 13, 11, 30, 0, 0, time.UTC)
	client := NewClient(nil,
		WithHTTPClient(server.Client()),
		WithBaseURL(server.URL),
		WithUploadURL(server.URL),
		WithJournal(journal),
		WithClock(func() time.Time { return now }, nil),
	)
	adapter := NewAdapter(client)
	adapter.Now = func() time.Time { return adapterClock }

	uploadedPub, err := adapter.Upload(context.Background(), UploadRequest{
		VideoJobID: domain.VideoJobID("job-42"),
		Asset: domain.MediaAsset{
			ID:         domain.MediaAssetID("asset-7"),
			VideoJobID: domain.VideoJobID("job-42"),
			Kind:       domain.MediaRendered,
			URI:        "file://" + videoPath,
			MIMEType:   "video/mp4",
			SizeBytes:  15,
			Duration:   7 * time.Second,
			Checksum:   "abc",
			Provider:   "ffmpeg",
			CreatedAt:  now,
		},
		Title:       "IT News Short",
		Description: "Desc",
		Privacy:     PrivacyPrivate,
		Language:    domain.LanguageRussian,
	})
	if err != nil {
		t.Fatalf("Adapter.Upload failed: %v", err)
	}
	if err := uploadedPub.Validate(); err != nil {
		t.Fatalf("uploaded publication invalid: %v", err)
	}

	// Simulate scheduling from a fresh Adapter sharing the same Journal with a different clock.
	adapter2 := NewAdapter(client)
	adapter2.Now = func() time.Time { return adapterClock.Add(time.Hour) }
	scheduledPub, err := adapter2.Schedule(context.Background(), ScheduleRequest{
		PublicationID: uploadedPub.ID,
		PublishAt:     now.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Adapter.Schedule failed: %v", err)
	}
	if err := scheduledPub.Validate(); err != nil {
		t.Fatalf("scheduled publication invalid: %v", err)
	}
	if scheduledPub.VideoJobID != uploadedPub.VideoJobID {
		t.Fatalf("VideoJobID mismatch: got %q, want %q", scheduledPub.VideoJobID, uploadedPub.VideoJobID)
	}
	if scheduledPub.IdempotencyKey != uploadedPub.IdempotencyKey {
		t.Fatalf("IdempotencyKey mismatch: got %q, want %q", scheduledPub.IdempotencyKey, uploadedPub.IdempotencyKey)
	}
	if !scheduledPub.CreatedAt.Equal(uploadedPub.CreatedAt) {
		t.Fatalf("publication created_at changed between upload and schedule: %v -> %v", uploadedPub.CreatedAt, scheduledPub.CreatedAt)
	}
}

func TestAdapter_KeyEncodesDistinctComponents(t *testing.T) {
	t.Parallel()

	pairs := []UploadRequest{
		{VideoJobID: domain.VideoJobID("job/a"), Asset: domain.MediaAsset{ID: domain.MediaAssetID("b")}},
		{VideoJobID: domain.VideoJobID("job"), Asset: domain.MediaAsset{ID: domain.MediaAssetID("a/b")}},
		{VideoJobID: domain.VideoJobID("job+a"), Asset: domain.MediaAsset{ID: domain.MediaAssetID("b")}},
		{VideoJobID: domain.VideoJobID("joba"), Asset: domain.MediaAsset{ID: domain.MediaAssetID("b")}},
		{VideoJobID: domain.VideoJobID("задача-1"), Asset: domain.MediaAsset{ID: domain.MediaAssetID("ролик+1")}},
		{VideoJobID: domain.VideoJobID("задача-2"), Asset: domain.MediaAsset{ID: domain.MediaAssetID("ролик+1")}},
	}
	seen := make(map[string]bool, len(pairs))
	for _, req := range pairs {
		key, err := idempotencyKeyFor(req)
		if err != nil {
			t.Fatalf("idempotencyKeyFor(%+v) failed: %v", req, err)
		}
		if len(key) > 128 {
			t.Fatalf("key %q exceeds 128 chars", key)
		}
		if seen[key] {
			t.Fatalf("different job/asset components produced duplicate key %q", key)
		}
		seen[key] = true
	}
}

func TestSchedulePublication_RejectsElapsedPublishAtAfterGetAndBeforeRetryPut(t *testing.T) {
	t.Parallel()

	t.Run("publishAt elapses during GET", func(t *testing.T) {
		t.Parallel()
		clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		future := clock.Add(time.Second)
		var puts atomic.Int32
		transport := staticRespTransport(func(r *http.Request) (*http.Response, error) {
			body := `{}`
			if r.Method == http.MethodGet {
				clock = future.Add(time.Second)
				body = `{"items":[{"id":"video","status":{"privacyStatus":"private"}}]}`
			} else {
				puts.Add(1)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		})
		client := NewClient(nil,
			WithHTTPClient(&http.Client{Transport: transport}),
			WithClock(func() time.Time { return clock }, nil),
		)
		_, err := client.SchedulePublication(context.Background(), "video", future, false)
		if puts.Load() != 0 || !errors.Is(err, ErrPublishAtNotFuture) {
			t.Fatalf("elapsed publishAt sent in %d PUT(s), err=%v", puts.Load(), err)
		}
	})

	t.Run("publishAt elapses during retry backoff before second PUT", func(t *testing.T) {
		t.Parallel()
		clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		future := clock.Add(5 * time.Second)
		var puts atomic.Int32
		transport := staticRespTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"items":[{"id":"video","status":{"privacyStatus":"private"}}]}`)),
					Request:    r,
				}, nil
			}
			puts.Add(1)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		})
		client := NewClient(nil,
			WithHTTPClient(&http.Client{Transport: transport}),
			WithRetryPolicy(3, time.Second, time.Second),
			WithClock(func() time.Time { return clock }, func(context.Context, time.Duration) error {
				clock = future.Add(time.Second)
				return nil
			}),
		)
		_, err := client.SchedulePublication(context.Background(), "video", future, false)
		if puts.Load() != 1 || !errors.Is(err, ErrPublishAtNotFuture) {
			t.Fatalf("expected 1 PUT and ErrPublishAtNotFuture, got puts=%d err=%v", puts.Load(), err)
		}
	})

	t.Run("sub-second publishAt truncating to current second is rejected", func(t *testing.T) {
		t.Parallel()
		clock := time.Date(2026, 11, 13, 12, 0, 0, int(500*time.Millisecond), time.UTC)
		publishAt := time.Date(2026, 11, 13, 12, 0, 0, int(900*time.Millisecond), time.UTC)
		client := NewClient(nil, WithClock(func() time.Time { return clock }, nil))
		if _, err := client.SchedulePublication(context.Background(), "video", publishAt, false); !errors.Is(err, ErrPublishAtNotFuture) {
			t.Fatalf("expected ErrPublishAtNotFuture for truncated sub-second timestamp, got %v", err)
		}
	})
}

func TestAdapter_UnknownPublicationNeverUpdatesYouTube(t *testing.T) {
	t.Parallel()

	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"video","status":{"privacyStatus":"private"}}]}`)
	}))
	defer server.Close()

	// Without journal.
	cNoJournal := NewClient(nil, WithHTTPClient(server.Client()), WithBaseURL(server.URL))
	if _, err := NewAdapter(cNoJournal).Schedule(context.Background(), ScheduleRequest{
		PublicationID: domain.PublicationID("pub-video"),
		PublishAt:     time.Now().Add(time.Hour),
	}); err == nil || puts.Load() != 0 {
		t.Fatalf("expected error and 0 PUTs without journal, got puts=%d err=%v", puts.Load(), err)
	}

	// With empty journal (unknown publication).
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	cWithJournal := NewClient(nil, WithHTTPClient(server.Client()), WithBaseURL(server.URL), WithJournal(j))
	if _, err := NewAdapter(cWithJournal).Schedule(context.Background(), ScheduleRequest{
		PublicationID: domain.PublicationID("pub-video"),
		PublishAt:     time.Now().Add(time.Hour),
	}); err == nil || puts.Load() != 0 {
		t.Fatalf("expected error and 0 PUTs for unknown publication, got puts=%d err=%v", puts.Load(), err)
	}

	// With locally known public video in journal: must refuse before PUT.
	if err := j.Put(context.Background(), JournalEntry{
		Key:        "upload/job-1/v1",
		State:      UploadStateCompleted,
		VideoID:    "pub-already",
		Privacy:    string(PrivacyPublic),
		VideoJobID: "job-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAdapter(cWithJournal).Schedule(context.Background(), ScheduleRequest{
		PublicationID: domain.PublicationID("pub-pub-already"),
		PublishAt:     time.Now().Add(time.Hour),
	}); !errors.Is(err, ErrAlreadyPublished) || puts.Load() != 0 {
		t.Fatalf("expected ErrAlreadyPublished and 0 PUTs, got puts=%d err=%v", puts.Load(), err)
	}
}

type closeTrackingBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type staticRespTransport func(*http.Request) (*http.Response, error)

func (f staticRespTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClient_ParseAPIErrorClosesBodyAndClassifiesErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		status         int
		body           string
		wantScopeErr   bool
		wantStatusCode int
	}{
		{
			name:           "401 unauthorized is APIError not ErrInsufficientScope",
			status:         http.StatusUnauthorized,
			body:           `{"error":{"code":401,"message":"invalid credentials"}}`,
			wantScopeErr:   false,
			wantStatusCode: http.StatusUnauthorized,
		},
		{
			name:           "403 insufficientPermissions is ErrInsufficientScope and APIError",
			status:         http.StatusForbidden,
			body:           `{"error":{"code":403,"message":"insufficient","errors":[{"reason":"insufficientPermissions"}]}}`,
			wantScopeErr:   true,
			wantStatusCode: http.StatusForbidden,
		},
		{
			name:           "403 quotaExceeded is APIError not ErrInsufficientScope",
			status:         http.StatusForbidden,
			body:           `{"error":{"code":403,"message":"quota","errors":[{"reason":"quotaExceeded"}]}}`,
			wantScopeErr:   false,
			wantStatusCode: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var closed atomic.Bool
			c := NewClient(nil, WithHTTPClient(&http.Client{
				Transport: staticRespTransport(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: tc.status,
						Header:     make(http.Header),
						Body:       closeTrackingBody{Reader: strings.NewReader(tc.body), closed: &closed},
						Request:    r,
					}, nil
				}),
			}))
			_, err := c.GetVideo(context.Background(), "vid-1")
			if err == nil {
				t.Fatal("expected error")
			}
			if !closed.Load() {
				t.Fatal("response body was not closed on error")
			}
			if errors.Is(err, ErrInsufficientScope) != tc.wantScopeErr {
				t.Fatalf("errors.Is(ErrInsufficientScope) = %v, want %v (err=%v)", !tc.wantScopeErr, tc.wantScopeErr, err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.wantStatusCode {
				t.Fatalf("expected *APIError with status %d, got %v", tc.wantStatusCode, err)
			}
		})
	}
}
