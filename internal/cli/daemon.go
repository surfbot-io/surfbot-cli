package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	svcrunner "github.com/surfbot-io/surfbot-cli/internal/service"
	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

// serviceName is the on-disk identifier for the service registration. It
// becomes:
//   - Linux systemd unit name (`surfbot-cli.service`)
//   - macOS launchd label (`surfbot-cli`)
//   - Windows service name
//
// Lower-case + no spaces is intentional: systemd unit names disallow spaces,
// and launchctl expects an FQDN-like label that is stable across upgrades.
const serviceName = "surfbot-cli"

// daemonSkipPin is the package-level toggle for --insecure-skip-pinning.
// Bound as a persistent flag on daemonCmd so it's accepted by both `daemon
// install` (where we write it into the service args) and `daemon run`
// (where systemd / launchd passes it back when invoking us). Persistent
// so subcommands inherit; off by default.
var daemonSkipPin bool

func init() {
	daemonCmd.PersistentFlags().BoolVar(&daemonSkipPin, "insecure-skip-pinning", false, "Bypass cert pinning on the WS endpoint (NOT for production)")
	daemonCmd.AddCommand(
		daemonInstallCmd,
		daemonUninstallCmd,
		daemonStartCmd,
		daemonStopCmd,
		daemonRestartCmd,
		daemonStatusCmd,
		daemonRunCmd,
	)
	rootCmd.AddCommand(daemonCmd)
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage surfbot-cli as a system service (systemd / launchd / Windows SCM)",
	Long: `Manage the OS-level service that keeps surfbot-cli connected.

Most users only need:

  sudo surfbot-cli daemon install
  surfbot-cli login                      # one-time enroll
  sudo surfbot-cli daemon restart        # pick up the freshly-saved token

Logs are routed to the platform's standard backend: journalctl on Linux,
the unified logging system on macOS, Event Viewer on Windows.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var daemonInstallCmd = &cobra.Command{
	Use:           "install",
	Short:         "Register surfbot-cli as a system service",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDaemonInstall,
}

var daemonUninstallCmd = &cobra.Command{
	Use:           "uninstall",
	Short:         "Remove the system service registration",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDaemonUninstall,
}

var daemonStartCmd = &cobra.Command{
	Use:           "start",
	Short:         "Start the installed service",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDaemonControl("start"),
}

var daemonStopCmd = &cobra.Command{
	Use:           "stop",
	Short:         "Stop the installed service",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDaemonControl("stop"),
}

var daemonRestartCmd = &cobra.Command{
	Use:           "restart",
	Short:         "Restart the installed service (use this after `login` / `enroll`)",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDaemonControl("restart"),
}

var daemonStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Report whether the system service is installed / running",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDaemonStatus,
}

// daemonRunCmd is what the OS service manager invokes. Hidden so the user
// doesn't run it interactively (it would block until the process is killed
// and bypass kardianos' service lifecycle integration).
var daemonRunCmd = &cobra.Command{
	Use:           "run",
	Hidden:        true,
	Short:         "Internal: invoked by the OS service manager. Do not run directly.",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runDaemonRun,
}

// serviceConfig builds the kardianos config the install/control commands
// share. Kept in one place so install/uninstall/start/stop see the exact
// same Name / Arguments and the service manager can correlate them.
func serviceConfig() *service.Config {
	args := []string{"daemon", "run"}
	if daemonSkipPin {
		args = append(args, "--insecure-skip-pinning")
	}
	return &service.Config{
		Name:        serviceName,
		DisplayName: "Surfbot CLI",
		Description: "Surfbot cloud-bridge endpoint (https://surfbot.io)",
		Arguments:   args,
	}
}

// newServiceProgram constructs the Program used by daemon run + by the
// install command's service.New() call (kardianos requires an Interface
// even on Install so it can detect the runtime model).
func newServiceProgram() (*svcrunner.Program, error) {
	dir, err := transport.DefaultConfigDir()
	if err != nil {
		return nil, fmt.Errorf("config dir: %w", err)
	}
	return &svcrunner.Program{
		ConfigDir:   dir,
		Version:     Version,
		Commit:      Commit,
		SkipPinning: daemonSkipPin,
	}, nil
}

// runDaemonInstall registers the service via kardianos. Refuses to run as
// non-root: install writes to /etc/systemd/system, /Library/LaunchDaemons,
// or the Windows SCM, all of which require admin.
func runDaemonInstall(cmd *cobra.Command, _ []string) error {
	if err := requireAdmin(); err != nil {
		return err
	}
	prg, err := newServiceProgram()
	if err != nil {
		return err
	}
	s, err := service.New(prg, serviceConfig())
	if err != nil {
		return fmt.Errorf("service.New: %w", err)
	}
	if err := s.Install(); err != nil {
		return fmt.Errorf("service install: %w", err)
	}
	out := cmd.OutOrStdout()
	pf(out, "✓ Installed service %q.\n", serviceName)
	pf(out, "  Next: enroll this machine (`surfbot-cli login` or `enroll <token>`),\n")
	pf(out, "  then `sudo surfbot-cli daemon start` (or `daemon restart` if it's running).\n")
	platformHints(out)
	return nil
}

// runDaemonUninstall removes the service registration. Also requires root.
func runDaemonUninstall(cmd *cobra.Command, _ []string) error {
	if err := requireAdmin(); err != nil {
		return err
	}
	prg, err := newServiceProgram()
	if err != nil {
		return err
	}
	s, err := service.New(prg, serviceConfig())
	if err != nil {
		return fmt.Errorf("service.New: %w", err)
	}
	// Best-effort stop before uninstall; on a stopped service the error is
	// expected and silently ignored. service.ErrNotInstalled bubbles back if
	// the service isn't registered.
	_ = s.Stop()
	if err := s.Uninstall(); err != nil {
		return fmt.Errorf("service uninstall: %w", err)
	}
	pf(cmd.OutOrStdout(), "✓ Uninstalled service %q.\n", serviceName)
	return nil
}

// runDaemonControl returns a RunE closure for start / stop / restart. The
// shape is identical aside from which kardianos method is called; the
// closure keeps the cobra wiring DRY.
func runDaemonControl(action string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if err := requireAdmin(); err != nil {
			return err
		}
		prg, err := newServiceProgram()
		if err != nil {
			return err
		}
		s, err := service.New(prg, serviceConfig())
		if err != nil {
			return fmt.Errorf("service.New: %w", err)
		}
		var opErr error
		switch action {
		case "start":
			opErr = s.Start()
		case "stop":
			opErr = s.Stop()
		case "restart":
			opErr = s.Restart()
		default:
			return fmt.Errorf("daemon: unknown control action %q", action)
		}
		if opErr != nil {
			return fmt.Errorf("service %s: %w", action, opErr)
		}
		pf(cmd.OutOrStdout(), "✓ Service %s issued.\n", action)
		return nil
	}
}

// runDaemonStatus reports installed/running/stopped without requiring root.
// Read-only query — querying the service manager doesn't need admin on any
// supported platform.
func runDaemonStatus(cmd *cobra.Command, _ []string) error {
	prg, err := newServiceProgram()
	if err != nil {
		return err
	}
	s, err := service.New(prg, serviceConfig())
	if err != nil {
		return fmt.Errorf("service.New: %w", err)
	}
	st, err := s.Status()
	out := cmd.OutOrStdout()
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		pf(out, "service: not installed (run `sudo surfbot-cli daemon install`)\n")
		return nil
	case err != nil:
		return fmt.Errorf("service status: %w", err)
	}
	pf(out, "service: %s (state=%s)\n", serviceName, humanServiceStatus(st))
	return nil
}

// humanServiceStatus maps kardianos' enum to a readable string.
func humanServiceStatus(st service.Status) string {
	switch st {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// runDaemonRun is the entry point kardianos invokes via the unit file's
// ExecStart. It blocks until the service manager signals stop.
//
// IMPORTANT: this runs in the same binary as the CLI, but in a non-interactive
// context (systemd / launchd / SCM). Stdout is captured by the service
// manager — we route all logging through service.Logger so journalctl /
// Console / Event Viewer get structured entries instead of bare prints.
func runDaemonRun(_ *cobra.Command, _ []string) error {
	prg, err := newServiceProgram()
	if err != nil {
		return err
	}
	s, err := service.New(prg, serviceConfig())
	if err != nil {
		return fmt.Errorf("service.New: %w", err)
	}
	// s.Run() blocks: calls prg.Start, waits for stop signal from the OS
	// service manager, calls prg.Stop, returns. After that returns, propagate
	// the program's terminal error so the process exit code reflects whether
	// the daemon shut down cleanly or hit a fatal sentinel.
	if err := s.Run(); err != nil {
		return fmt.Errorf("service run: %w", err)
	}
	return prg.ExitError()
}

// requireAdmin gates install / uninstall / start / stop / restart. On Unix
// we check Geteuid; on Windows we accept any user and let the SCM's own
// elevation prompt handle it (the operator typically runs an elevated PS
// prompt, and kardianos returns a clear error on access denied).
func requireAdmin() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if os.Geteuid() == 0 {
		return nil
	}
	return errors.New("this command requires root — re-run with sudo")
}

// platformHints prints OS-specific log inspection commands at the end of
// install, so the operator immediately knows where to look for output.
func platformHints(out io.Writer) {
	switch runtime.GOOS {
	case "linux":
		pf(out, "\nLogs: journalctl -u %s -f\n", serviceName)
	case "darwin":
		pf(out, "\nLogs: log stream --predicate 'process == \"%s\"'\n", serviceName)
	case "windows":
		pf(out, "\nLogs: Event Viewer → Windows Logs → Application (source: %s)\n", serviceName)
	}
}
