package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2"
)

const tokenFileName = "youtube-token.json"

// DefaultTokenPath returns the credentials path. It lives in the user config
// directory, which is outside the repository, so tokens can never be committed
// by accident.
func DefaultTokenPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("youtube: locate user config directory: %w", err)
	}
	return filepath.Join(dir, "it-news-shorts", tokenFileName), nil
}

// TokenStore keeps OAuth credentials in one file with mode 0600.
//
// It is deliberately a small interface-free struct so a production secret
// store can replace it later without touching the client or the CLI.
type TokenStore struct {
	path string
}

// NewTokenStore validates the path and refuses anything inside a git working
// tree, because a token inside the repository would eventually be committed.
func NewTokenStore(path string) (*TokenStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("youtube: token path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("youtube: resolve token path: %w", err)
	}
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("youtube: token path %s must not be a symlink", abs)
	}
	resolved, err := resolvePathThroughSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("youtube: resolve token symlinks: %w", err)
	}
	if root, ok := findGitRoot(filepath.Dir(resolved)); ok {
		return nil, fmt.Errorf("youtube: token path %s is inside the git work tree %s; keep credentials outside the repository", resolved, root)
	}
	return &TokenStore{path: resolved}, nil
}

// resolvePathThroughSymlinks resolves symlinks for the deepest existing
// ancestor of path and re-attaches any not-yet-created trailing segments.
func resolvePathThroughSymlinks(abs string) (string, error) {
	cur := abs
	var tail []string
	for {
		realPath, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				realPath = filepath.Join(realPath, tail[i])
			}
			return realPath, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// Path returns the token file path.
func (s *TokenStore) Path() string { return s.path }

// Load reads the stored token after verifying it is a regular file with 0600
// permissions (not a symlink or world/group-accessible file).
func (s *TokenStore) Load() (*oauth2.Token, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no token at %s", ErrCredentialsMissing, s.path)
		}
		return nil, fmt.Errorf("youtube: stat token: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("youtube: token file %s must not be a symlink", s.path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("youtube: token file %s must be a regular file", s.path)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return nil, fmt.Errorf("youtube: token file %s has insecure permissions %04o (want 0600)", s.path, perm)
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("youtube: read token: %w", err)
	}
	token := &oauth2.Token{}
	if err := json.Unmarshal(raw, token); err != nil {
		return nil, fmt.Errorf("youtube: parse token: %w", err)
	}
	if token.AccessToken == "" && token.RefreshToken == "" {
		return nil, fmt.Errorf("%w: token file %s is empty", ErrCredentialsMissing, s.path)
	}
	return token, nil
}

// Replace writes a newly authorized token atomically with mode 0600 without
// inheriting any previous refresh token, so switching accounts/channels never
// mixes credentials.
func (s *TokenStore) Replace(token *oauth2.Token) error {
	if token == nil {
		return fmt.Errorf("youtube: token is nil")
	}
	raw, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("youtube: encode token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("youtube: create token directory: %w", err)
	}
	return writeFileAtomic(s.path, raw, 0o600)
}

// Save writes the token atomically with mode 0600.
//
// A refreshed token often arrives without a refresh token; in that case the
// previously stored refresh token is kept, so refreshing never destroys
// offline access.
func (s *TokenStore) Save(token *oauth2.Token) error {
	if token == nil {
		return fmt.Errorf("youtube: token is nil")
	}
	toSave := *token
	if toSave.RefreshToken == "" {
		previous, err := s.Load()
		switch {
		case err == nil && previous.RefreshToken != "":
			toSave.RefreshToken = previous.RefreshToken
		case err != nil && !errors.Is(err, ErrCredentialsMissing):
			return fmt.Errorf("youtube: load existing token before save: %w", err)
		}
	}
	raw, err := json.MarshalIndent(&toSave, "", "  ")
	if err != nil {
		return fmt.Errorf("youtube: encode token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("youtube: create token directory: %w", err)
	}
	return writeFileAtomic(s.path, raw, 0o600)
}

// findGitRoot walks up from dir looking for a .git entry.
func findGitRoot(dir string) (string, bool) {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
