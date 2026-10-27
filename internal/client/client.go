// Package client wraps the MemSpace server HTTP API for client-side use.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/model"
)

// Client is an HTTP client for the MemSpace server.
type Client struct {
	ServerURL  string
	Token      string
	HTTPClient *http.Client
}

// New creates a Client. Token may be empty for unauthenticated calls.
func New(serverURL, token string) *Client {
	return &Client{
		ServerURL:  strings.TrimRight(serverURL, "/"),
		Token:      token,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: transport},
	}
}

// transport skips HTTP(S)_PROXY for servers on private networks. A typical
// self-hosted server is on the LAN or a tailnet, and desktop proxies (often
// set for the whole session, and inherited by agent hooks) usually cannot
// reach those addresses: requests would hang until the timeout.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = func(r *http.Request) (*url.URL, error) {
		if PrivateHost(r.URL.Hostname()) {
			return nil, nil
		}
		return http.ProxyFromEnvironment(r)
	}
	return t
}()

// cgnat is 100.64.0.0/10, used by Tailscale.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// PrivateHost reports whether host is a loopback, private, link-local or
// CGNAT address, or a single-label / .local / .lan / .internal / .home.arpa
// name.
func PrivateHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || !strings.Contains(h, ".") {
		return true
	}
	for _, suf := range []string{".local", ".lan", ".internal", ".home.arpa", ".localhost", ".ts.net"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// HTTPError is a non-2xx response from the server.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("server: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("server: HTTP %d", e.Status)
}

// Retryable reports whether a failed request may succeed later unchanged:
// network failures, timeouts, 5xx, 429 and auth problems (the user can fix
// a revoked token without losing spooled events).
func Retryable(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return true // transport-level failure
	}
	switch {
	case he.Status >= 500, he.Status == http.StatusTooManyRequests,
		he.Status == http.StatusRequestTimeout,
		he.Status == http.StatusUnauthorized, he.Status == http.StatusForbidden:
		return true
	}
	return false
}

// IsStatus reports whether err is an HTTPError with the given status.
func IsStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}

func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("client: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.ServerURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("client: new request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("client: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		he := &HTTPError{Status: resp.StatusCode}
		var errResp map[string]string
		if json.Unmarshal(body, &errResp) == nil {
			he.Message = errResp["error"]
		}
		return he
	}
	if respBody != nil {
		if err := json.Unmarshal(body, respBody); err != nil {
			return fmt.Errorf("client: unmarshal response: %w", err)
		}
	}
	return nil
}

// Do sends a JSON request to path and decodes the JSON response.
func (c *Client) Do(ctx context.Context, method, path string, reqBody, respBody any) error {
	return c.do(ctx, method, path, reqBody, respBody)
}

// Register calls POST /api/v1/devices with a one-time invite and returns the
// device id and token.
func (c *Client) Register(ctx context.Context, name, platform, invite string) (deviceID, token string, err error) {
	var resp model.RegisterDeviceResponse
	req := model.RegisterDeviceRequest{Name: name, Platform: platform, InviteCode: invite}
	if err := c.do(ctx, "POST", "/api/v1/devices", req, &resp); err != nil {
		return "", "", err
	}
	return resp.DeviceID, resp.Token, nil
}

// EnsureProject idempotently resolves a project by Git identity or name.
func (c *Client) EnsureProject(ctx context.Context, name, gitIdentity string) (string, error) {
	req := model.EnsureProjectRequest{Name: name}
	if gitIdentity != "" {
		req.CanonicalGitIdentity = &gitIdentity
	}
	var resp model.EnsureProjectResponse
	if err := c.do(ctx, "POST", "/api/v1/projects", req, &resp); err != nil {
		return "", err
	}
	return resp.ProjectID, nil
}

// EnsureSession idempotently resolves a session.
func (c *Client) EnsureSession(ctx context.Context, req model.EnsureSessionRequest) (*model.EnsureSessionResponse, error) {
	var resp model.EnsureSessionResponse
	if err := c.do(ctx, "POST", "/api/v1/sessions", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BatchEvents uploads a batch of events for one project.
func (c *Client) BatchEvents(ctx context.Context, projectID string, events []model.Event) (*model.BatchEventsResponse, error) {
	var resp model.BatchEventsResponse
	if err := c.do(ctx, "POST", "/api/v1/events/batch", model.BatchEventsRequest{ProjectID: projectID, Events: events}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Search runs a full-text search.
func (c *Client) Search(ctx context.Context, req model.SearchRequest) (*model.SearchResponse, error) {
	var resp model.SearchResponse
	if err := c.do(ctx, "POST", "/api/v1/search", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RotateMCPToken issues a new read-only MCP token for this device.
func (c *Client) RotateMCPToken(ctx context.Context) (string, error) {
	var resp struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, "POST", "/api/v1/devices/mcp-token", nil, &resp); err != nil {
		return "", err
	}
	return resp.Token, nil
}

// EmbeddingStatus describes the server's semantic index.
type EmbeddingStatus struct {
	Enabled bool `json:"enabled"`
	Active  *struct {
		Model   string `json:"model"`
		Docs    int    `json:"docs_embedded"`
		Pending int    `json:"docs_pending"`
	} `json:"active_space"`
	Building *struct {
		Model   string `json:"model"`
		Docs    int    `json:"docs_embedded"`
		Pending int    `json:"docs_pending"`
	} `json:"building_space"`
	LastError string `json:"last_error"`
}

// EmbeddingStatus returns the state of semantic search on the server.
func (c *Client) EmbeddingStatus(ctx context.Context) (*EmbeddingStatus, error) {
	var resp EmbeddingStatus
	if err := c.do(ctx, "GET", "/api/v1/embedding/status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SyncStatus returns the server's view of this device.
func (c *Client) SyncStatus(ctx context.Context) (*model.SyncStatusResponse, error) {
	var resp model.SyncStatusResponse
	if err := c.do(ctx, "GET", "/api/v1/sync/status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReportSync sends the local spool state (POST /api/v1/sync/report).
func (c *Client) ReportSync(ctx context.Context, rep model.SyncReport) error {
	return c.do(ctx, "POST", "/api/v1/sync/report", rep, nil)
}

// Health calls GET /api/v1/health (no auth required).
func (c *Client) Health(ctx context.Context) (string, error) {
	var resp struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, "GET", "/api/v1/health", nil, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}
