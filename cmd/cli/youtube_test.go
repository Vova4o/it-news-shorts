package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/it-news-shorts/it-news-shorts/internal/youtube"
)

func TestCLI_ExpiredTokenRefreshesAutomaticallyWithLocalEndpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token.json")
	journalPath := filepath.Join(dir, "journal.json")

	store, err := youtube.NewTokenStore(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&oauth2.Token{
		AccessToken:  "expired-access",
		RefreshToken: "valid-refresh",
		Expiry:       time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var tokenCalls atomic.Int32
	var gotAuthHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("client_id") != "cli-client-id" || r.FormValue("client_secret") != "cli-client-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":"invalid_client"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"fresh-access-token","token_type":"Bearer","expires_in":3600}`)
		case "/channels":
			gotAuthHeader = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"items":[{"id":"UC-cli-1","snippet":{"title":"CLI Test Channel"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	env := map[string]string{
		"YOUTUBE_CLIENT_ID":     "cli-client-id",
		"YOUTUBE_CLIENT_SECRET": "cli-client-secret",
		"YOUTUBE_TOKEN_FILE":    tokenPath,
		"YOUTUBE_JOURNAL_FILE":  journalPath,
	}
	getenv := func(k string) string { return env[k] }

	cfg := youtube.OAuthConfig{
		ClientID:     env["YOUTUBE_CLIENT_ID"],
		ClientSecret: env["YOUTUBE_CLIENT_SECRET"],
		Scopes:       []string{youtube.ScopeForceSSL},
		Endpoint: oauth2.Endpoint{
			AuthURL:   server.URL + "/auth",
			TokenURL:  server.URL + "/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}

	client, _, err := newYouTubeClientWithEnv(context.Background(), getenv, &cfg, youtube.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("newYouTubeClientWithEnv failed: %v", err)
	}
	ch, err := client.VerifyChannel(context.Background(), "UC-cli-1")
	if err != nil {
		t.Fatalf("VerifyChannel failed: %v", err)
	}
	if ch.ID != "UC-cli-1" || tokenCalls.Load() != 1 || gotAuthHeader != "Bearer fresh-access-token" {
		t.Fatalf("unexpected refresh behavior: ch=%+v tokenCalls=%d auth=%q", ch, tokenCalls.Load(), gotAuthHeader)
	}

	// Verify refreshed token was persisted and refresh token was kept.
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "fresh-access-token" || saved.RefreshToken != "valid-refresh" {
		t.Fatalf("unexpected persisted token: %+v", saved)
	}

	// Verify VerifyChannel rejects wrong channel ID.
	if _, err := client.VerifyChannel(context.Background(), "UC-other"); !errors.Is(err, youtube.ErrChannelMismatch) {
		t.Fatalf("expected ErrChannelMismatch, got %v", err)
	}
}

func TestCLI_NewYouTubeClientRequiresCredentials(t *testing.T) {
	t.Parallel()

	_, _, err := newYouTubeClientWithEnv(context.Background(), func(string) string { return "" }, nil)
	if !errors.Is(err, youtube.ErrCredentialsMissing) {
		t.Fatalf("expected ErrCredentialsMissing, got %v", err)
	}
}

func TestCLI_DeriveIdempotencyKey_ContentBasedAndPathIndependent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(file, []byte("identical-video-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	keyRel, err := deriveIdempotencyKey(context.Background(), "./clip.mp4", "Integration Test")
	if err != nil {
		t.Fatal(err)
	}
	keyAbs, err := deriveIdempotencyKey(context.Background(), file, "Integration Test")
	if err != nil {
		t.Fatal(err)
	}
	if keyRel != keyAbs {
		t.Fatalf("key changed between relative and absolute path: %q != %q", keyRel, keyAbs)
	}

	// Touch mtime; key must remain identical.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	keyAfterTouch, err := deriveIdempotencyKey(context.Background(), file, "Integration Test")
	if err != nil {
		t.Fatal(err)
	}
	if keyAfterTouch != keyAbs {
		t.Fatalf("key changed after mtime update: %q != %q", keyAfterTouch, keyAbs)
	}

	// Change content; key must change.
	if err := os.WriteFile(file, []byte("different-video-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDiff, err := deriveIdempotencyKey(context.Background(), file, "Integration Test")
	if err != nil {
		t.Fatal(err)
	}
	if keyDiff == keyAbs {
		t.Fatal("key did not change when file content changed")
	}

	// Cancelled context and non-regular file must fail.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := deriveIdempotencyKey(cctx, file, "Integration Test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := deriveIdempotencyKey(context.Background(), dir, "Integration Test"); err == nil {
		t.Fatal("expected error for directory path")
	}

	// FIFO must fail immediately without blocking.
	fifoPath := filepath.Join(t.TempDir(), "pipe.mp4")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := deriveIdempotencyKey(context.Background(), fifoPath, "Integration Test")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for FIFO path")
		}
	case <-time.After(time.Second):
		t.Fatal("deriveIdempotencyKey blocked on FIFO")
	}
}

func TestCLI_UploadDryRunAndPublicRefusal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(file, []byte("test-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Refuses public upload.
	err := youtubeUpload(context.Background(), []string{
		"--file", file,
		"--title", "Test",
		"--privacy", "public",
		"--confirm-upload",
	})
	if err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("expected public upload refusal, got %v", err)
	}

	// Without --confirm-upload, runs dry-run without needing OAuth credentials.
	if err := youtubeUpload(context.Background(), []string{
		"--file", file,
		"--title", "Test Dry Run",
	}); err != nil {
		t.Fatalf("dry-run upload failed: %v", err)
	}
}
