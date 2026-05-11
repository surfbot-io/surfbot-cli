package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Cli-side mirrors of the API close codes (SPEC §6.3.6). The WS client
// surfaces these to the caller so cobra commands can map them to exit codes
// and token-purge actions.
const (
	closeCodeTokenRevoked = 4401
	closeCodeTokenInvalid = 4403
	closeCodeRateLimited  = 4429
	closeCodeOrgSuspended = 4503
	defaultHeartbeatInt   = 30 * time.Second
	heartbeatIntMin       = 5 * time.Second
	heartbeatIntMax       = 300 * time.Second
	handshakeTimeout      = 10 * time.Second
	writeTimeout          = 10 * time.Second
	backoffJitter         = 0.25
)

// stableConnectionWindow is the duration a single conn must hold open
// before the next disconnect resets the backoff attempt counter. SPEC §6.4
// suggests 5 min; declared as a var (not const) so tests can shrink it.
var stableConnectionWindow = 5 * time.Minute

// requireSecureScheme rejects plaintext ws://, http:// schemes on
// non-loopback hosts. The skipPinning escape hatch (the existing
// --insecure-skip-pinning flag, already a "knowingly insecure" gesture) is
// accepted as the operator-explicit override for dev clusters that haven't
// yet provisioned TLS.
//
// Loopback hosts (127.0.0.0/8, ::1, localhost) are always allowed: tests
// and `surfbot-cli` running side-by-side with a dev `surfbot-api` over
// http never need pinning, and forcing wss in unit tests would gratuitously
// require generating a CA chain per test.
func requireSecureScheme(rawURL string, skipPinning bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	switch u.Scheme {
	case "wss", "https":
		return nil
	case "ws", "http":
		// fall through to loopback / skip-pinning gating
	default:
		return fmt.Errorf("%w: unsupported scheme %q in %s", ErrInsecureScheme, u.Scheme, rawURL)
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	if skipPinning {
		return nil
	}
	return fmt.Errorf("%w: scheme %q on host %q (use --insecure-skip-pinning to override on dev clusters)", ErrInsecureScheme, u.Scheme, u.Hostname())
}

// isLoopbackHost recognizes localhost, 127.0.0.0/8, ::1. Used to whitelist
// the test/dev path through the plaintext-scheme gate without weakening the
// production check.
func isLoopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// Sentinel errors the caller distinguishes on.
//
//	ErrTokenRevoked — purge token file + exit 0.
//	ErrTokenInvalid — purge token file + log warning + exit 1.
//	ErrOrgSuspended — exit 1 (token kept; org may un-suspend).
//	ErrRateLimited  — internal; the run loop self-handles backoff doubling.
var (
	ErrTokenRevoked = errors.New("ws: token revoked (close 4401)")
	ErrTokenInvalid = errors.New("ws: token invalid (close 4403)")
	ErrOrgSuspended = errors.New("ws: org suspended (close 4503)")
	ErrRateLimited  = errors.New("ws: rate limited (close 4429)")
)

// WSClient maintains a single persistent connection to /cli/ws with
// reconnect + heartbeat + handshake. One-shot: build via NewWSClient, call
// Run, the call blocks until a fatal close or ctx cancellation.
type WSClient struct {
	URL         string
	Token       string
	AgentID     string
	Version     string
	BuildCommit string
	Hostname    string
	Fingerprint string

	// TokenStore, when non-nil, is purged automatically when the server
	// closes the connection with code 4401 (token revoked) or 4403 (token
	// invalid). SPEC §6.3.6 mandates token deletion in both cases — the
	// hotfix for CRITICAL #1 wires this through to the WSClient so daemon
	// and one-shot commands share the same purge path. Callers that don't
	// want auto-purge (tests, dry-run probes) leave this nil.
	TokenStore *TokenStore

	// Hooks the caller fills in. Optional.
	OnServerHello func(ServerHelloPayload)
	OnConnect     func()
	OnDisconnect  func(error)
	OnReconnect   func(attempt int, delay time.Duration)
	Logger        func(level, msg string, kv ...any)

	// Pinning toggle. When true, the cert pinning is bypassed (the
	// --insecure-skip-pinning flag). The same flag also unlocks insecure
	// transport schemes (ws://, http://) on non-loopback hosts — the CRITICAL
	// #4 hotfix rejects insecure schemes by default to prevent MITM token
	// leak, but operators with a dev cluster need an explicit override and
	// this flag is already the documented "I know what I'm doing" gesture.
	// Logged loudly on Run() entry.
	SkipPinning bool

	// Internal heartbeat handle exposed so `status` can capture the last
	// observed values without a full reconnect loop.
	lastHello mutexValue[ServerHelloPayload]
	lastBeat  mutexValue[time.Time]
}

// mutexValue is a tiny generic wrapper for thread-safe single-value access
// without pulling in sync/atomic.Value's any-typed API at call sites.
type mutexValue[T any] struct {
	mu sync.Mutex
	v  T
	ok bool
}

func (a *mutexValue[T]) store(v T) { a.mu.Lock(); a.v = v; a.ok = true; a.mu.Unlock() }
func (a *mutexValue[T]) load() (T, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v, a.ok
}

// LastServerHello returns the most recently received server.hello payload.
func (c *WSClient) LastServerHello() (ServerHelloPayload, bool) { return c.lastHello.load() }

// LastHeartbeat returns the most recent heartbeat send timestamp.
func (c *WSClient) LastHeartbeat() (time.Time, bool) { return c.lastBeat.load() }

// log emits a structured log line via the optional hook. Falls back to a
// no-op when Logger is nil so unit tests don't print noise.
func (c *WSClient) log(level, msg string, kv ...any) {
	if c.Logger == nil {
		return
	}
	c.Logger(level, msg, kv...)
}

// fastReconnectDelay is the post-1001 / post-server.shutdown delay per
// SPEC §6.3.5: the server is announcing a planned restart, so we skip the
// exponential ladder and try again almost immediately. Holding it as a
// package var (not const) lets tests dial it down without round-tripping
// through the public API.
var fastReconnectDelay = 50 * time.Millisecond

// Run drives the lifecycle: dial → handshake → loop → reconnect with
// backoff. Returns nil on caller-initiated ctx cancellation, or one of the
// sentinel errors on a non-recoverable close. Recoverable errors (network
// flap, 1001 going_away, 1006 abnormal_closure, etc.) are absorbed and the
// loop reconnects.
//
// CRITICAL hotfixes wired in here (PR #5 review):
//   - C1: on ErrTokenRevoked / ErrTokenInvalid, purge TokenStore before return.
//   - C2: on close 1001 / errServerShutdown, use 50ms fast reconnect.
//   - C3: reset backoff attempt counter when the connection stayed up >5min;
//     the reset condition is gated on connectedAt (set inside runOnce after
//     server.hello), not on a post-sleep timestamp like the original.
func (c *WSClient) Run(ctx context.Context) error {
	if c.SkipPinning {
		c.log("warn", "cert pinning bypassed via --insecure-skip-pinning")
	}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		// connectedAt is filled in by runOnce once server.hello is received,
		// so reading it here after the call tells us how long this attempt
		// actually held a live conn. A long-stable connection (>5min) wipes
		// the attempt counter so a later flap restarts at delay=1s, not 60s.
		var connectedAt time.Time
		err := c.runOnceTracked(ctx, &connectedAt)
		if err == nil {
			// ctx cancellation propagated as nil — return cleanly.
			return nil
		}

		if !connectedAt.IsZero() && time.Since(connectedAt) > stableConnectionWindow {
			attempt = 0
		}

		if errors.Is(err, ErrTokenRevoked) || errors.Is(err, ErrTokenInvalid) {
			c.purgeToken(err)
			return err
		}
		if errors.Is(err, ErrOrgSuspended) {
			return err
		}
		if errors.Is(err, ErrInsecureScheme) {
			// No retry can make a plaintext URL on a remote host safe; bubble
			// up so the caller surfaces it to the operator (and, post-enroll,
			// can purge the seeded token).
			return err
		}

		// Backoff selection. server.shutdown / 1001 → fast-reconnect 50ms.
		var delay time.Duration
		switch {
		case errors.Is(err, errServerShutdown), websocket.CloseStatus(err) == websocket.StatusGoingAway:
			delay = fastReconnectDelay
		case errors.Is(err, ErrRateLimited):
			delay = backoffFor(attempt, true)
		default:
			delay = backoffFor(attempt, false)
		}
		attempt++
		c.log("info", "ws reconnect scheduled", "attempt", attempt, "delay", delay.String(), "err", err.Error())
		if c.OnReconnect != nil {
			c.OnReconnect(attempt, delay)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// purgeToken deletes the on-disk token when the server explicitly invalidated
// it. Best-effort: a Delete failure is logged but does not block the sentinel
// from bubbling up — the operator still needs to know the token is dead.
func (c *WSClient) purgeToken(reason error) {
	if c.TokenStore == nil {
		return
	}
	if err := c.TokenStore.Delete(); err != nil {
		c.log("warn", "token purge failed", "reason", reason.Error(), "err", err.Error())
		return
	}
	c.log("info", "token purged after server revoke/invalid", "reason", reason.Error())
}

// runOnceTracked is runOnce + an out-parameter that captures the moment the
// connection became live (server.hello received). Keeps Run's reset logic
// clean without exporting the timestamp.
func (c *WSClient) runOnceTracked(ctx context.Context, connectedAt *time.Time) error {
	prev := c.OnConnect
	c.OnConnect = func() {
		*connectedAt = time.Now()
		if prev != nil {
			prev()
		}
	}
	defer func() { c.OnConnect = prev }()
	return c.runOnce(ctx)
}

// ErrInsecureScheme is returned by runOnce when the configured ws_url uses
// a plaintext scheme on a non-loopback host without --insecure-skip-pinning.
// SPEC §6.1 requires `wss`; rejecting plaintext closes the MITM vector where
// a hostile token-issuance response could swap `wss://api...` for
// `ws://attacker...` and leak the agent_token in cleartext over the first
// frame's Authorization header.
var ErrInsecureScheme = errors.New("transport: refusing to connect over plaintext scheme")

// runOnce performs a single connect-handshake-loop cycle. Returns the
// classified error; the outer loop in Run decides whether to back off or
// surface it.
func (c *WSClient) runOnce(ctx context.Context) error {
	if err := requireSecureScheme(c.URL, c.SkipPinning); err != nil {
		// Fatal: there is no retry that makes plaintext safe. Log once,
		// return immediately. The caller surfaces this to the user.
		c.log("error", "refusing insecure ws_url", "url", c.URL, "err", err.Error())
		return err
	}

	dialCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	httpClient, err := c.httpClient()
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.Token)
	header.Set("User-Agent", fmt.Sprintf("surfbot-cli/%s (%s/%s)", c.Version, runtime.GOOS, runtime.GOARCH))

	conn, resp, err := websocket.Dial(dialCtx, c.URL, &websocket.DialOptions{
		Subprotocols: []string{"surfbot.cli.v1"},
		HTTPClient:   httpClient,
		HTTPHeader:   header,
	})
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return c.classifyHandshakeError(resp.StatusCode, err)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(1 << 20)

	// Handshake: write client.hello, expect server.hello.
	if err := c.writeClientHello(ctx, conn); err != nil {
		return fmt.Errorf("write client.hello: %w", err)
	}
	hello, err := c.readServerHello(ctx, conn)
	if err != nil {
		return c.translateCloseErr(err)
	}
	c.lastHello.store(hello)
	if c.OnServerHello != nil {
		c.OnServerHello(hello)
	}
	if c.OnConnect != nil {
		c.OnConnect()
	}

	interval := clampHeartbeat(hello.HeartbeatIntervalSeconds, c.log)

	// Main loop. The heartbeat goroutine pushes periodic heartbeat frames;
	// the read loop classifies inbound frames and exits on close.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.runHeartbeats(loopCtx, conn, interval)
	}()
	go func() {
		defer wg.Done()
		c.runPings(loopCtx, conn, interval/2)
	}()

	err = c.readLoop(loopCtx, conn, interval)
	loopCancel()
	wg.Wait()
	if c.OnDisconnect != nil {
		c.OnDisconnect(err)
	}
	return c.translateCloseErr(err)
}

// httpClient builds an http.Client with pinning attached to the TLS config.
// The websocket dial uses this client for the HTTP handshake.
func (c *WSClient) httpClient() (*http.Client, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	host := u.Hostname()
	// Only attach pinning for wss/https. Plain ws/http (local dev) skip pinning entirely.
	rt := http.DefaultTransport
	if u.Scheme == "wss" || u.Scheme == "https" {
		validator := NewPinValidator(host, c.SkipPinning, true)
		validator.Log = c.log
		rt = &http.Transport{
			TLSHandshakeTimeout: handshakeTimeout,
			TLSClientConfig: &tls.Config{
				MinVersion:            tls.VersionTLS13,
				VerifyPeerCertificate: validator.VerifyPeerCertificate,
			},
			ForceAttemptHTTP2: true,
		}
	}
	return &http.Client{Transport: rt, Timeout: 0}, nil
}

// classifyHandshakeError maps HTTP status codes on the handshake response
// to the sentinel errors so commands like `login` can react sensibly (e.g.
// purge the token on a 401 right at dial time).
func (c *WSClient) classifyHandshakeError(status int, err error) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: handshake 401", ErrTokenInvalid)
	case http.StatusForbidden:
		return fmt.Errorf("%w: handshake 403", ErrOrgSuspended)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: handshake 429", ErrRateLimited)
	default:
		return fmt.Errorf("handshake: %d: %w", status, err)
	}
}

// translateCloseErr inspects a websocket close error and returns one of the
// sentinel errors when the close code matches a known custom code.
func (c *WSClient) translateCloseErr(err error) error {
	if err == nil {
		return nil
	}
	status := websocket.CloseStatus(err)
	switch status {
	case -1:
		// Not a close error.
		return err
	case closeCodeTokenRevoked:
		return ErrTokenRevoked
	case closeCodeTokenInvalid:
		return ErrTokenInvalid
	case closeCodeOrgSuspended:
		return ErrOrgSuspended
	case closeCodeRateLimited:
		return ErrRateLimited
	default:
		return err
	}
}

// writeClientHello serializes and sends the first frame on a fresh conn.
func (c *WSClient) writeClientHello(ctx context.Context, conn *websocket.Conn) error {
	payload := ClientHelloPayload{
		AgentID:      c.AgentID,
		Version:      c.Version,
		BuildCommit:  c.BuildCommit,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Kernel:       kernelVersion(),
		Hostname:     c.Hostname,
		Fingerprint:  c.Fingerprint,
		Capabilities: []string{"heartbeat.v1"},
	}
	raw, err := NewEnvelope(TypeClientHello, payload)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

// readServerHello reads exactly one TEXT frame, asserts it's a server.hello,
// and returns the payload.
func (c *WSClient) readServerHello(ctx context.Context, conn *websocket.Conn) (ServerHelloPayload, error) {
	var zero ServerHelloPayload
	readCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	mt, raw, err := conn.Read(readCtx)
	if err != nil {
		return zero, fmt.Errorf("read server.hello: %w", err)
	}
	if mt != websocket.MessageText {
		return zero, fmt.Errorf("server.hello: expected text frame, got %v", mt)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return zero, fmt.Errorf("server.hello envelope: %w", err)
	}
	if env.Type != TypeServerHello {
		return zero, fmt.Errorf("server.hello: got type %q", env.Type)
	}
	var p ServerHelloPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return zero, fmt.Errorf("server.hello payload: %w", err)
	}
	return p, nil
}

// runHeartbeats writes a heartbeat envelope every `interval`. Exits when
// loopCtx is canceled or the write fails (the read loop will surface the
// underlying close).
func (c *WSClient) runHeartbeats(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	// First heartbeat fires immediately so the server's last_seen_at is
	// touched right after handshake (otherwise the agent could appear
	// "stale" for up to `interval` post-connect on the dashboard).
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		payload := Sample(ctx)
		raw, err := NewEnvelope(TypeHeartbeat, payload)
		if err != nil {
			c.log("warn", "heartbeat marshal failed", "err", err.Error())
			continue
		}
		writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		err = conn.Write(writeCtx, websocket.MessageText, raw)
		cancel()
		if err != nil {
			c.log("info", "heartbeat write failed (conn likely closed)", "err", err.Error())
			return
		}
		c.lastBeat.store(time.Now())
	}
}

// runPings drives native WebSocket ping/pong control frames every
// interval/2 per SPEC §6.5. coder/websocket's Ping() blocks until either a
// matching pong arrives or the ctx deadline elapses, so a hung server
// (silent NAT timeout, half-open conn) trips this watchdog before the
// envelope-heartbeat path would. A single failed ping closes the conn with
// policy_violation; the read loop then exits and the outer Run reconnects.
func (c *WSClient) runPings(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				c.log("info", "ws ping failed (forcing reconnect)", "err", err.Error())
				_ = conn.Close(websocket.StatusPolicyViolation, "ping timeout")
				return
			}
		}
	}
}

// readLoop reads until close. Each frame is classified; heartbeat.ack and
// server.shutdown are recognized, every other type is logged-and-ignored
// (forward-compat). Heartbeat-timeout enforcement is via a watchdog that
// resets on each inbound frame.
func (c *WSClient) readLoop(ctx context.Context, conn *websocket.Conn, interval time.Duration) error {
	timeout := interval * 3
	watchdog := time.AfterFunc(timeout, func() {
		_ = conn.Close(websocket.StatusPolicyViolation, "no frames within heartbeat*3")
	})
	defer watchdog.Stop()

	for {
		mt, raw, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		watchdog.Reset(timeout)
		if mt != websocket.MessageText {
			c.log("warn", "ws: ignoring non-text frame", "type", mt.String())
			continue
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.log("warn", "ws: bad envelope", "err", err.Error())
			continue
		}
		switch env.Type {
		case TypeHeartbeatAck:
			// No required action today. Future: parse config_version, etc.
		case TypeServerShutdown:
			var p ServerShutdownPayload
			_ = json.Unmarshal(env.Payload, &p)
			c.log("info", "server shutdown announced", "reason", p.Reason, "downtime_s", p.EstimatedDowntimeSeconds)
			// Close cleanly and signal accelerated reconnect to outer loop.
			_ = conn.Close(websocket.StatusNormalClosure, "ack server.shutdown")
			return errServerShutdown
		default:
			c.log("debug", "ws: unknown envelope type (ignored)", "type", string(env.Type))
		}
	}
}

// errServerShutdown is an internal marker so the outer reconnect loop can
// pick a fast 50ms delay. Not exported; callers see a successful close +
// fresh reconnect attempt instead.
var errServerShutdown = errors.New("ws: server shutdown")

// backoffFor returns the next delay per SPEC §6.4: 1, 2, 4, 8, 16, 60 cap,
// with ±25% jitter. When rateLimited is true, the table is shifted right
// (double the delay) to honor close 4429.
func backoffFor(attempt int, rateLimited bool) time.Duration {
	table := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		60 * time.Second,
	}
	if rateLimited {
		// On 4429 we double the current delay AND skip a step.
		attempt++
	}
	if attempt >= len(table) {
		attempt = len(table) - 1
	}
	d := table[attempt]
	if rateLimited {
		d *= 2
	}
	// ±25% jitter.
	jitter := (rand.Float64()*2 - 1) * backoffJitter
	return d + time.Duration(float64(d)*jitter)
}

// clampHeartbeat enforces the [5s, 300s] range from SPEC §6.3.2. Server
// values outside the window fall back to the default with a warn log.
func clampHeartbeat(serverSeconds int, log func(level, msg string, kv ...any)) time.Duration {
	d := time.Duration(serverSeconds) * time.Second
	if d < heartbeatIntMin || d > heartbeatIntMax {
		log("warn", "server heartbeat_interval out of range, defaulting", "received_s", serverSeconds, "default_s", int(defaultHeartbeatInt.Seconds()))
		return defaultHeartbeatInt
	}
	return d
}

// kernelVersion returns a best-effort kernel/build identifier. Linux: read
// /proc/sys/kernel/osrelease; Darwin/Windows: runtime info string. Used in
// client.hello.kernel.
func kernelVersion() string {
	if runtime.GOOS == "linux" {
		if b, err := readFileTrimmed("/proc/sys/kernel/osrelease"); err == nil {
			return b
		}
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

func readFileTrimmed(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
