package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

func TestStatus_NoToken_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	var out bytes.Buffer
	statusCmd.SetOut(&out)
	statusCmd.SetErr(&out)
	statusCmd.SetContext(context.Background())

	err := runStatus(statusCmd, nil)
	if err == nil {
		t.Fatalf("expected error when no token enrolled")
	}
	if !strings.Contains(err.Error(), "no agent enrolled") {
		t.Fatalf("error doesn't mention enrolment: %v", err)
	}
}

func TestStatus_WithMetadata_PrintsFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	store := transport.NewTokenStoreAt(dir + "/surfbot")
	if err := store.Save("srfb_agt_8a7b3c2d1e5f6789abcdef01"); err != nil {
		t.Fatalf("save token: %v", err)
	}
	meta := &transport.AgentMetadata{
		AgentID:     "ag_statustest",
		OrgID:       "org_xxx",
		WSURL:       "ws://127.0.0.1:0/cli/ws",
		Hostname:    "statushost",
		TokenPrefix: "srfb_agt_8a7b",
		EnrolledAt:  time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339),
		Method:      "device_code",
	}
	if err := transport.SaveMetadata(dir+"/surfbot", meta); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	flagStatusNoConnect = true
	var out bytes.Buffer
	statusCmd.SetOut(&out)
	statusCmd.SetErr(&out)
	statusCmd.SetContext(context.Background())

	if err := runStatus(statusCmd, nil); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"agent_id:",
		"ag_statustest",
		"display_name: statushost",
		"org:          org_xxx",
		"ws_url:",
		"heartbeat:",
		"version:",
		"config:",
		"token:",
		"srfb_agt_8a7b",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q\n---\n%s", want, got)
		}
	}
}
