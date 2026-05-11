package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	baseURL     string
	http        *http.Client
	ua          string
	skipPinning bool
}

// newAPIClient builds the REST client. When baseURL is HTTPS, attaches a
// shared transport.PinValidator (skip-pinning honored, fail-open while pins
// are placeholders, strict once Andrew commits real hashes). HTTP scheme
// is rejected at request time on non-loopback hosts unless skipPinning is
// true — see CRITICAL #4 hotfix: a hostile token-issuance response that
// swapped `https://api...` for `http://attacker...` would otherwise leak
// the agent_token in cleartext on the very next call.
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
		baseURL:     strings.TrimRight(baseURL, "/"),
		http:        &http.Client{Timeout: 30 * time.Second, Transport: rt},
		ua:          fmt.Sprintf("surfbot-cli/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH),
		skipPinning: skipPinning,
	}
}

// ErrInsecureAPIBase mirrors transport.ErrInsecureScheme on the REST side.
// SPEC §6.1 + CRITICAL #4 hotfix: token-issuing endpoints MUST be HTTPS
// unless the operator pinned themselves into a dev cluster with
// --insecure-skip-pinning. Loopback hosts (localhost / 127.0.0.0/8 / ::1)
// are always allowed so tests + dev daemons keep working over http.
var ErrInsecureAPIBase = errors.New("api: refusing to call insecure baseURL")

// requireSecure runs before every request. We re-parse on every call rather
// than caching the decision at constructor time because logout falls back
// through inferAPIBaseURL → DefaultAPIBaseURL → cli-flag override, and a
// safe-by-default check at the request boundary is cheaper than threading
// a "validated at construction" flag through every call site.
func (c *apiClient) requireSecure() error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("api: parse baseURL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		// fall through to loopback / skip-pinning gating
	default:
		return fmt.Errorf("%w: unsupported scheme %q in %s", ErrInsecureAPIBase, u.Scheme, c.baseURL)
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	if c.skipPinning {
		return nil
	}
	return fmt.Errorf("%w: scheme %q on host %q (use --insecure-skip-pinning to override on dev clusters)", ErrInsecureAPIBase, u.Scheme, u.Hostname())
}

// isLoopbackHost recognizes localhost, 127.0.0.0/8, ::1 — duplicated here
// to keep the cli package free of a back-edge dep on internal/transport.
func isLoopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
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
	if err := c.requireSecure(); err != nil {
		return err
	}
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
	if err := c.requireSecure(); err != nil {
		return err
	}
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
