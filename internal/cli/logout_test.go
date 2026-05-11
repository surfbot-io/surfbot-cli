package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

func TestLogout_PurgesLocalEvenWhenServerMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// /cli/logout doesn't exist in PR2.api yet — 404 must NOT block local purge.
		http.NotFound(w, nil)
		_ = w
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	store := transport.NewTokenStoreAt(dir + "/surfbot")
	if err := store.Save("srfb_agt_test123"); err != nil {
		t.Fatalf("save token: %v", err)
	}
	if err := transport.SaveMetadata(dir+"/surfbot", &transport.AgentMetadata{
		AgentID: "ag_xxx", WSURL: "ws://127.0.0.1:0/cli/ws",
	}); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	flagLogoutAPIBase = srv.URL

	var out bytes.Buffer
	logoutCmd.SetOut(&out)
	logoutCmd.SetErr(&out)
	logoutCmd.SetContext(context.Background())

	if err := runLogout(logoutCmd, nil); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Local token purged") {
		t.Fatalf("missing local purge confirmation:\n%s", got)
	}

	// Token + metadata files must be gone.
	if _, err := os.Stat(filepath.Join(dir, "surfbot", transport.TokenFileName)); !os.IsNotExist(err) {
		t.Fatalf("token file still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "surfbot", transport.MetadataFileName)); !os.IsNotExist(err) {
		t.Fatalf("metadata file still present: %v", err)
	}
}

func TestLogout_NoTokenIsNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	flagLogoutAPIBase = "http://127.0.0.1:0"

	var out bytes.Buffer
	logoutCmd.SetOut(&out)
	logoutCmd.SetErr(&out)
	logoutCmd.SetContext(context.Background())

	if err := runLogout(logoutCmd, nil); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	if !strings.Contains(out.String(), "No agent enrolled") {
		t.Fatalf("expected 'No agent enrolled' message, got:\n%s", out.String())
	}
}

func TestLogout_HappyPathServerOK(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cli/logout" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "missing", http.StatusUnauthorized)
			return
		}
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	store := transport.NewTokenStoreAt(dir + "/surfbot")
	_ = store.Save("srfb_agt_test123")
	_ = transport.SaveMetadata(dir+"/surfbot", &transport.AgentMetadata{
		AgentID: "ag_xxx", WSURL: "ws://127.0.0.1:0/cli/ws",
	})

	flagLogoutAPIBase = srv.URL
	var out bytes.Buffer
	logoutCmd.SetOut(&out)
	logoutCmd.SetErr(&out)
	logoutCmd.SetContext(context.Background())

	if err := runLogout(logoutCmd, nil); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
	got := out.String()
	if !strings.Contains(got, "Server-side token revoked") {
		t.Fatalf("missing server-side revoke confirmation:\n%s", got)
	}
}
