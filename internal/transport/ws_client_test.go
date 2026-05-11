package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// helloHandler is a tiny in-process /cli/ws server that:
//   - accepts the surfbot.cli.v1 subprotocol
//   - reads the client.hello
//   - replies with the configured server.hello payload
//   - then either closes with the configured code or stays open until ctx done
type helloHandler struct {
	t                  *testing.T
	helloPayload       ServerHelloPayload
	sendServerShutdown *ServerShutdownPayload // when set, sent after server.hello before closing
	closeCode          websocket.StatusCode
	closeReason        string
	holdOpen           time.Duration
	clientHello        chan ClientHelloPayload
}

func (h *helloHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"surfbot.cli.v1"},
	})
	if err != nil {
		h.t.Logf("server accept: %v", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		_ = conn.Close(websocket.StatusInvalidFramePayloadData, "bad envelope")
		return
	}
	var hello ClientHelloPayload
	_ = json.Unmarshal(env.Payload, &hello)
	if h.clientHello != nil {
		select {
		case h.clientHello <- hello:
		default:
		}
	}

	resp, err := NewEnvelope(TypeServerHello, h.helloPayload)
	if err != nil {
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, resp); err != nil {
		return
	}
	if h.sendServerShutdown != nil {
		shutdown, err := NewEnvelope(TypeServerShutdown, *h.sendServerShutdown)
		if err == nil {
			_ = conn.Write(ctx, websocket.MessageText, shutdown)
		}
		// Wait briefly for the client's clean close, then exit.
		_, _, _ = conn.Read(ctx)
		return
	}
	if h.closeCode != 0 {
		_ = conn.Close(h.closeCode, h.closeReason)
		return
	}
	if h.holdOpen > 0 {
		select {
		case <-time.After(h.holdOpen):
		case <-ctx.Done():
		}
	}
}

func newHelloServer(t *testing.T, h *helloHandler) (url string, cleanup func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cli/ws"
	return wsURL, srv.Close
}

func TestWSClient_HandshakeRoundtrip(t *testing.T) {
	helloCh := make(chan ClientHelloPayload, 1)
	url, cleanup := newHelloServer(t, &helloHandler{
		t: t,
		helloPayload: ServerHelloPayload{
			SessionID:                "sess_test",
			ServerVersion:            "test",
			HeartbeatIntervalSeconds: 5,
			Capabilities:             []string{"heartbeat.v1"},
		},
		closeCode:   websocket.StatusNormalClosure,
		closeReason: "test done",
		clientHello: helloCh,
	})
	defer cleanup()

	var helloSeen atomic.Bool
	c := &WSClient{
		URL:         url,
		Token:       "srfb_agt_test",
		AgentID:     "ag_test",
		Version:     "0.0.0-test",
		BuildCommit: "abcdef",
		Hostname:    "testhost",
		Fingerprint: "sha256:deadbeef",
		OnServerHello: func(_ ServerHelloPayload) {
			helloSeen.Store(true)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Run() will return cleanly after the server closes with 1000 and ctx
	// later expires. We don't expect a fatal error here.
	done := make(chan error, 1)
	go func() { done <- c.runOnce(ctx) }()

	select {
	case got := <-helloCh:
		if got.AgentID != "ag_test" || got.Hostname != "testhost" || got.Fingerprint != "sha256:deadbeef" {
			t.Fatalf("server saw bad client.hello: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server never saw client.hello")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("runOnce did not return")
	}
	if !helloSeen.Load() {
		t.Fatalf("OnServerHello was not invoked")
	}
	if got, _ := c.LastServerHello(); got.SessionID != "sess_test" {
		t.Fatalf("LastServerHello mismatch: %+v", got)
	}
}

func TestWSClient_PurgesOnTokenRevoked(t *testing.T) {
	url, cleanup := newHelloServer(t, &helloHandler{
		t: t,
		helloPayload: ServerHelloPayload{
			SessionID:                "sess_test",
			ServerVersion:            "test",
			HeartbeatIntervalSeconds: 5,
		},
		closeCode:   websocket.StatusCode(4401),
		closeReason: "revoked",
	})
	defer cleanup()

	c := &WSClient{URL: url, Token: "x", AgentID: "ag", Hostname: "h", Fingerprint: "sha256:f"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("err = %v, want ErrTokenRevoked", err)
	}
}

func TestWSClient_PurgesOnTokenInvalid(t *testing.T) {
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

	c := &WSClient{URL: url, Token: "x", AgentID: "ag", Hostname: "h", Fingerprint: "sha256:f"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestWSClient_HandshakeReject_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing_token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cli/ws"

	c := &WSClient{URL: url, Token: "x", AgentID: "ag", Hostname: "h", Fingerprint: "sha256:f"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestBackoffFor_TableAndJitter(t *testing.T) {
	// Each step must remain within 1.25x its base.
	bases := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 60 * time.Second}
	for i, base := range bases {
		d := backoffFor(i, false)
		lo := time.Duration(float64(base) * 0.75)
		hi := time.Duration(float64(base) * 1.25)
		if d < lo || d > hi {
			t.Fatalf("attempt %d: d=%v out of [%v,%v]", i, d, lo, hi)
		}
	}
}

func TestBackoffFor_RateLimitedDoubles(t *testing.T) {
	// Rate limited at attempt 0 → table[1]*2 = 4s base, ±25%.
	d := backoffFor(0, true)
	if d < 3*time.Second || d > 5*time.Second {
		t.Fatalf("rate-limited backoff out of range: %v", d)
	}
}

func TestWSClient_ServerShutdown_TriggersReconnect(t *testing.T) {
	url, cleanup := newHelloServer(t, &helloHandler{
		t: t,
		helloPayload: ServerHelloPayload{
			SessionID:                "sess_test",
			HeartbeatIntervalSeconds: 5,
		},
		sendServerShutdown: &ServerShutdownPayload{
			Reason:                   "deploy",
			EstimatedDowntimeSeconds: 10,
		},
	})
	defer cleanup()

	c := &WSClient{URL: url, Token: "x", AgentID: "ag", Hostname: "h", Fingerprint: "sha256:f"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.runOnce(ctx)
	if !errors.Is(err, errServerShutdown) {
		t.Fatalf("err = %v, want errServerShutdown", err)
	}
}

func TestClampHeartbeat(t *testing.T) {
	noop := func(string, string, ...any) {}
	if got := clampHeartbeat(0, noop); got != defaultHeartbeatInt {
		t.Fatalf("clamp 0: %v", got)
	}
	if got := clampHeartbeat(1, noop); got != defaultHeartbeatInt {
		t.Fatalf("clamp 1: %v", got)
	}
	if got := clampHeartbeat(301, noop); got != defaultHeartbeatInt {
		t.Fatalf("clamp 301: %v", got)
	}
	if got := clampHeartbeat(60, noop); got != 60*time.Second {
		t.Fatalf("clamp 60: %v", got)
	}
}
