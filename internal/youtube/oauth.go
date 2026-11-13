package youtube

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// ScopeForceSSL allows full management of the authorized YouTube account,
// including uploading videos, reading/updating metadata and schedules, and
// deleting user YouTube data. It is required because YouTube Data API v3 does
// not provide a narrower scope that covers both upload and videos.update.
const ScopeForceSSL = "https://www.googleapis.com/auth/youtube.force-ssl"

// Google endpoints. They are declared here instead of importing
// golang.org/x/oauth2/google so the integration pulls no extra module.
var googleEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// DefaultCallbackTimeout bounds the wait for the browser redirect.
const DefaultCallbackTimeout = 5 * time.Minute

// OAuthConfig holds the Desktop OAuth client.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
	Endpoint     oauth2.Endpoint
}

// NewOAuthConfig validates the client credentials.
func NewOAuthConfig(clientID, clientSecret string) (OAuthConfig, error) {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return OAuthConfig{}, fmt.Errorf("%w: set YOUTUBE_CLIENT_ID and YOUTUBE_CLIENT_SECRET", ErrCredentialsMissing)
	}
	return OAuthConfig{
		ClientID:     strings.TrimSpace(clientID),
		ClientSecret: strings.TrimSpace(clientSecret),
		Scopes:       []string{ScopeForceSSL},
		Endpoint:     googleEndpoint,
	}, nil
}

// ChannelChecker reads the channel that owns a token.
type ChannelChecker func(ctx context.Context, token *oauth2.Token) (Channel, error)

// AuthorizeOptions tunes the interactive flow.
type AuthorizeOptions struct {
	// ExpectedChannelID, when set, must match the authorized channel.
	ExpectedChannelID string
	// CheckChannel verifies the token against the API. Optional.
	CheckChannel ChannelChecker
}

// Authorizer runs the OAuth Authorization Code flow with PKCE and a loopback
// redirect, then persists the token.
type Authorizer struct {
	Config OAuthConfig
	Store  *TokenStore
	// OpenBrowser opens the consent URL; nil means the platform default.
	OpenBrowser func(url string) error
	// CallbackTimeout bounds the browser round trip.
	CallbackTimeout time.Duration
	// Out receives human-facing progress lines. Secrets are never written here.
	Out io.Writer
	// HTTPClient performs the token exchange; nil means http.DefaultClient.
	HTTPClient *http.Client
	// Now is the clock used for token expiry bookkeeping.
	Now func() time.Time
}

func (a *Authorizer) out() io.Writer {
	if a.Out != nil {
		return a.Out
	}
	return io.Discard
}

func (a *Authorizer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authorizer) oauthConfig(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.Config.ClientID,
		ClientSecret: a.Config.ClientSecret,
		Scopes:       a.Config.Scopes,
		Endpoint:     a.Config.Endpoint,
		RedirectURL:  redirectURL,
	}
}

// requestContext injects the transport used for token calls. Without this the
// oauth2 package would always use http.DefaultClient (which has no timeout),
// and tests could not reach a mock token endpoint.
func (a *Authorizer) requestContext(ctx context.Context) context.Context {
	client := a.HTTPClient
	if client == nil {
		client = DefaultHTTPClient()
	}
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}

// Authorize performs the interactive flow and returns the authorized channel.
func (a *Authorizer) Authorize(ctx context.Context, opts AuthorizeOptions) (Channel, *oauth2.Token, error) {
	if a.Store == nil {
		return Channel{}, nil, fmt.Errorf("youtube: token store is not configured")
	}
	if strings.TrimSpace(a.Config.ClientID) == "" || strings.TrimSpace(a.Config.ClientSecret) == "" {
		return Channel{}, nil, ErrCredentialsMissing
	}
	if strings.TrimSpace(opts.ExpectedChannelID) != "" && opts.CheckChannel == nil {
		return Channel{}, nil, fmt.Errorf("youtube: channel checker is required when expected channel id is set")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Channel{}, nil, fmt.Errorf("youtube: start loopback listener: %w", err)
	}
	defer listener.Close()

	redirectURL := "http://" + listener.Addr().String() + "/callback"
	cfg := a.oauthConfig(redirectURL)

	verifier := oauth2.GenerateVerifier()
	state, err := randomState()
	if err != nil {
		return Channel{}, nil, err
	}

	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"),
	)

	outcomes := make(chan callbackOutcome, 1)
	server := &http.Server{
		Handler:           a.callbackHandler(state, outcomes),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	defer server.Close()

	fmt.Fprintf(a.out(), "Open this URL in a browser to authorize:\n\n  %s\n\n", authURL)
	opener := a.OpenBrowser
	if opener == nil {
		opener = openBrowser
	}
	if err := opener(authURL); err != nil {
		fmt.Fprintf(a.out(), "Could not open a browser automatically (%v). Open the URL above manually.\n", err)
	}

	timeout := a.CallbackTimeout
	if timeout <= 0 {
		timeout = DefaultCallbackTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var outcome callbackOutcome
	select {
	case outcome = <-outcomes:
	case err := <-serveErr:
		return Channel{}, nil, fmt.Errorf("youtube: loopback server failed: %w", err)
	case <-timer.C:
		return Channel{}, nil, fmt.Errorf("%w: no redirect received within %s", ErrAuthorizationDenied, timeout)
	case <-ctx.Done():
		return Channel{}, nil, ctx.Err()
	}
	if outcome.err != nil {
		return Channel{}, nil, outcome.err
	}

	exchangeCtx, cancelExchange := withDefaultTimeout(a.requestContext(ctx), 60*time.Second)
	defer cancelExchange()
	token, err := cfg.Exchange(exchangeCtx, outcome.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Channel{}, nil, fmt.Errorf("youtube: exchange authorization code: %w", mapTokenError(err))
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		return Channel{}, nil, fmt.Errorf("%w: authorization response did not include a refresh token; revoke app access and re-authorize with offline consent", ErrRefreshTokenInvalid)
	}

	channel := Channel{}
	if opts.CheckChannel != nil {
		checkCtx, cancelCheck := withDefaultTimeout(ctx, 60*time.Second)
		defer cancelCheck()
		channel, err = opts.CheckChannel(checkCtx, token)
		if err != nil {
			return Channel{}, nil, fmt.Errorf("youtube: read authorized channel: %w", err)
		}
		if opts.ExpectedChannelID != "" && channel.ID != opts.ExpectedChannelID {
			return channel, nil, fmt.Errorf("%w: got %s, want %s", ErrChannelMismatch, channel.ID, opts.ExpectedChannelID)
		}
	}

	if err := a.Store.Replace(token); err != nil {
		return Channel{}, nil, err
	}
	fmt.Fprintf(a.out(), "Token stored at %s (mode 0600).\n", a.Store.Path())
	return channel, token, nil
}

type callbackOutcome struct {
	code string
	err  error
}

// callbackHandler answers the loopback redirect. It rejects provider errors,
// a mismatched state and a missing code before anything is exchanged.
func (a *Authorizer) callbackHandler(wantState string, outcomes chan<- callbackOutcome) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if providerError := query.Get("error"); providerError != "" {
			message := providerError
			if description := query.Get("error_description"); description != "" {
				message += ": " + description
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "Authorization failed: %s", message)
			sendOutcome(outcomes, callbackOutcome{err: fmt.Errorf("%w: %s", ErrAuthorizationDenied, message)})
			return
		}
		if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(wantState)) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "Authorization failed: state mismatch")
			sendOutcome(outcomes, callbackOutcome{err: ErrStateMismatch})
			return
		}
		code := query.Get("code")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "Authorization failed: missing authorization code")
			sendOutcome(outcomes, callbackOutcome{err: fmt.Errorf("%w: missing authorization code", ErrAuthorizationDenied)})
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Authorization complete. You can close this window.")
		sendOutcome(outcomes, callbackOutcome{code: code})
	}
}

func sendOutcome(outcomes chan<- callbackOutcome, outcome callbackOutcome) {
	select {
	case outcomes <- outcome:
	default:
	}
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// TokenSource loads the stored token and returns a source that refreshes it
// automatically and persists every rotation.
func (a *Authorizer) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if a.Store == nil {
		return nil, fmt.Errorf("youtube: token store is not configured")
	}
	if strings.TrimSpace(a.Config.ClientID) == "" ||
		strings.TrimSpace(a.Config.ClientSecret) == "" ||
		strings.TrimSpace(a.Config.Endpoint.TokenURL) == "" {
		return nil, fmt.Errorf("%w: oauth client config is incomplete", ErrCredentialsMissing)
	}
	token, err := a.Store.Load()
	if err != nil {
		return nil, err
	}
	cfg := a.oauthConfig("")
	base := cfg.TokenSource(a.requestContext(ctx), token)
	return &persistingTokenSource{source: base, store: a.Store, last: token, now: a.Now}, nil
}

// persistingTokenSource writes refreshed tokens back to the store.
type persistingTokenSource struct {
	mu     sync.Mutex
	source oauth2.TokenSource
	store  *TokenStore
	last   *oauth2.Token
	now    func() time.Time
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	token, err := p.source.Token()
	if err != nil {
		return nil, mapTokenError(err)
	}
	changed := p.last == nil ||
		token.AccessToken != p.last.AccessToken ||
		token.RefreshToken != p.last.RefreshToken ||
		!token.Expiry.Equal(p.last.Expiry)
	if changed {
		if err := p.store.Save(token); err != nil {
			return nil, err
		}
		p.last = token
	}
	return token, nil
}

// mapTokenError turns Google token errors into integration errors without
// exposing raw token response bodies or sensitive URL query parameters.
func mapTokenError(err error) error {
	if err == nil {
		return nil
	}
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		code := retrieve.ErrorCode
		if code == "" {
			if parsed, ok := parseOAuthError(retrieve.Body); ok {
				code = parsed
			}
		}
		switch code {
		case "invalid_grant":
			return fmt.Errorf("%w (%s)", ErrRefreshTokenInvalid, code)
		case "insufficient_scope":
			return fmt.Errorf("%w (%s)", ErrInsufficientScope, code)
		default:
			if code != "" {
				return fmt.Errorf("youtube: oauth token error (%s)", code)
			}
			if retrieve.Response != nil {
				return fmt.Errorf("youtube: oauth token request failed with status %d", retrieve.Response.StatusCode)
			}
			return errors.New("youtube: oauth token request failed")
		}
	}
	return sanitizeTransportError(err)
}

func parseOAuthError(body []byte) (string, bool) {
	const marker = `"error"`
	text := string(body)
	index := strings.Index(text, marker)
	if index < 0 {
		return "", false
	}
	rest := text[index+len(marker):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return "", false
	}
	rest = strings.TrimSpace(rest[colon+1:])
	rest = strings.TrimPrefix(rest, `"`)
	end := strings.Index(rest, `"`)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

func randomState() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("youtube: generate oauth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// openBrowser opens url with the platform handler.
func openBrowser(rawURL string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{rawURL}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		command, args = "xdg-open", []string{rawURL}
	}
	if _, err := exec.LookPath(command); err != nil {
		return err
	}
	process := exec.Command(command, args...)
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	if err := process.Start(); err != nil {
		return err
	}
	go func() { _ = process.Wait() }()
	return nil
}

// ParseRedirectURL is a helper for tests and docs.
func ParseRedirectURL(raw string) (*url.URL, error) { return url.Parse(raw) }

// DefaultHTTPClient is used when no client is injected.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

// EnvClientCredentials reads the OAuth client from the environment.
func EnvClientCredentials(getenv func(string) string) (string, string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv("YOUTUBE_CLIENT_ID"), getenv("YOUTUBE_CLIENT_SECRET")
}
