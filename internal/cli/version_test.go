package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestVersionCmd(t *testing.T) {
	origVersion, origCommit, origBuildDate := Version, Commit, BuildDate
	t.Cleanup(func() {
		Version, Commit, BuildDate = origVersion, origCommit, origBuildDate
	})

	Version = "1.2.3"
	Commit = "abcdef1"
	BuildDate = "2026-05-10T12:00:00Z"

	var buf bytes.Buffer
	versionCmd.SetOut(&buf)
	if err := versionCmd.RunE(versionCmd, nil); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	out := buf.String()
	wantSubstrings := []string{
		"surfbot-cli 1.2.3",
		"commit abcdef1",
		"built 2026-05-10T12:00:00Z",
		runtime.GOOS + "/" + runtime.GOARCH,
		runtime.Version(),
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q\nfull output: %s", want, out)
		}
	}
}

func TestRootCmd_HasVersion(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Name() == "version" {
			return
		}
	}
	t.Fatal("rootCmd does not register the 'version' subcommand")
}
