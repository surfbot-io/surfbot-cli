package cli

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// Tests that call rootCmd.Find do NOT use t.Parallel: cobra's Find mutates
// per-command persistent-flag state on first traversal (see
// command.go:updateParentsPflags), which races between parallel callers.
// Running these serially is cheap and avoids touching cobra internals.
func TestDaemonCommandRegistered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"daemon"})
	if err != nil {
		t.Fatalf("rootCmd.Find(daemon): %v", err)
	}
	if cmd == nil || cmd.Use != "daemon" {
		t.Fatalf("daemon command not registered; got %+v", cmd)
	}

	wantSubs := map[string]bool{
		"install":   false,
		"uninstall": false,
		"start":     false,
		"stop":      false,
		"restart":   false,
		"status":    false,
		"run":       false,
	}
	for _, sub := range cmd.Commands() {
		if _, ok := wantSubs[sub.Use]; ok {
			wantSubs[sub.Use] = true
		}
	}
	for name, found := range wantSubs {
		if !found {
			t.Errorf("daemon subcommand %q not registered", name)
		}
	}
}

func TestDaemonRunCmdIsHidden(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"daemon", "run"})
	if err != nil {
		t.Fatalf("find daemon run: %v", err)
	}
	if !cmd.Hidden {
		t.Errorf("daemon run must be Hidden=true (it is invoked only by the service manager)")
	}
}

// TestServiceConfig exercises both the default branch and the
// skip-pinning branch of serviceConfig in a single test so the package-level
// daemonSkipPin flag is never mutated concurrently. (Splitting these into
// two parallel tests would race on daemonSkipPin.)
func TestServiceConfig(t *testing.T) {
	prev := daemonSkipPin
	defer func() { daemonSkipPin = prev }()

	t.Run("defaults", func(t *testing.T) {
		daemonSkipPin = false
		cfg := serviceConfig()
		if cfg.Name != "surfbot-cli" {
			t.Errorf("Name = %q, want surfbot-cli", cfg.Name)
		}
		if cfg.DisplayName != "Surfbot CLI" {
			t.Errorf("DisplayName = %q, want Surfbot CLI", cfg.DisplayName)
		}
		if !strings.Contains(cfg.Description, "surfbot.io") {
			t.Errorf("Description should reference surfbot.io: %q", cfg.Description)
		}
		if len(cfg.Arguments) != 2 || cfg.Arguments[0] != "daemon" || cfg.Arguments[1] != "run" {
			t.Errorf("Arguments = %v, want [daemon run]", cfg.Arguments)
		}
	})

	t.Run("with_skip_pinning", func(t *testing.T) {
		daemonSkipPin = true
		cfg := serviceConfig()
		if len(cfg.Arguments) != 3 || cfg.Arguments[2] != "--insecure-skip-pinning" {
			t.Errorf("Arguments = %v, want [daemon run --insecure-skip-pinning]", cfg.Arguments)
		}
	})
}

func TestRequireAdmin(t *testing.T) {
	t.Parallel()

	err := requireAdmin()
	switch {
	case runtime.GOOS == "windows":
		if err != nil {
			t.Errorf("requireAdmin on windows should return nil, got %v", err)
		}
	case os.Geteuid() == 0:
		if err != nil {
			t.Errorf("requireAdmin as root should return nil, got %v", err)
		}
	default:
		if err == nil {
			t.Errorf("requireAdmin as non-root should error; got nil")
		} else if !strings.Contains(err.Error(), "sudo") {
			t.Errorf("error should mention sudo so the operator knows how to re-run; got %q", err.Error())
		}
	}
}
