package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

// captureLogger collects every log line so tests can assert which classifier
// branch ran (info / warning / error) without coupling to the exact format
// string.
type captureLogger struct {
	mu    sync.Mutex
	infos []string
	warns []string
	errs  []string
}

func (c *captureLogger) push(slot *[]string, line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*slot = append(*slot, line)
}

func joinAny(parts []any) string {
	s := make([]string, 0, len(parts))
	for _, p := range parts {
		s = append(s, fmt.Sprintf("%v", p))
	}
	return strings.Join(s, " ")
}

func (c *captureLogger) Info(v ...any) error    { c.push(&c.infos, joinAny(v)); return nil }
func (c *captureLogger) Warning(v ...any) error { c.push(&c.warns, joinAny(v)); return nil }
func (c *captureLogger) Error(v ...any) error   { c.push(&c.errs, joinAny(v)); return nil }
func (c *captureLogger) Infof(format string, a ...any) error {
	c.push(&c.infos, fmt.Sprintf(format, a...))
	return nil
}
func (c *captureLogger) Warningf(format string, a ...any) error {
	c.push(&c.warns, fmt.Sprintf(format, a...))
	return nil
}
func (c *captureLogger) Errorf(format string, a ...any) error {
	c.push(&c.errs, fmt.Sprintf(format, a...))
	return nil
}

func (c *captureLogger) any(predicate func(line string) bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, slot := range [][]string{c.infos, c.warns, c.errs} {
		for _, l := range slot {
			if predicate(l) {
				return true
			}
		}
	}
	return false
}

func TestProgram_StopWaitsForGoroutine(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	prg := &Program{
		ConfigDir: "/tmp/does-not-matter-test-overrides-loader",
		Version:   "test",
		Commit:    "test",
		loadTransport: func(string) (string, *transport.AgentMetadata, string, error) {
			return "srfb_agt_dummy", &transport.AgentMetadata{
				AgentID: "ag_test",
				WSURL:   "wss://example.invalid/cli/ws",
			}, "", nil
		},
		runClient: func(ctx context.Context, _ *transport.WSClient) error {
			<-ctx.Done() // block until Stop cancels
			close(released)
			return nil
		},
	}
	prg.SetLogger(&captureLogger{})

	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Stop must block until the goroutine actually returns. We verify by
	// observing that `released` is closed by the time Stop returns.
	done := make(chan error, 1)
	go func() { done <- prg.Stop(nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s")
	}
	select {
	case <-released:
	default:
		t.Fatal("runClient goroutine did not see ctx cancellation before Stop returned")
	}
}

func TestProgram_NoTokenLogsAndExits(t *testing.T) {
	t.Parallel()

	log := &captureLogger{}
	prg := &Program{
		loadTransport: func(string) (string, *transport.AgentMetadata, string, error) {
			return "", nil, "", nil
		},
		runClient: func(_ context.Context, _ *transport.WSClient) error {
			t.Fatalf("runClient must not be invoked when token is empty")
			return nil
		},
	}
	prg.SetLogger(log)

	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := prg.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !log.any(func(l string) bool { return strings.Contains(l, "no agent enrolled") }) {
		t.Fatalf("expected 'no agent enrolled' log; got: errs=%v warns=%v infos=%v", log.errs, log.warns, log.infos)
	}
	if got := prg.ExitError(); got != nil {
		t.Fatalf("ExitError should be nil for no-token branch (waiting for enrollment is not a failure), got: %v", got)
	}
}

func TestProgram_ExitsCleanlyOnRevoke(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Seed a metadata file so the test can verify DeleteMetadata wipes it on
	// revoke (mirroring the hotfix #7 contract).
	if err := transport.SaveMetadata(dir, &transport.AgentMetadata{
		AgentID:  "ag_test",
		WSURL:    "wss://example.invalid/cli/ws",
		Hostname: "test-host",
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	// And a token file so the synthetic loader doesn't have to spoof it.
	store := transport.NewTokenStoreAt(dir)
	if err := store.Save("srfb_agt_dummytoken"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	log := &captureLogger{}
	prg := &Program{
		ConfigDir: dir,
		runClient: func(_ context.Context, _ *transport.WSClient) error {
			return transport.ErrTokenRevoked
		},
	}
	prg.SetLogger(log)

	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := prg.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(prg.ExitError(), transport.ErrTokenRevoked) {
		t.Fatalf("ExitError = %v, want ErrTokenRevoked", prg.ExitError())
	}
	meta, err := transport.LoadMetadata(dir)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta != nil {
		t.Fatalf("metadata file should have been deleted on revoke, still found: %+v", meta)
	}
	if !log.any(func(l string) bool { return strings.Contains(l, "token revoked") }) {
		t.Fatalf("expected 'token revoked' log line; warns=%v", log.warns)
	}
}

func TestProgram_TokenInvalidPurgesMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := transport.SaveMetadata(dir, &transport.AgentMetadata{
		AgentID: "ag_test",
		WSURL:   "wss://example.invalid/cli/ws",
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	// Seed a token so run() reaches the runClient invocation (without a token
	// it would short-circuit to the "no agent enrolled" branch and never
	// surface the sentinel).
	if err := transport.NewTokenStoreAt(dir).Save("srfb_agt_dummytoken"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	prg := &Program{
		ConfigDir: dir,
		runClient: func(_ context.Context, _ *transport.WSClient) error {
			return transport.ErrTokenInvalid
		},
	}
	prg.SetLogger(&captureLogger{})
	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := prg.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(prg.ExitError(), transport.ErrTokenInvalid) {
		t.Fatalf("ExitError = %v, want ErrTokenInvalid", prg.ExitError())
	}
	meta, _ := transport.LoadMetadata(dir)
	if meta != nil {
		t.Fatalf("metadata should be wiped on ErrTokenInvalid, still found: %+v", meta)
	}
}

func TestProgram_OrgSuspendedKeepsToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := transport.SaveMetadata(dir, &transport.AgentMetadata{
		AgentID:  "ag_test",
		WSURL:    "wss://example.invalid/cli/ws",
		Hostname: "test-host",
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	store := transport.NewTokenStoreAt(dir)
	if err := store.Save("srfb_agt_dummytoken"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	prg := &Program{
		ConfigDir: dir,
		runClient: func(_ context.Context, _ *transport.WSClient) error {
			return transport.ErrOrgSuspended
		},
	}
	prg.SetLogger(&captureLogger{})
	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := prg.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(prg.ExitError(), transport.ErrOrgSuspended) {
		t.Fatalf("ExitError = %v, want ErrOrgSuspended", prg.ExitError())
	}
	// Metadata + token must NOT be deleted — org may be re-enabled.
	meta, _ := transport.LoadMetadata(dir)
	if meta == nil {
		t.Fatalf("metadata should be retained on org-suspended; got nil")
	}
	tok, _ := store.Load()
	if tok == "" {
		t.Fatalf("token should be retained on org-suspended")
	}
}

func TestProgram_InsecureSchemeIsFatal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := transport.SaveMetadata(dir, &transport.AgentMetadata{
		AgentID:  "ag_test",
		WSURL:    "ws://api.example.com/cli/ws",
		Hostname: "test-host",
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	store := transport.NewTokenStoreAt(dir)
	if err := store.Save("srfb_agt_dummytoken"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	prg := &Program{
		ConfigDir: dir,
		runClient: func(_ context.Context, _ *transport.WSClient) error {
			return transport.ErrInsecureScheme
		},
	}
	prg.SetLogger(&captureLogger{})
	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := prg.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(prg.ExitError(), transport.ErrInsecureScheme) {
		t.Fatalf("ExitError = %v, want ErrInsecureScheme", prg.ExitError())
	}
}
