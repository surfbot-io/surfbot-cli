package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPIServer is a /cli/device/code + /cli/device/token tracker that lets a
// test drive a full device-code happy path against a real httptest server.
type fakeAPIServer struct {
	t            *testing.T
	pollCount    atomic.Int32
	approveAfter int32 // approve once pollCount > approveAfter
	deviceCode   string
	userCode     string
	finalToken   string
	wsURL        string
}

func (f *fakeAPIServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cli/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&struct{}{})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               f.deviceCode,
			"user_code":                 f.userCode,
			"verification_uri":          "https://app-test.example/cli/activate",
			"verification_uri_complete": "https://app-test.example/cli/activate?code=" + f.userCode,
			"interval":                  1,
			"expires_in":                60,
		})
	})
	mux.HandleFunc("POST /cli/device/token", func(w http.ResponseWriter, r *http.Request) {
		n := f.pollCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= f.approveAfter {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_token": f.finalToken,
			"agent_id":    "ag_test01",
			"org_id":      "org_xxx",
			"ws_url":      f.wsURL,
		})
	})
	return mux
}

func TestLogin_HappyPath_PrintsExpectedFormat(t *testing.T) {
	fake := &fakeAPIServer{
		t:            t,
		approveAfter: 1,
		deviceCode:   "dc_test1234",
		userCode:     "ABCD-1234",
		finalToken:   "srfb_agt_8a7b3c2d1e5f6789abcdef01",
		wsURL:        "ws://127.0.0.1:0/cli/ws", // unreachable; --connect=false skips
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// On macOS go's os.UserHomeDir falls back to HOME too; pin both.
	t.Setenv("HOME", dir)

	flagAPIBase = srv.URL
	flagLoginAutoConnect = false
	flagLoginPollDuration = 30 * time.Second
	flagInsecureSkipPin = false

	var out bytes.Buffer
	loginCmd.SetOut(&out)
	loginCmd.SetErr(&out)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loginCmd.SetContext(ctx)

	if err := runLogin(loginCmd, nil); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Visit:",
		"https://app-test.example/cli/activate",
		"And enter this code:",
		"ABCD-1234",
		"Waiting for activation...",
		"✓ Authorized as",
		"ag_test01",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q\n----\n%s", want, got)
		}
	}
}
