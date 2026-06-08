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

// authorizeResponse mirrors the server's JSON response. UserID (the
// resolved WorkOS user_id) is set on allowed=true; the server also
// echoes slack_user_id / slack_team_id on slack requests, but the
// adapter doesn't need them — it falls back to the raw slack id from the
// incoming event when UserID is empty — so we ignore those fields here.
// Re-add them if a consumer ever needs the directory lookup result
// without an extra round-trip.
type authorizeResponse struct {
	Allowed bool   `json:"allowed"`
	UserID  string `json:"user_id,omitempty"`
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

// authorize issues GET /api/v1/deployments/authorize and returns the parsed
// Result. Empty identityType/identityID are valid (anonymous) — the server's
// anyone short-circuit may still allow them.
//
// identityScope is sent as identity_scope when non-empty; the server uses
// it to disambiguate identity_id values that aren't globally unique
// (slack user_id needs team_id). Empty scope is the unscoped behavior.
func (c *authorizeClient) authorize(ctx context.Context, identityType, identityID, adapter, identityScope string, slackProfile *SlackUserProfile) (Result, error) {
	if c.serverURL == "" {
		return Result{}, errors.New("authz: server URL not configured")
	}
	if c.token == "" {
		return Result{}, errors.New("authz: identity token not configured")
	}

	u, err := url.Parse(c.serverURL + "/api/v1/deployments/authorize")
	if err != nil {
		return Result{}, fmt.Errorf("authz: bad server URL: %w", err)
	}
	q := u.Query()
	if identityType != "" {
		q.Set("identity_type", identityType)
	}
	if identityID != "" {
		q.Set("identity_id", identityID)
	}
	if identityScope != "" {
		q.Set("identity_scope", identityScope)
	}
	if slackProfile != nil && slackProfile.Present {
		// The authorize endpoint is currently GET-based, so Slack profile fields
		// appear in the query string. Treat astro-server access logs for this
		// route as containing Slack user PII.
		q.Set("slack_display_name", slackProfile.DisplayName)
		q.Set("slack_username", slackProfile.Username)
		q.Set("slack_avatar_url", slackProfile.AvatarURL)
		q.Set("slack_is_bot", fmt.Sprint(slackProfile.IsBot))
		q.Set("slack_deleted", fmt.Sprint(slackProfile.Deleted))
	}
	q.Set("adapter", adapter)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("authz: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("authz: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("authz: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("authz: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out authorizeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Result{}, fmt.Errorf("authz: parse response: %w", err)
	}
	if out.Error != "" {
		return Result{}, fmt.Errorf("authz: server error: %s", out.Error)
	}
	return Result{
		Allowed: out.Allowed,
		UserID:  out.UserID,
	}, nil
}
