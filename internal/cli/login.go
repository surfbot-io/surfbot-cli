package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

// DefaultAPIBaseURL is the staging endpoint baked into the binary. Overridable
// via --api-base on every authenticating cobra command.
const DefaultAPIBaseURL = "https://api-staging.surfbot.io"

var (
	flagAPIBase           string
	flagInsecureSkipPin   bool
	flagLoginAutoConnect  bool
	flagLoginPollDuration time.Duration
)

func init() {
	loginCmd.Flags().StringVar(&flagAPIBase, "api-base", DefaultAPIBaseURL, "Base URL of the surfbot-api endpoint")
	loginCmd.Flags().BoolVar(&flagInsecureSkipPin, "insecure-skip-pinning", false, "Bypass public-key pinning on the WS endpoint (NOT for production)")
	loginCmd.Flags().BoolVar(&flagLoginAutoConnect, "connect", true, "Connect the WebSocket after enrollment to verify the agent is reachable")
	loginCmd.Flags().DurationVar(&flagLoginPollDuration, "poll-timeout", 11*time.Minute, "Hard ceiling on the device-code polling loop")
	rootCmd.AddCommand(loginCmd)
}

var loginCmd = &cobra.Command{
	Use:           "login",
	Short:         "Enroll this machine via device-code flow",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLogin,
}

func runLogin(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hostname, _ := os.Hostname()
	api := newAPIClient(flagAPIBase, Version, flagInsecureSkipPin)

	// 1. POST /cli/device/code.
	var resp deviceCodeResp
	req := deviceCodeReq{
		ClientVersion: Version,
		ClientOS:      runtime.GOOS,
		ClientArch:    runtime.GOARCH,
		Hostname:      hostname,
	}
	if err := api.postJSON(ctx, "/cli/device/code", req, &resp); err != nil {
		return fmt.Errorf("device/code: %w", err)
	}
	if resp.Interval <= 0 {
		resp.Interval = 5
	}

	// 2. Print user_code + verification URI prominently. SPEC §10 / wireframe S1.
	printActivation(out, resp)

	// 3. Poll /cli/device/token until success or timeout.
	dir, err := transport.DefaultConfigDir()
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	tokenStore := transport.NewTokenStoreAt(dir)

	poll := time.Duration(resp.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	if flagLoginPollDuration > 0 {
		if alt := time.Now().Add(flagLoginPollDuration); alt.Before(deadline) {
			deadline = alt
		}
	}
	tokenResp, err := pollDeviceToken(ctx, api, resp.DeviceCode, poll, deadline, out)
	if err != nil {
		return err
	}

	// 4. Persist token + metadata.
	if err := tokenStore.Save(tokenResp.AgentToken); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	meta := &transport.AgentMetadata{
		AgentID:     tokenResp.AgentID,
		OrgID:       tokenResp.OrgID,
		WSURL:       tokenResp.WSURL,
		Hostname:    hostname,
		TokenPrefix: tokenPrefix(tokenResp.AgentToken),
		EnrolledAt:  time.Now().UTC().Format(time.RFC3339),
		Method:      "device_code",
	}
	if err := transport.SaveMetadata(dir, meta); err != nil {
		return fmt.Errorf("save metadata: %w", err)
	}

	pf(out, "\n✓ Authorized as %s (%s)\n", hostname, tokenResp.AgentID)

	if !flagLoginAutoConnect {
		return nil
	}
	return verifyConnection(ctx, out, tokenResp.WSURL, tokenResp.AgentToken, tokenResp.AgentID, hostname, flagInsecureSkipPin)
}

// printActivation prints the activation banner per wireframe S1+S3.
func printActivation(w io.Writer, r deviceCodeResp) {
	pln(w, "\nVisit:")
	pf(w, "  %s\n\n", r.VerificationURI)
	pln(w, "And enter this code:")
	pf(w, "  %s\n\n", r.UserCode)
	pln(w, "Waiting for activation...")
}

// pollDeviceToken runs the polling loop per RFC 8628 §3.5: authorization_pending
// → continue; slow_down → double interval; expired_token / access_denied →
// abort. 200 → return the token.
func pollDeviceToken(
	ctx context.Context,
	api *apiClient,
	deviceCode string,
	initialInterval time.Duration,
	deadline time.Time,
	out io.Writer,
) (*deviceTokenResp, error) {
	interval := initialInterval
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, errors.New("device code expired before authorization")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		var tr deviceTokenResp
		err := api.postJSON(ctx, "/cli/device/token", deviceTokenReq{DeviceCode: deviceCode}, &tr)
		if err == nil {
			return &tr, nil
		}
		ae := asAPIError(err)
		if ae == nil {
			// Network error: log and keep polling.
			pf(out, "  (transient error: %v; will retry)\n", err)
			continue
		}
		switch ae.Code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval *= 2
			pf(out, "  (server asked us to slow down; polling every %s now)\n", interval)
		case "expired_token":
			return nil, errors.New("activation code expired; please re-run `surfbot-cli login`")
		case "access_denied":
			return nil, errors.New("activation denied; aborting")
		case "invalid_device_code":
			return nil, errors.New("device code is no longer valid; please re-run `surfbot-cli login`")
		default:
			return nil, fmt.Errorf("device/token: %s", ae.Error())
		}
	}
}

// verifyConnection opens the WS, waits for server.hello, then disconnects.
// Used by `login` to confirm the freshly-enrolled token actually works.
func verifyConnection(ctx context.Context, out io.Writer, wsURL, token, agentID, hostname string, skipPin bool) error {
	fp, err := transport.Fingerprint()
	if err != nil {
		// Fingerprint failures are best-effort; emit empty fingerprint and
		// the server's strict check will return a clean error.
		pf(out, "(could not derive fingerprint: %v)\n", err)
		fp = ""
	}
	client := &transport.WSClient{
		URL:         wsURL,
		Token:       token,
		AgentID:     agentID,
		Version:     Version,
		BuildCommit: Commit,
		Hostname:    hostname,
		Fingerprint: fp,
		SkipPinning: skipPin,
		Logger: func(level, msg string, kv ...any) {
			// Silent by default; surface warns only.
			if level == "warn" {
				pf(out, "  [%s] %s %v\n", level, msg, kv)
			}
		},
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	done := make(chan struct{})
	client.OnServerHello = func(_ transport.ServerHelloPayload) {
		pln(out, "✓ Connected (heartbeat ready, you can run `surfbot-cli status` to monitor)")
		close(done)
		cancel()
	}
	err = client.Run(verifyCtx)
	select {
	case <-done:
		return nil
	default:
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		pf(out, "(connection verification failed: %v)\n", err)
		return nil // do not fail enrollment because verification failed
	}
	return nil
}

// tokenPrefix returns the 13-char prefix logged + persisted to metadata.
func tokenPrefix(token string) string {
	if len(token) <= 13 {
		return token
	}
	return token[:13]
}

// ── REST DTOs ─────────────────────────────────────────────────────────────────

type deviceCodeReq struct {
	ClientVersion string `json:"client_version"`
	ClientOS      string `json:"client_os"`
	ClientArch    string `json:"client_arch"`
	Hostname      string `json:"hostname"`
}

type deviceCodeResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type deviceTokenReq struct {
	DeviceCode string `json:"device_code"`
}

type deviceTokenResp struct {
	AgentToken string `json:"agent_token"`
	AgentID    string `json:"agent_id"`
	OrgID      string `json:"org_id"`
	WSURL      string `json:"ws_url"`
}
