package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnroll_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cli/enroll" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			EnrollmentToken string `json:"enrollment_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.HasPrefix(req.EnrollmentToken, "srfb_enr_") {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_token"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_token": "srfb_agt_xyz1234567890abcdefghij",
			"agent_id":    "ag_enrolltest",
			"org_id":      "org_xxx",
			"ws_url":      "ws://127.0.0.1:0/cli/ws",
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	flagEnrollAPIBase = srv.URL
	flagEnrollAutoConnect = false

	var out bytes.Buffer
	enrollCmd.SetOut(&out)
	enrollCmd.SetErr(&out)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	enrollCmd.SetContext(ctx)

	if err := runEnroll(enrollCmd, []string{"srfb_enr_validtoken1234"}); err != nil {
		t.Fatalf("runEnroll: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ Enrolled as") {
		t.Fatalf("missing success line; output:\n%s", got)
	}
	if !strings.Contains(got, "ag_enrolltest") {
		t.Fatalf("missing agent_id; output:\n%s", got)
	}
}

func TestEnroll_RejectsMalformedToken(t *testing.T) {
	flagEnrollAPIBase = "http://127.0.0.1:0"
	flagEnrollAutoConnect = false

	var out bytes.Buffer
	enrollCmd.SetOut(&out)
	enrollCmd.SetErr(&out)
	enrollCmd.SetContext(context.Background())

	err := runEnroll(enrollCmd, []string{"not_a_valid_token"})
	if err == nil {
		t.Fatalf("expected error for malformed token")
	}
	if !strings.Contains(err.Error(), "srfb_enr_") {
		t.Fatalf("error didn't mention prefix; got %v", err)
	}
}

func TestEnroll_SurfacesExpiredOrConsumed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "expired_or_consumed"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	flagEnrollAPIBase = srv.URL
	flagEnrollAutoConnect = false

	var out bytes.Buffer
	enrollCmd.SetOut(&out)
	enrollCmd.SetErr(&out)
	enrollCmd.SetContext(context.Background())

	err := runEnroll(enrollCmd, []string{"srfb_enr_validlookingbutexpired"})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("got %v want 'expired'", err)
	}
}
