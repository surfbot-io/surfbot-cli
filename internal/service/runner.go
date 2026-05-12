// Package service hosts the OS-service lifecycle wrapper around the WSClient.
// The runner exists so that `surfbot-cli daemon run` (invoked by systemd /
// launchd / Windows SCM) shares the same connect-heartbeat-reconnect logic
// the foreground commands use, without re-implementing reconnect or close-code
// handling.
//
// Contract with WSClient post-hotfix #7:
//
//   - WSClient.Run() has its own internal reconnect ladder, so the runner
//     does NOT wrap it in a for-loop. One Run() call covers the whole
//     lifetime; it returns only on ctx cancellation or a fatal sentinel.
//   - When TokenStore is wired, WSClient auto-purges the token file on close
//     4401 / 4403. The runner mirrors that by deleting the metadata sibling
//     so the next `status` reports "no agent enrolled" instead of "online
//     for a ghost token".
//   - Sentinel errors handled here: ErrTokenRevoked, ErrTokenInvalid,
//     ErrOrgSuspended, ErrInsecureScheme. Anything else is a transient
//     network/disk error WSClient already retried internally; if it bubbles
//     out of Run() we log and exit so the OS service supervisor can restart.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/kardianos/service"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

// Logger is the minimal surface the runner needs from kardianos. Captured as
// an interface so unit tests can supply a fake without spinning up a real
// service.Service.
type Logger interface {
	Info(v ...any) error
	Infof(format string, a ...any) error
	Warning(v ...any) error
	Warningf(format string, a ...any) error
	Error(v ...any) error
	Errorf(format string, a ...any) error
}

// nopLogger is used when the program is exercised outside a kardianos
// service.Service (notably unit tests that don't pass a Logger).
type nopLogger struct{}

func (nopLogger) Info(...any) error             { return nil }
func (nopLogger) Infof(string, ...any) error    { return nil }
func (nopLogger) Warning(...any) error          { return nil }
func (nopLogger) Warningf(string, ...any) error { return nil }
func (nopLogger) Error(...any) error            { return nil }
func (nopLogger) Errorf(string, ...any) error   { return nil }

// Program implements service.Interface. Constructed by the daemon cobra
// command before s := service.New(prg, cfg); s.Run().
type Program struct {
	// ConfigDir is the directory holding agent.token + agent.json. Set by the
	// daemon command (typically transport.DefaultConfigDir()). Exposed so
	// tests can point at a tempdir without touching $XDG_CONFIG_HOME.
	ConfigDir string

	// Version / Commit propagate the binary's build-time identity into
	// client.hello so server-side telemetry tracks which agent build is
	// connected.
	Version string
	Commit  string

	// SkipPinning surfaces the --insecure-skip-pinning toggle from the daemon
	// install args. Off by default; the service install command exposes no
	// flag for it in v1 — operators with dev clusters edit the unit file by
	// hand.
	SkipPinning bool

	// runClient is overridable in tests so we can inject a stub instead of
	// the real WSClient. Production code leaves it nil and the default
	// transport-backed run is used.
	runClient func(ctx context.Context, c *transport.WSClient) error

	// loadTransport is the seam that builds the WSClient bits (token +
	// metadata + fingerprint). Overridable in tests to feed pre-baked
	// values without touching disk.
	loadTransport func(ConfigDir string) (token string, meta *transport.AgentMetadata, fp string, err error)

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	exitErr  error
	logger   Logger
	stopOnce sync.Once
}

// SetLogger wires the kardianos service.Logger (or a test fake) into the
// program. Called from daemonRunCmd before Run().
func (p *Program) SetLogger(l Logger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l == nil {
		p.logger = nopLogger{}
		return
	}
	p.logger = l
}

// log returns a never-nil Logger so callers can dereference safely without
// a per-call guard.
func (p *Program) log() Logger {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logger == nil {
		return nopLogger{}
	}
	return p.logger
}

// ExitError returns the terminal error from the background goroutine (nil if
// it exited cleanly or is still running). Used by daemonRunCmd to decide on
// a non-zero process exit, which lets systemd/launchd policy decide whether
// to restart us.
func (p *Program) ExitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

// Start satisfies service.Interface. Per kardianos docs it MUST return
// quickly; the actual work runs in a goroutine. Errors that surface during
// startup (no token, missing metadata) are logged via service.Logger and the
// goroutine returns — the service stays "started" so systemd doesn't
// hot-restart in a loop. The operator runs `daemon restart` after enroll.
func (p *Program) Start(s service.Service) error {
	if s != nil {
		// In real operation kardianos hands us a logger via the service
		// instance. Tests pass nil and call SetLogger directly.
		if lg, err := s.Logger(nil); err == nil && lg != nil {
			p.SetLogger(lg)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	go p.run(ctx)
	return nil
}

// run is the body of the supervised goroutine. Extracted from Start so the
// goroutine entry point is one statement and easy to test.
func (p *Program) run(ctx context.Context) {
	defer p.wg.Done()

	load := p.loadTransport
	if load == nil {
		load = defaultLoadTransport
	}
	token, meta, fp, err := load(p.ConfigDir)
	if err != nil {
		_ = p.log().Errorf("surfbot-cli daemon: load state: %v", err)
		p.setExitErr(err)
		return
	}
	if token == "" || meta == nil {
		_ = p.log().Error("surfbot-cli daemon: no agent enrolled — run `surfbot-cli login` or `enroll <token>` then restart the service")
		return
	}

	store := transport.NewTokenStoreAt(p.ConfigDir)
	hostname := meta.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	client := &transport.WSClient{
		URL:         meta.WSURL,
		Token:       token,
		AgentID:     meta.AgentID,
		Version:     p.Version,
		BuildCommit: p.Commit,
		Hostname:    hostname,
		Fingerprint: fp,
		SkipPinning: p.SkipPinning,
		// TokenStore wired so close 4401 / 4403 auto-purges the on-disk
		// token. We mirror by wiping the metadata sibling below.
		TokenStore: store,
		Logger:     p.wsLogger(),
	}

	_ = p.log().Infof("surfbot-cli daemon: connecting agent=%s ws=%s os=%s/%s", meta.AgentID, meta.WSURL, runtime.GOOS, runtime.GOARCH)

	runner := p.runClient
	if runner == nil {
		runner = func(ctx context.Context, c *transport.WSClient) error { return c.Run(ctx) }
	}
	runErr := runner(ctx, client)

	switch {
	case runErr == nil:
		_ = p.log().Info("surfbot-cli daemon: exiting cleanly (context canceled)")
	case errors.Is(runErr, transport.ErrTokenRevoked):
		// WSClient already removed the token file (via the TokenStore handle).
		// Wipe the sibling so `status` reports no-agent post-restart.
		_ = transport.DeleteMetadata(p.ConfigDir)
		_ = p.log().Warning("surfbot-cli daemon: token revoked by server (4401); local state purged, service exiting")
		p.setExitErr(runErr)
	case errors.Is(runErr, transport.ErrTokenInvalid):
		_ = transport.DeleteMetadata(p.ConfigDir)
		_ = p.log().Warning("surfbot-cli daemon: token invalid (4403); local state purged, service exiting")
		p.setExitErr(runErr)
	case errors.Is(runErr, transport.ErrOrgSuspended):
		// Token kept — org may be reactivated; no purge.
		_ = p.log().Warning("surfbot-cli daemon: organization suspended (4503); service exiting, token retained")
		p.setExitErr(runErr)
	case errors.Is(runErr, transport.ErrInsecureScheme):
		_ = p.log().Errorf("surfbot-cli daemon: insecure ws_url refused (%v); fix config and restart", runErr)
		p.setExitErr(runErr)
	default:
		_ = p.log().Errorf("surfbot-cli daemon: WSClient.Run returned: %v", runErr)
		p.setExitErr(runErr)
	}
}

// Stop satisfies service.Interface. Blocking: waits for the background
// goroutine to acknowledge ctx cancellation and exit. The WSClient internally
// sends close 1000 on its own ctx done, so a clean stop emits the right
// close frame to the server before the goroutine returns.
func (p *Program) Stop(_ service.Service) error {
	p.stopOnce.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
	})
	p.wg.Wait()
	return nil
}

// wsLogger adapts the kardianos Logger to the WSClient log hook signature
// (level, msg, kv...).
func (p *Program) wsLogger() func(level, msg string, kv ...any) {
	return func(level, msg string, kv ...any) {
		l := p.log()
		switch level {
		case "error":
			_ = l.Errorf("ws: %s %s", msg, fmtKV(kv))
		case "warn":
			_ = l.Warningf("ws: %s %s", msg, fmtKV(kv))
		default:
			_ = l.Infof("ws: %s %s", msg, fmtKV(kv))
		}
	}
}

func fmtKV(kv []any) string {
	if len(kv) == 0 {
		return ""
	}
	s := ""
	for i := 0; i+1 < len(kv); i += 2 {
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%v=%v", kv[i], kv[i+1])
	}
	return s
}

func (p *Program) setExitErr(err error) {
	p.mu.Lock()
	p.exitErr = err
	p.mu.Unlock()
}

// defaultLoadTransport reads token + metadata from the on-disk config dir
// and derives the machine fingerprint. Pulled out as a package-level fn so
// tests inject their own.
func defaultLoadTransport(dir string) (string, *transport.AgentMetadata, string, error) {
	if dir == "" {
		var err error
		dir, err = transport.DefaultConfigDir()
		if err != nil {
			return "", nil, "", fmt.Errorf("config dir: %w", err)
		}
	}
	store := transport.NewTokenStoreAt(dir)
	token, err := store.Load()
	if err != nil {
		return "", nil, "", fmt.Errorf("load token: %w", err)
	}
	meta, err := transport.LoadMetadata(dir)
	if err != nil {
		return "", nil, "", fmt.Errorf("load metadata: %w", err)
	}
	fp, _ := transport.Fingerprint()
	return token, meta, fp, nil
}
