package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpClient is the minimal interface this package needs from net/http,
// extracted so tests can stub it.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// authorizeClient calls astro-server's /deployments/authorize endpoint.
// It carries the raw deploy token as a Bearer credential — the server
// validates the signature on each call.
type authorizeClient struct {
	httpClient httpClient
	serverURL  string // e.g. "http://astro-server:8080"
	token      string // raw JWT; opaque to this package
}

// authorizeResponse mirrors the server's JSON response: {"allowed": bool}.
type authorizeResponse struct {
	Allowed bool   `json:"allowed"`
	Error   string `json:"error,omitempty"`
}

// newAuthorizeClient constructs the HTTP client used to call astro-server.
// timeout caps each round-trip; on timeout the caller treats the request as
// denied (fail-closed) so a slow server doesn't accidentally widen access.
func newAuthorizeClient(serverURL, token string, timeout time.Duration) *authorizeClient {
	return &authorizeClient{
		httpClient: &http.Client{Timeout: timeout},
		serverURL:  strings.TrimRight(serverURL, "/"),
		token:      token,
	}
}

// authorize issues GET /api/v1/deployments/authorize and returns the boolean
// result from the server. Empty identityType/identityID are valid (anonymous)
// — the server's anyone short-circuit may still allow them.
func (c *authorizeClient) authorize(ctx context.Context, identityType, identityID, adapter string) (bool, error) {
	if c.serverURL == "" {
		return false, errors.New("authz: server URL not configured")
	}
	if c.token == "" {
		return false, errors.New("authz: identity token not configured")
	}

	u, err := url.Parse(c.serverURL + "/api/v1/deployments/authorize")
	if err != nil {
		return false, fmt.Errorf("authz: bad server URL: %w", err)
	}
	q := u.Query()
	if identityType != "" {
		q.Set("identity_type", identityType)
	}
	if identityID != "" {
		q.Set("identity_id", identityID)
	}
	q.Set("adapter", adapter)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("authz: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("authz: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("authz: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("authz: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out authorizeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("authz: parse response: %w", err)
	}
	if out.Error != "" {
		return false, fmt.Errorf("authz: server error: %s", out.Error)
	}
	return out.Allowed, nil
}
