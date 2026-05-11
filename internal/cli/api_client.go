package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/surfbot-io/surfbot-cli/internal/transport"
)

// apiClient is a thin wrapper around net/http for the cli's REST calls into
// surfbot-api. It pins the cert via transport.PinValidator (same fail-open
// canary as the WS client) so the token-issuing endpoints (/cli/device/token,
// /cli/enroll) get the same MITM protection as the post-issuance WS channel.
type apiClient struct {
	baseURL string
	http    *http.Client
	ua      string
}

// newAPIClient builds the REST client. When baseURL is HTTPS, attaches a
// shared transport.PinValidator (skip-pinning honored, fail-open while pins
// are placeholders, strict once Andrew commits real hashes). HTTP scheme
// (local dev / httptest) bypasses pinning entirely.
func newAPIClient(baseURL, version string, skipPinning bool) *apiClient {
	rt := http.DefaultTransport
	if u, err := url.Parse(baseURL); err == nil && u.Scheme == "https" {
		validator := transport.NewPinValidator(u.Hostname(), skipPinning, true)
		rt = &http.Transport{
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig: &tls.Config{
				MinVersion:            tls.VersionTLS13,
				VerifyPeerCertificate: validator.VerifyPeerCertificate,
			},
			ForceAttemptHTTP2: true,
		}
	}
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second, Transport: rt},
		ua:      fmt.Sprintf("surfbot-cli/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH),
	}
}

// apiError surfaces non-2xx responses so the caller can dispatch on the
// response body's `error` field per RFC 8628 § 3.5 conventions. Status
// carries the HTTP code; Code is the body.error string (e.g.
// "authorization_pending"); Body is the full payload for logging.
type apiError struct {
	Status int
	Code   string
	Body   string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("api: %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("api: %d", e.Status)
}

// postJSON issues a POST with a JSON body and decodes the JSON response
// into out. On non-2xx the decoded body's `error` field is wrapped in an
// apiError and returned (caller errors.As checks for it).
func (c *apiClient) postJSON(ctx context.Context, path string, in, out any) error {
	u, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return fmt.Errorf("join url: %w", err)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		return nil
	}
	// Try to parse {error: "..."} per RFC 8628 §3.5; tolerate any shape.
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &errBody)
	return &apiError{Status: resp.StatusCode, Code: errBody.Error, Body: string(raw)}
}

// postBearer is postJSON with an Authorization: Bearer header. Used by
// /cli/logout (when it lands) and /cli/whoami fallbacks.
func (c *apiClient) postBearer(ctx context.Context, path, token string, in, out any) error {
	u, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return fmt.Errorf("join url: %w", err)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		return nil
	}
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &errBody)
	return &apiError{Status: resp.StatusCode, Code: errBody.Error, Body: string(raw)}
}

// asAPIError extracts the apiError if err wraps one; returns nil otherwise.
func asAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// inferAPIBaseURL turns a wss://api.surfbot.io/cli/ws URL (or an https
// equivalent) into the REST base, "https://api.surfbot.io". Used by status
// + logout once the cli has an enrolled ws_url it can pivot from.
func inferAPIBaseURL(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}
