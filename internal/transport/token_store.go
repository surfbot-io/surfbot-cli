// Package transport implements the cli-side primitives for talking to
// surfbot-api: persistent on-disk token storage, cert pinning, machine
// fingerprinting, the WebSocket client, and the host stats sampler used by
// heartbeats. SPEC-CLI1 §6, §10, §11 T1/T2/T5.
package transport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// TokenFileName is the basename of the token file on disk. Single-line file
// containing only the raw agent_token. SPEC-CLI1 §10.
const TokenFileName = "agent.token"

// MetadataFileName stores the non-secret bookkeeping the cli wrote at enroll
// time (agent_id, org_id, ws_url, prefix, hostname...). The token file is
// kept narrow (just the secret) so a `cat agent.token` is grep-safe; this
// sibling file holds everything else status needs.
const MetadataFileName = "agent.json"

// TokenStore is a tiny, OS-aware wrapper over the on-disk token file. The
// path is overridable in tests by constructing with NewTokenStoreAt.
type TokenStore struct {
	path string
}

// NewDefaultTokenStore returns a TokenStore at the platform-default config
// path: $XDG_CONFIG_HOME/surfbot/agent.token on Linux/Mac, %APPDATA%\surfbot
// \agent.token on Windows. The parent dir is NOT created here; Save creates
// it on demand with 0700.
func NewDefaultTokenStore() (*TokenStore, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return nil, err
	}
	return &TokenStore{path: filepath.Join(dir, TokenFileName)}, nil
}

// NewTokenStoreAt is for tests: pass an explicit directory.
func NewTokenStoreAt(dir string) *TokenStore {
	return &TokenStore{path: filepath.Join(dir, TokenFileName)}
}

// Path returns the absolute path the store reads from / writes to.
func (s *TokenStore) Path() string { return s.path }

// Save writes the token atomically with mode 0600. Parent dir is created
// with 0700 if missing. The atomic write is "write tmp + fsync + rename"
// so a crash mid-write never leaves a truncated token on disk.
func (s *TokenStore) Save(token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("token_store: refusing to save empty token")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("token_store: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".agent.token.*")
	if err != nil {
		return fmt.Errorf("token_store: tempfile: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("token_store: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("token_store: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("token_store: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("token_store: chmod: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return fmt.Errorf("token_store: rename: %w", err)
	}
	return nil
}

// Load returns the token, or "" with nil error if the file does not exist.
// Any other I/O error is returned as-is. The returned token is trimmed of
// surrounding whitespace.
func (s *TokenStore) Load() (string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("token_store: read: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Delete removes the token file. Missing file is not an error.
func (s *TokenStore) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("token_store: remove: %w", err)
	}
	return nil
}

// DefaultConfigDir returns the OS-appropriate per-user config directory for
// surfbot. On Linux/Mac this is $XDG_CONFIG_HOME/surfbot (with fallback to
// ~/.config/surfbot); on Windows it's %APPDATA%\surfbot.
func DefaultConfigDir() (string, error) {
	if runtime.GOOS == "windows" {
		if a := os.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, "surfbot"), nil
		}
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "surfbot"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config dir: %w", err)
	}
	return filepath.Join(home, ".config", "surfbot"), nil
}

// RedactToken returns the token's display form: prefix + 4 chars + ellipsis
// of bullets. The full token is never logged or printed. SPEC-CLI1 §11 T1.
func RedactToken(token string) string {
	if token == "" {
		return ""
	}
	// Tokens are "srfb_agt_<24chars>" — show prefix + 4 random chars + bullets.
	if i := strings.LastIndex(token, "_"); i > 0 && i < len(token)-4 {
		return token[:i+5] + "•••••••"
	}
	if len(token) > 6 {
		return token[:6] + "•••••••"
	}
	return "•••"
}
