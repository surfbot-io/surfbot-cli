package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

var (
	flagStatusNoConnect bool
	flagStatusSkipPin   bool
)

func init() {
	statusCmd.Flags().BoolVar(&flagStatusNoConnect, "no-connect", false, "Skip the WS round-trip and report only the locally cached state")
	statusCmd.Flags().BoolVar(&flagStatusSkipPin, "insecure-skip-pinning", false, "Bypass public-key pinning on the WS endpoint (NOT for production)")
	rootCmd.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show this agent's enrollment + live connection state",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runStatus,
}

// runStatus reports:
//   - agent_id, display_name, hostname (from local metadata)
//   - status: online / offline / no-token (with a brief WS handshake unless
//     --no-connect)
//   - ws_url, heartbeat interval, version, config path
//
// The /cli/whoami endpoint in the spec is NOT implemented in PR2.api yet;
// until it lands, status uses local metadata + a short-lived WS handshake
// to verify the agent is live. The fallback is documented in the PR.
func runStatus(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	dir, err := transport.DefaultConfigDir()
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	store := transport.NewTokenStoreAt(dir)
	token, err := store.Load()
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	meta, err := transport.LoadMetadata(dir)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if token == "" || meta == nil {
		return errors.New("no agent enrolled on this machine — run `surfbot-cli login` or `surfbot-cli enroll <token>`")
	}

	var statusLine string
	var hbInterval int
	switch {
	case flagStatusNoConnect:
		statusLine = "offline (skipped probe)"
	default:
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		ok, intervalSec, probeErr := probeWS(ctx, meta.WSURL, token, meta.AgentID, meta.Hostname, flagStatusSkipPin)
		switch {
		case ok:
			statusLine = "online"
			hbInterval = intervalSec
		case errors.Is(probeErr, transport.ErrTokenRevoked):
			statusLine = "revoked"
		case errors.Is(probeErr, transport.ErrTokenInvalid):
			statusLine = "token-invalid"
		default:
			statusLine = "offline"
		}
	}

	enrolled, _ := time.Parse(time.RFC3339, meta.EnrolledAt)
	printStatus(out, statusLine, meta, enrolled, hbInterval)
	return nil
}

func printStatus(out io.Writer, statusLine string, m *transport.AgentMetadata, enrolled time.Time, hbInterval int) {
	cfgPath := transport.ConfigPath()
	pf(out, "agent_id:     %s\n", m.AgentID)
	pf(out, "display_name: %s\n", m.Hostname)
	pf(out, "org:          %s\n", m.OrgID)
	if !enrolled.IsZero() {
		pf(out, "status:       %s (enrolled %s)\n", statusLine, humanDuration(time.Since(enrolled)))
	} else {
		pf(out, "status:       %s\n", statusLine)
	}
	pf(out, "ws_url:       %s\n", m.WSURL)
	if hbInterval > 0 {
		pf(out, "heartbeat:    interval %ds (negotiated)\n", hbInterval)
	} else {
		pf(out, "heartbeat:    interval 30s (default)\n")
	}
	pf(out, "version:      %s (commit %s, %s)\n", Version, Commit, runtime.Version())
	pf(out, "config:       %s\n", cfgPath)
	pf(out, "token:        %s•••••••\n", m.TokenPrefix)
}

// probeWS opens a WS handshake, waits for server.hello, then disconnects.
// Returns (true, interval, nil) on success; surfaces revoke/invalid sentinels
// distinctly so the caller can render a specific status.
func probeWS(
	ctx context.Context,
	wsURL, token, agentID, hostname string,
	skipPin bool,
) (bool, int, error) {
	fp, _ := transport.Fingerprint()
	client := &transport.WSClient{
		URL:         wsURL,
		Token:       token,
		AgentID:     agentID,
		Version:     Version,
		BuildCommit: Commit,
		Hostname:    hostname,
		Fingerprint: fp,
		SkipPinning: skipPin,
		Logger:      func(string, string, ...any) {},
	}
	intervalCh := make(chan int, 1)
	client.OnServerHello = func(p transport.ServerHelloPayload) {
		select {
		case intervalCh <- p.HeartbeatIntervalSeconds:
		default:
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Run(runCtx) }()

	select {
	case interval := <-intervalCh:
		cancel()
		<-errCh
		return true, interval, nil
	case err := <-errCh:
		if errors.Is(err, transport.ErrTokenRevoked) ||
			errors.Is(err, transport.ErrTokenInvalid) ||
			errors.Is(err, transport.ErrOrgSuspended) {
			return false, 0, err
		}
		return false, 0, err
	case <-ctx.Done():
		cancel()
		<-errCh
		return false, 0, ctx.Err()
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}
