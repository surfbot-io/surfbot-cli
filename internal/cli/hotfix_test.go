package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

// HOTFIX C4: REST apiClient must reject http:// on non-loopback hosts.
func TestAPIClient_RejectsPlaintextOnNonLoopback(t *testing.T) {
	c := newAPIClient("http://example.com", "test", false)
	err := c.postJSON(context.Background(), "/cli/whoami", nil, nil)
	if !errors.Is(err, ErrInsecureAPIBase) {
		t.Fatalf("err = %v, want ErrInsecureAPIBase", err)
	}
}

func TestAPIClient_AcceptsHTTPSAndLoopback(t *testing.T) {
	for _, base := range []string{
		"https://api.surfbot.io",
		"http://127.0.0.1:1234",
		"http://localhost:8080",
	} {
		c := newAPIClient(base, "test", false)
		if err := c.requireSecure(); err != nil {
			t.Errorf("baseURL %q: requireSecure err = %v, want nil", base, err)
		}
	}
}

func TestAPIClient_SkipPinningUnlocksPlaintext(t *testing.T) {
	c := newAPIClient("http://staging-dev.example.com", "test", true)
	if err := c.requireSecure(); err != nil {
		t.Fatalf("skip-pinning override failed: %v", err)
	}
}

// HOTFIX H7: a server that responds with 4403 on the FIRST WS handshake
// after enrollment must purge the just-saved token and surface an error,
// not silently print "Authorized" and ship a dead token.
func TestVerifyConnection_PurgesAndErrorsOnTokenInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"surfbot.cli.v1"},
		})
		if err != nil {
			return
		}
		// Read the client.hello then close 4403 immediately.
		_, _, _ = conn.Read(r.Context())
		_ = conn.Close(websocket.StatusCode(4403), "bad token")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cli/ws"

	// Seed a token + metadata as if enroll had just persisted them.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	store := transport.NewTokenStoreAt(dir + "/surfbot")
	if err := store.Save("srfb_agt_seeded"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if err := transport.SaveMetadata(dir+"/surfbot", &transport.AgentMetadata{
		AgentID: "ag_test", WSURL: wsURL,
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}

	var out strings.Builder
	err := verifyConnection(context.Background(), &out, wsURL, "srfb_agt_seeded", "ag_test", "host", false)
	if err == nil {
		t.Fatalf("expected hard error from verifyConnection on 4403")
	}
	if !errors.Is(err, transport.ErrTokenInvalid) {
		t.Fatalf("err = %v, want wraps ErrTokenInvalid", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "surfbot", transport.TokenFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("token still on disk after dead-token verify: stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "surfbot", transport.MetadataFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("metadata still on disk after dead-token verify: stat err=%v", statErr)
	}
}

// HOTFIX H7 contrapositive: a transient network error during verification
// must NOT purge the token. The token is presumed-good; surface a soft
// warning so the user can retry via `surfbot-cli status`.
func TestVerifyConnection_NetworkErrorKeepsToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	store := transport.NewTokenStoreAt(dir + "/surfbot")
	if err := store.Save("srfb_agt_keep"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	// Unreachable URL → dial fails. verifyConnection caps at 15s; the
	// outer ctx here additionally bounds the whole call so the test wraps
	// quickly even if the WS client decides to retry inside the window.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var out strings.Builder
	err := verifyConnection(ctx, &out, "ws://127.0.0.1:1/cli/ws", "srfb_agt_keep", "ag_test", "host", false)
	if err != nil {
		t.Fatalf("transient network error should not surface as error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "surfbot", transport.TokenFileName)); statErr != nil {
		t.Fatalf("token file vanished after transient error: %v", statErr)
	}
}

// HOTFIX C4 + H7 cross-check: when the server's `/cli/device/token` returns
// a ws://attacker.example.com URL, the post-enroll verification must reject
// it as insecure and purge the seeded token. (We invoke verifyConnection
// directly with the malicious URL since runLogin's full path needs a longer
// fake server; the unit test catches the scheme-enforcement bug at the
// exact call site that mattered.)
func TestVerifyConnection_PurgesOnInsecureScheme(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	store := transport.NewTokenStoreAt(dir + "/surfbot")
	if err := store.Save("srfb_agt_seeded"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if err := transport.SaveMetadata(dir+"/surfbot", &transport.AgentMetadata{
		AgentID: "ag_test", WSURL: "ws://attacker.example.com/cli/ws",
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}

	var out strings.Builder
	err := verifyConnection(context.Background(), &out, "ws://attacker.example.com/cli/ws", "srfb_agt_seeded", "ag_test", "host", false)
	if err == nil {
		t.Fatalf("expected hard error from verifyConnection on insecure scheme")
	}
	if !errors.Is(err, transport.ErrInsecureScheme) {
		t.Fatalf("err = %v, want wraps ErrInsecureScheme", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "surfbot", transport.TokenFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("token still on disk after insecure-scheme reject: stat err=%v", statErr)
	}
}

// Ensure encoding/json import stays used in case the file is trimmed.
var _ = json.Marshal
