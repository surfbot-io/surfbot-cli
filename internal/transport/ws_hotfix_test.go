package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// HOTFIX C1: token must be purged on close 4401/4403 by the WSClient itself,
// not by the (eventually-forgetful) caller.
func TestWSClient_PurgesTokenStoreOnRevoke(t *testing.T) {
	url, cleanup := newHelloServer(t, &helloHandler{
		t: t,
		helloPayload: ServerHelloPayload{
			SessionID:                "sess_purgetest",
			HeartbeatIntervalSeconds: 5,
		},
		closeCode:   websocket.StatusCode(4401),
		closeReason: "revoked",
	})
	defer cleanup()

	dir := t.TempDir()
	store := NewTokenStoreAt(dir)
	if err := store.Save("srfb_agt_purgetest"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	c := &WSClient{
		URL: url, Token: "x", AgentID: "ag", Hostname: "h",
		Fingerprint: "sha256:f", TokenStore: store,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("err = %v, want ErrTokenRevoked", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, TokenFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("token file still on disk after revoke: stat err=%v", statErr)
	}
}

func TestWSClient_PurgesTokenStoreOnInvalid(t *testing.T) {
	url, cleanup := newHelloServer(t, &helloHandler{
		t: t,
		helloPayload: ServerHelloPayload{
			SessionID:                "s",
			HeartbeatIntervalSeconds: 5,
		},
		closeCode:   websocket.StatusCode(4403),
		closeReason: "bad token",
	})
	defer cleanup()

	dir := t.TempDir()
	store := NewTokenStoreAt(dir)
	if err := store.Save("srfb_agt_invalidtest"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	c := &WSClient{
		URL: url, Token: "x", AgentID: "ag", Hostname: "h",
		Fingerprint: "sha256:f", TokenStore: store,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, TokenFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("token file still on disk after invalid: stat err=%v", statErr)
	}
}

// HOTFIX C1 contrapositive: a non-revoke close (network error) must NOT
// touch the token file. Otherwise a brief flap deletes the operator's auth.
func TestWSClient_NetworkErrorDoesNotPurgeToken(t *testing.T) {
	// Closed-server URL → dial fails. We exit Run by ctx cancel before
	// reconnect storm exhausts attempts.
	dir := t.TempDir()
	store := NewTokenStoreAt(dir)
	if err := store.Save("srfb_agt_keepme"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	c := &WSClient{
		URL:         "ws://127.0.0.1:1/cli/ws", // closed port, refuses immediately
		Token:       "x",
		AgentID:     "ag",
		Hostname:    "h",
		Fingerprint: "sha256:f",
		TokenStore:  store,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)
	if _, statErr := os.Stat(filepath.Join(dir, TokenFileName)); statErr != nil {
		t.Fatalf("token file vanished after network error: %v", statErr)
	}
}

// HOTFIX C2: close 1001 / server.shutdown must trigger fast-reconnect ≤50ms,
// not the regular 1s+ backoff. We hijack fastReconnectDelay and the backoff
// table to make the assertion tight without slowing the suite.
func TestWSClient_ServerShutdown_TriggersFastReconnect(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"surfbot.cli.v1"},
		})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, err = conn.Read(r.Context())
		if err != nil {
			return
		}
		// Reply with valid server.hello then close 1001.
		hello, _ := NewEnvelope(TypeServerHello, ServerHelloPayload{
			SessionID:                "s",
			HeartbeatIntervalSeconds: 5,
		})
		_ = conn.Write(r.Context(), websocket.MessageText, hello)
		_ = conn.Close(websocket.StatusGoingAway, "deploy")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cli/ws"

	c := &WSClient{URL: wsURL, Token: "x", AgentID: "ag", Hostname: "h", Fingerprint: "sha256:f"}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel right after we've seen at least 2 reconnects; the test
		// asserts they happened within the fast-reconnect window.
		for dials.Load() < 2 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	_ = c.Run(ctx)
	elapsed := time.Since(start)
	if dials.Load() < 2 {
		t.Fatalf("only %d dial(s) within fast-reconnect window", dials.Load())
	}
	// 2 connects with 50ms fast-reconnect (+ ≤handshakeTimeout each) must
	// land well under 1s; the regular backoff would have used ~1s for the
	// first re-attempt alone.
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("fast-reconnect took %v, want <1.5s", elapsed)
	}
}

// HOTFIX C3: the attempt counter must reset once the connection stayed up
// past stableConnectionWindow. We hijack the window to a tiny value, push
// 3 disconnects fast (to bump attempt), then have dial #4 stay open past
// the window before closing abnormally. The OnReconnect after dial #4 must
// fire with a stage-0 backoff (~1s ±25%), not stage-3+ (8s+) that would
// indicate the counter never reset. We cancel ctx inside OnReconnect once
// we have exactly the 4 observations we want, so dial #5 never happens and
// the captured delay is unambiguously iter-4's.
func TestWSClient_BackoffResetsAfterStableConnection(t *testing.T) {
	prevWindow := stableConnectionWindow
	stableConnectionWindow = 50 * time.Millisecond
	t.Cleanup(func() { stableConnectionWindow = prevWindow })

	prevFast := fastReconnectDelay
	fastReconnectDelay = 5 * time.Millisecond
	t.Cleanup(func() { fastReconnectDelay = prevFast })

	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := dials.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"surfbot.cli.v1"},
		})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, err = conn.Read(r.Context())
		if err != nil {
			return
		}
		hello, _ := NewEnvelope(TypeServerHello, ServerHelloPayload{HeartbeatIntervalSeconds: 5})
		_ = conn.Write(r.Context(), websocket.MessageText, hello)
		// First 3 dials close with 1001 (fast-reconnect); the 4th holds the
		// conn open long enough to exceed stableConnectionWindow.
		if n < 4 {
			_ = conn.Close(websocket.StatusGoingAway, "flap")
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(150 * time.Millisecond):
		}
		_ = conn.Close(websocket.StatusAbnormalClosure, "tear down stable conn")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cli/ws"

	c := &WSClient{URL: wsURL, Token: "x", AgentID: "ag", Hostname: "h", Fingerprint: "sha256:f"}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	var observedDelays []time.Duration
	c.OnReconnect = func(_ int, d time.Duration) {
		observedDelays = append(observedDelays, d)
		// Cancel as soon as we've observed iter-4's delay so iter-5 never
		// dials (which would have used a stage-1 backoff and shadowed the
		// post-stable measurement we care about).
		if len(observedDelays) == 4 {
			cancel()
		}
	}

	_ = c.Run(ctx)

	if len(observedDelays) < 4 {
		t.Fatalf("expected >=4 reconnect observations, got %d", len(observedDelays))
	}
	// observedDelays[0..2] = fast-reconnect path (≤fastReconnectDelay × jitter)
	// observedDelays[3] = post-stable abnormal_closure → should be stage-0
	// backoff (~1s ±25%), proving the counter reset. Without the reset, the
	// attempt counter would be at index 3+ → 8s+ delay.
	postStable := observedDelays[3]
	if postStable > 2*time.Second {
		t.Fatalf("post-stable reconnect delay %v indicates the backoff counter did not reset (expected ≤2s)", postStable)
	}
}

// HOTFIX C4: a plaintext scheme on a non-loopback host must be rejected by
// runOnce *before* the http.Client sends the token. We feed a ws:// URL
// pointing at a non-loopback host and assert ErrInsecureScheme.
func TestWSClient_RejectsPlaintextOnNonLoopback(t *testing.T) {
	c := &WSClient{
		URL:         "ws://example.com/cli/ws",
		Token:       "srfb_agt_secret",
		AgentID:     "ag",
		Hostname:    "h",
		Fingerprint: "sha256:f",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := c.runOnce(ctx)
	if !errors.Is(err, ErrInsecureScheme) {
		t.Fatalf("err = %v, want ErrInsecureScheme", err)
	}
}

// C4 inverse: loopback hosts are allowed for tests/dev.
func TestRequireSecureScheme_LoopbackAllowed(t *testing.T) {
	for _, raw := range []string{
		"ws://127.0.0.1:8080/cli/ws",
		"ws://localhost/cli/ws",
		"ws://[::1]/cli/ws",
		"http://localhost/healthz",
	} {
		if err := requireSecureScheme(raw, false); err != nil {
			t.Errorf("requireSecureScheme(%q) = %v, want nil", raw, err)
		}
	}
}

// C4: skip-pinning flag unlocks plaintext on remote hosts (dev clusters).
func TestRequireSecureScheme_SkipPinningOverride(t *testing.T) {
	err := requireSecureScheme("ws://staging-dev.example.com/cli/ws", true)
	if err != nil {
		t.Fatalf("skip-pinning override failed: %v", err)
	}
}

// C4: wss/https always pass without skip-pinning.
func TestRequireSecureScheme_TLSAlwaysOK(t *testing.T) {
	for _, raw := range []string{
		"wss://api.surfbot.io/cli/ws",
		"https://api-staging.surfbot.io",
	} {
		if err := requireSecureScheme(raw, false); err != nil {
			t.Errorf("requireSecureScheme(%q) = %v, want nil", raw, err)
		}
	}
}

// HOTFIX H6: native ping detects a hung server faster than the envelope
// heartbeat watchdog. coder/websocket auto-responds to pings server-side so
// we can't directly observe outbound pings without a handcrafted server.
// Instead we exercise the wire: hold the conn open >1 ping interval and
// assert runOnce stays alive (i.e. ping/pong is keeping the watchdog quiet
// rather than the read loop hanging). The companion runHeartbeats unit test
// already asserts the heartbeat goroutine; this one asserts runPings doesn't
// kill an otherwise-healthy connection.
func TestWSClient_PingDoesNotKillHealthyConn(t *testing.T) {
	url, cleanup := newHelloServer(t, &helloHandler{
		t: t,
		helloPayload: ServerHelloPayload{
			HeartbeatIntervalSeconds: 5, // ping every 2.5s
		},
		holdOpen: 4 * time.Second, // 4s > 1 ping interval; pong must work
	})
	defer cleanup()

	c := &WSClient{URL: url, Token: "x", AgentID: "ag", Hostname: "h", Fingerprint: "sha256:f"}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	start := time.Now()
	err := c.runOnce(ctx)
	elapsed := time.Since(start)
	// Server holds the conn open for ~4s then closes abnormally; we expect
	// runOnce to return around that mark, NOT earlier (which would mean ping
	// failed) and NOT later than ctx timeout.
	if elapsed < 3500*time.Millisecond {
		t.Fatalf("runOnce returned in %v (<3.5s); ping likely killed a healthy conn. err=%v", elapsed, err)
	}
	if elapsed > 5500*time.Millisecond {
		t.Fatalf("runOnce did not return within hold window (%v); ping/heartbeat watchdog likely not wired", elapsed)
	}
}
