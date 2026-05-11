package transport

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTokenStore_SaveLoadDelete_RespectsPerms(t *testing.T) {
	dir := t.TempDir()
	s := NewTokenStoreAt(dir)

	if got, err := s.Load(); err != nil || got != "" {
		t.Fatalf("empty load: got %q, err=%v", got, err)
	}

	tok := "srfb_agt_8a7b3c2d1e5f6789abcdef01"
	if err := s.Save(tok); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != tok {
		t.Fatalf("load mismatch: got %q want %q", got, tok)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, TokenFileName))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file perm: got %o want 0600", perm)
		}
	}

	if err := s.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, err := s.Load(); err != nil || got != "" {
		t.Fatalf("post-delete load: got %q, err=%v", got, err)
	}
	// Delete is idempotent.
	if err := s.Delete(); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestTokenStore_Save_RejectsEmpty(t *testing.T) {
	s := NewTokenStoreAt(t.TempDir())
	for _, tok := range []string{"", "   ", "\n"} {
		if err := s.Save(tok); err == nil {
			t.Fatalf("save %q: want error, got nil", tok)
		}
	}
}

func TestTokenStore_Save_AtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	s := NewTokenStoreAt(dir)
	if err := s.Save("first"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := s.Save("second"); err != nil {
		t.Fatalf("second save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "second" {
		t.Fatalf("got %q want second", got)
	}
	// No tempfiles left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".agent.token.") {
			t.Fatalf("leaked tempfile: %s", e.Name())
		}
	}
}

func TestRedactToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc", "•••"},
		{"srfb_agt_abcdef01234567890123", "srfb_agt_abcd•••••••"},
		{"srfb_enr_xyz12345", "srfb_enr_xyz1•••••••"},
		{"loosetoken", "looset•••••••"},
	}
	for _, tc := range cases {
		if got := RedactToken(tc.in); got != tc.want {
			t.Fatalf("RedactToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
