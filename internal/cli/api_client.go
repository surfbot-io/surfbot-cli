package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// apiClient is a thin wrapper around net/http for the cli's REST calls into
// surfbot-api. It does NOT pin the cert here — the WS client owns pinning;
// REST goes through the system trust store (HTTPS, same hostname, same
// underlying TLS chain). If MITM hardening on REST becomes a concern we
// surface it via a shared transport.PinValidator.
type apiClient struct {
	baseURL string
	http    *http.Client
	ua      string
}

func newAPIClient(baseURL, version string) *apiClient {
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
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
