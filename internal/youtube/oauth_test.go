package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestAuthorizer_AuthorizeSuccessAndTokenStoreMode(t *testing.T) {
	t.Parallel()

	var gotCode, gotVerifier string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotCode = r.FormValue("code")
		gotVerifier = r.FormValue("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"test-access","refresh_token":"test-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer server.Close()

	store, err := NewTokenStore(filepath.Join(t.TempDir(), "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	auth := &Authorizer{
		Store: store,
		Config: OAuthConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			Scopes:       []string{ScopeForceSSL},
			Endpoint: oauth2.Endpoint{
				AuthURL:   server.URL + "/auth",
				TokenURL:  server.URL + "/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		HTTPClient: server.Client(),
		OpenBrowser: func(raw string) error {
			parsed, err := url.Parse(raw)
			if err != nil {
				return err
			}
			cb, err := url.Parse(parsed.Query().Get("redirect_uri"))
			if err != nil {
				return err
			}
			q := cb.Query()
			q.Set("state", parsed.Query().Get("state"))
			q.Set("code", "auth-code-123")
			cb.RawQuery = q.Encode()

			resp, err := http.Get(cb.String())
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			// Second stray callback must not block or hang.
			resp2, err := http.Get(cb.String())
			if err != nil {
				return err
			}
			return resp2.Body.Close()
		},
	}

	ch, tok, err := auth.Authorize(context.Background(), AuthorizeOptions{
		ExpectedChannelID: "UC-expected",
		CheckChannel: func(_ context.Context, _ *oauth2.Token) (Channel, error) {
			return Channel{ID: "UC-expected", Title: "IT News"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Authorize failed: %v", err)
	}
	if ch.ID != "UC-expected" || tok.RefreshToken != "test-refresh" {
		t.Fatalf("unexpected channel/token: ch=%+v", ch)
	}
	if gotCode != "auth-code-123" || gotVerifier == "" {
		t.Fatalf("missing code or PKCE verifier: code=%q verifier=%q", gotCode, gotVerifier)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", info.Mode().Perm())
	}
}

func TestAuthorizer_ChannelMismatchPreservesStoredCredentials(t *testing.T) {
	t.Parallel()

	store, err := NewTokenStore(filepath.Join(t.TempDir(), "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&oauth2.Token{
		AccessToken:  "old-local-test-token",
		RefreshToken: "old-local-test-refresh",
	}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"wrong-channel-token","refresh_token":"wrong-channel-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer server.Close()

	auth := &Authorizer{
		Store: store,
		Config: OAuthConfig{
			ClientID:     "local-test-id",
			ClientSecret: "local-test-secret",
			Endpoint: oauth2.Endpoint{
				AuthURL:   server.URL + "/auth",
				TokenURL:  server.URL + "/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		HTTPClient: server.Client(),
		OpenBrowser: func(raw string) error {
			parsed, err := url.Parse(raw)
			if err != nil {
				return err
			}
			cb, err := url.Parse(parsed.Query().Get("redirect_uri"))
			if err != nil {
				return err
			}
			q := cb.Query()
			q.Set("state", parsed.Query().Get("state"))
			q.Set("code", "local-test-code")
			cb.RawQuery = q.Encode()
			resp, err := http.Get(cb.String())
			if err != nil {
				return err
			}
			return resp.Body.Close()
		},
	}

	_, _, err = auth.Authorize(context.Background(), AuthorizeOptions{
		ExpectedChannelID: "expected",
		CheckChannel: func(context.Context, *oauth2.Token) (Channel, error) {
			return Channel{ID: "wrong"}, nil
		},
	})
	if !errors.Is(err, ErrChannelMismatch) {
		t.Fatalf("expected ErrChannelMismatch, got %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccessToken != "old-local-test-token" || loaded.RefreshToken != "old-local-test-refresh" {
		t.Fatalf("channel mismatch overwrote prior credentials")
	}
}

func TestAuthorizer_CallbackErrorsAndStateMismatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		query   func(state string) url.Values
		wantErr error
	}{
		{
			name: "state mismatch",
			query: func(_ string) url.Values {
				v := url.Values{}
				v.Set("state", "wrong-state")
				v.Set("code", "abc")
				return v
			},
			wantErr: ErrStateMismatch,
		},
		{
			name: "provider error",
			query: func(state string) url.Values {
				v := url.Values{}
				v.Set("state", state)
				v.Set("error", "access_denied")
				v.Set("error_description", "user cancelled")
				return v
			},
			wantErr: ErrAuthorizationDenied,
		},
		{
			name: "missing code",
			query: func(state string) url.Values {
				v := url.Values{}
				v.Set("state", state)
				return v
			},
			wantErr: ErrAuthorizationDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := NewTokenStore(filepath.Join(t.TempDir(), "token.json"))
			if err != nil {
				t.Fatal(err)
			}
			auth := &Authorizer{
				Store: store,
				Config: OAuthConfig{
					ClientID:     "cid",
					ClientSecret: "csec",
					Endpoint:     googleEndpoint,
				},
				OpenBrowser: func(raw string) error {
					parsed, _ := url.Parse(raw)
					cb, _ := url.Parse(parsed.Query().Get("redirect_uri"))
					cb.RawQuery = tc.query(parsed.Query().Get("state")).Encode()
					resp, err := http.Get(cb.String())
					if err != nil {
						return err
					}
					return resp.Body.Close()
				},
			}
			_, _, err = auth.Authorize(context.Background(), AuthorizeOptions{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestAuthorizer_MissingCredentialsAndCancellation(t *testing.T) {
	t.Parallel()

	if _, err := NewOAuthConfig("", "secret"); !errors.Is(err, ErrCredentialsMissing) {
		t.Fatalf("expected ErrCredentialsMissing, got %v", err)
	}

	store, err := NewTokenStore(filepath.Join(t.TempDir(), "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	authNoConfig := &Authorizer{Store: store}
	if _, err := authNoConfig.TokenSource(context.Background()); !errors.Is(err, ErrCredentialsMissing) {
		t.Fatalf("expected ErrCredentialsMissing for incomplete config, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	auth := &Authorizer{
		Store: store,
		Config: OAuthConfig{
			ClientID:     "cid",
			ClientSecret: "csec",
			Endpoint:     googleEndpoint,
		},
		OpenBrowser: func(_ string) error {
			cancel()
			return nil
		},
	}
	_, _, err = auth.Authorize(ctx, AuthorizeOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestTokenSource_RefreshPreservesRefreshTokenAndHandlesInvalidGrant(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"access_token":"refreshed-access","token_type":"Bearer","expires_in":3600}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	}))
	defer server.Close()

	store, err := NewTokenStore(filepath.Join(t.TempDir(), "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	expired := &oauth2.Token{
		AccessToken:  "old-access",
		RefreshToken: "keep-refresh",
		Expiry:       time.Now().Add(-time.Hour),
	}
	if err := store.Save(expired); err != nil {
		t.Fatal(err)
	}

	auth := &Authorizer{
		Store: store,
		Config: OAuthConfig{
			ClientID:     "cid",
			ClientSecret: "csec",
			Endpoint: oauth2.Endpoint{
				AuthURL:   server.URL + "/auth",
				TokenURL:  server.URL + "/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		HTTPClient: server.Client(),
	}
	src, err := auth.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Concurrent Token() calls must be race-free and preserve refresh token.
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := src.Token()
			if err != nil {
				t.Errorf("Token failed: %v", err)
				return
			}
			if tok.AccessToken != "refreshed-access" {
				t.Errorf("got access token %q", tok.AccessToken)
			}
		}()
	}
	wg.Wait()

	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "keep-refresh" {
		t.Fatalf("refresh token was lost: got %q", saved.RefreshToken)
	}

	// Now force another refresh that returns invalid_grant.
	if err := store.Save(&oauth2.Token{
		AccessToken:  "expired-again",
		RefreshToken: "revoked-refresh",
		Expiry:       time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	src2, err := auth.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = src2.Token()
	if !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Fatalf("expected ErrRefreshTokenInvalid, got %v", err)
	}
}

func TestTokenStore_RejectsRepoAndSymlinksAndPreservesOnCorruptLoad(t *testing.T) {
	t.Parallel()

	// Create a fake git repo inside TempDir.
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTokenStore(filepath.Join(root, "token.json")); err == nil {
		t.Fatal("expected error for token path inside git repo")
	}

	// Symlink outside repo pointing into git repo must also be rejected.
	outside := t.TempDir()
	linkDir := filepath.Join(outside, "link-to-repo")
	if err := os.Symlink(root, linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTokenStore(filepath.Join(linkDir, "sub", "token.json")); err == nil {
		t.Fatal("expected error for symlinked path inside git repo")
	}

	// Corrupt existing token file must not be silently overwritten when saving without refresh token.
	validPath := filepath.Join(outside, "token.json")
	store, err := NewTokenStore(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(validPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&oauth2.Token{AccessToken: "new-only"}); err == nil {
		t.Fatal("expected Save to fail when existing token file cannot be parsed")
	}
	// Ensure valid JSON save with RefreshToken works and writes valid JSON.
	if err := store.Save(&oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed oauth2.Token
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.RefreshToken != "r" {
		t.Fatalf("unexpected saved token: %v", err)
	}

	// Insecure 0644 token file must be rejected on Load.
	if err := os.Chmod(validPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("expected Load to reject token file with mode 0644")
	}
}

func TestAuthorizer_NewChannelAuthNeverReusesOldChannelRefresh(t *testing.T) {
	t.Parallel()

	store, err := NewTokenStore(filepath.Join(t.TempDir(), "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&oauth2.Token{
		AccessToken:  "old-channel-test-access",
		RefreshToken: "old-channel-test-refresh",
	}); err != nil {
		t.Fatal(err)
	}

	var returnRefresh atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if returnRefresh.Load() {
			fmt.Fprint(w, `{"access_token":"new-channel-test-access","refresh_token":"new-channel-test-refresh","token_type":"Bearer","expires_in":3600}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"new-channel-test-access","token_type":"Bearer","expires_in":3600}`)
	}))
	defer server.Close()

	auth := &Authorizer{
		Store: store,
		Config: OAuthConfig{
			ClientID:     "test-client",
			ClientSecret: "test-secret",
			Endpoint: oauth2.Endpoint{
				AuthURL:   server.URL + "/auth",
				TokenURL:  server.URL + "/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		HTTPClient: server.Client(),
		OpenBrowser: func(raw string) error {
			parsed, err := url.Parse(raw)
			if err != nil {
				return err
			}
			cb, err := url.Parse(parsed.Query().Get("redirect_uri"))
			if err != nil {
				return err
			}
			q := cb.Query()
			q.Set("state", parsed.Query().Get("state"))
			q.Set("code", "test-code")
			cb.RawQuery = q.Encode()
			resp, err := http.Get(cb.String())
			if err != nil {
				return err
			}
			return resp.Body.Close()
		},
	}

	// Missing refresh_token in new authorization must fail and keep previous credentials intact.
	_, _, err = auth.Authorize(context.Background(), AuthorizeOptions{
		ExpectedChannelID: "new-channel",
		CheckChannel:      func(context.Context, *oauth2.Token) (Channel, error) { return Channel{ID: "new-channel"}, nil },
	})
	if !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Fatalf("expected ErrRefreshTokenInvalid when refresh_token missing, got %v", err)
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "old-channel-test-access" || persisted.RefreshToken != "old-channel-test-refresh" {
		t.Fatalf("existing credentials were altered on missing-refresh auth: %+v", persisted)
	}

	// When new authorization includes a new refresh_token, it replaces old credentials completely.
	returnRefresh.Store(true)
	_, _, err = auth.Authorize(context.Background(), AuthorizeOptions{
		ExpectedChannelID: "new-channel",
		CheckChannel:      func(context.Context, *oauth2.Token) (Channel, error) { return Channel{ID: "new-channel"}, nil },
	})
	if err != nil {
		t.Fatalf("Authorize with new refresh token failed: %v", err)
	}
	updated, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessToken != "new-channel-test-access" || updated.RefreshToken != "new-channel-test-refresh" {
		t.Fatalf("new channel credentials not replaced cleanly: %+v", updated)
	}
}
