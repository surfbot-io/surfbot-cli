package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

var flagLogoutAPIBase string

func init() {
	logoutCmd.Flags().StringVar(&flagLogoutAPIBase, "api-base", "", "Override base URL of surfbot-api (defaults to host inferred from agent.json's ws_url)")
	rootCmd.AddCommand(logoutCmd)
}

var logoutCmd = &cobra.Command{
	Use:           "logout",
	Short:         "Revoke this agent's token and purge local state",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLogout,
}

func runLogout(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir, err := transport.DefaultConfigDir()
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	store := transport.NewTokenStoreAt(dir)
	token, err := store.Load()
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	if token == "" {
		pln(out, "No agent enrolled on this machine.")
		return nil
	}
	meta, _ := transport.LoadMetadata(dir)

	// Try server-side revoke. /cli/logout is documented in the spec but does
	// not exist in PR2.api yet — when missing, we accept the 404 and continue
	// with the local purge so the operator isn't blocked. When the endpoint
	// lands, no cli change is required.
	base := flagLogoutAPIBase
	if base == "" && meta != nil {
		base = inferAPIBaseURL(meta.WSURL)
	}
	if base == "" {
		base = DefaultAPIBaseURL
	}
	api := newAPIClient(base, Version)
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := api.postBearer(reqCtx, "/cli/logout", token, nil, nil); err != nil {
		switch ae := asAPIError(err); {
		case ae != nil && ae.Status == 404:
			pln(out, "(server-side revoke endpoint not yet available; purging local token only)")
		case ae != nil && ae.Status == 401:
			pln(out, "(token already invalid on the server)")
		case ae != nil:
			pf(out, "(server-side revoke returned %s; continuing with local purge)\n", ae.Error())
		case errors.Is(err, context.DeadlineExceeded):
			pln(out, "(server-side revoke timed out; continuing with local purge)")
		default:
			pf(out, "(server-side revoke failed: %v; continuing with local purge)\n", err)
		}
	} else {
		pln(out, "✓ Server-side token revoked.")
	}

	if err := store.Delete(); err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	if err := transport.DeleteMetadata(dir); err != nil {
		return fmt.Errorf("delete metadata: %w", err)
	}
	pln(out, "✓ Local token purged.")
	return nil
}
