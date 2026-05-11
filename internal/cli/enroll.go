package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

var (
	flagEnrollAPIBase         string
	flagEnrollInsecureSkipPin bool
	flagEnrollAutoConnect     bool
)

func init() {
	enrollCmd.Flags().StringVar(&flagEnrollAPIBase, "api-base", DefaultAPIBaseURL, "Base URL of the surfbot-api endpoint")
	enrollCmd.Flags().BoolVar(&flagEnrollInsecureSkipPin, "insecure-skip-pinning", false, "Bypass public-key pinning on the WS endpoint (NOT for production)")
	enrollCmd.Flags().BoolVar(&flagEnrollAutoConnect, "connect", true, "Connect the WebSocket after enrollment to verify the agent is reachable")
	rootCmd.AddCommand(enrollCmd)
}

var enrollCmd = &cobra.Command{
	Use:           "enroll <token>",
	Short:         "Enroll this machine using a one-time enrollment token",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runEnroll,
}

func runEnroll(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := strings.TrimSpace(args[0])
	if !strings.HasPrefix(token, "srfb_enr_") {
		return errors.New("enrollment token must start with 'srfb_enr_'")
	}

	hostname, _ := os.Hostname()
	fp, fpErr := transport.Fingerprint()
	if fpErr != nil {
		// Server requires fingerprint for client.hello — but POST /cli/enroll
		// accepts empty fingerprint (writes "" into agents row). We continue.
		pf(out, "(warning: could not derive fingerprint: %v)\n", fpErr)
	}

	api := newAPIClient(flagEnrollAPIBase, Version)
	var resp enrollResp
	req := enrollReq{
		EnrollmentToken: token,
		ClientVersion:   Version,
		ClientOS:        runtime.GOOS,
		ClientArch:      runtime.GOARCH,
		Hostname:        hostname,
		Fingerprint:     fp,
	}
	err := api.postJSON(ctx, "/cli/enroll", req, &resp)
	if err != nil {
		if ae := asAPIError(err); ae != nil {
			switch ae.Code {
			case "invalid_token":
				return errors.New("invalid enrollment token (malformed)")
			case "expired_or_consumed":
				return errors.New("enrollment token expired or already used")
			case "org_quota_exceeded":
				return errors.New("your organization has reached its agent quota")
			default:
				return fmt.Errorf("enrollment failed: %s", ae.Error())
			}
		}
		return fmt.Errorf("enrollment failed: %w", err)
	}

	dir, err := transport.DefaultConfigDir()
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	tokenStore := transport.NewTokenStoreAt(dir)
	if err := tokenStore.Save(resp.AgentToken); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	meta := &transport.AgentMetadata{
		AgentID:     resp.AgentID,
		OrgID:       resp.OrgID,
		WSURL:       resp.WSURL,
		Hostname:    hostname,
		TokenPrefix: tokenPrefix(resp.AgentToken),
		EnrolledAt:  time.Now().UTC().Format(time.RFC3339),
		Method:      "paste_token",
	}
	if err := transport.SaveMetadata(dir, meta); err != nil {
		return fmt.Errorf("save metadata: %w", err)
	}

	pf(out, "✓ Enrolled as %s (%s)\n", hostname, resp.AgentID)
	if !flagEnrollAutoConnect {
		return nil
	}
	return verifyConnection(ctx, out, resp.WSURL, resp.AgentToken, resp.AgentID, hostname, flagEnrollInsecureSkipPin)
}

type enrollReq struct {
	EnrollmentToken string `json:"enrollment_token"`
	ClientVersion   string `json:"client_version"`
	ClientOS        string `json:"client_os"`
	ClientArch      string `json:"client_arch"`
	Hostname        string `json:"hostname"`
	Fingerprint     string `json:"fingerprint"`
}

type enrollResp struct {
	AgentToken string `json:"agent_token"`
	AgentID    string `json:"agent_id"`
	OrgID      string `json:"org_id"`
	WSURL      string `json:"ws_url"`
}
