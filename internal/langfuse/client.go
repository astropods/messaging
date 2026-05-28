// Package langfuse provides a minimal client for submitting observability
// scores to Langfuse from the messaging server.
//
// We deliberately keep this package small and dependency-free rather than
// reusing the larger Langfuse client in astro-server: messaging only needs
// score-write today, and a tight surface area means fewer reasons for the
// two services to coupling-drift.
//
// All operations are silent no-ops when the client was constructed without
// credentials — adapters can call CreateScore unconditionally and get
// graceful degradation in deployments where Langfuse isn't configured.
package langfuse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client submits scores to Langfuse's public scores API.
//
// A zero-value or nil-receiver Client is a valid no-op — methods on a
// disabled client return nil immediately so callers don't need a separate
// "is configured" check.
type Client struct {
	baseURL    string
	publicKey  string
	secretKey  string
	httpClient *http.Client
}

// ScoreRequest is the JSON body for POST /api/public/scores.
//
// ID is an idempotency key — Langfuse upserts on (traceId, name, id), so
// resubmitting the same {trace, name, id} triple is safe (a double-click
// on a feedback button doesn't create two scores).
//
// Value is required. For numeric scores (THUMBS_UP=1, THUMBS_DOWN=0) pass
// a float. For comment-style feedback the spec uses value=0.5 (neutral)
// and the Comment field carries the text.
type ScoreRequest struct {
	ID      string  `json:"id,omitempty"`
	TraceID string  `json:"traceId"`
	Name    string  `json:"name"`
	Value   float64 `json:"value"`
	Comment string  `json:"comment,omitempty"`
}

// New builds a Client. Pass empty strings to construct a disabled client
// (the messaging server uses this when LANGFUSE_* env vars are unset).
func New(baseURL, publicKey, secretKey string) *Client {
	return &Client{
		baseURL:   baseURL,
		publicKey: publicKey,
		secretKey: secretKey,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Enabled reports whether the client has credentials and will actually
// hit the network. Callers can branch on this for logging, but the public
// CreateScore method already handles the disabled case silently.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != "" && c.publicKey != "" && c.secretKey != ""
}

// CreateScore submits a single score. Silent no-op when the client is
// disabled. Returns nil on 2xx; otherwise an error suitable for logging
// (callers should NOT surface this to end users — score submission is a
// best-effort observability signal).
func (c *Client) CreateScore(ctx context.Context, req ScoreRequest) error {
	if !c.Enabled() {
		return nil
	}
	if req.TraceID == "" {
		return fmt.Errorf("langfuse: trace_id required")
	}
	if req.Name == "" {
		return fmt.Errorf("langfuse: name required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("langfuse: marshal request: %w", err)
	}

	url := c.baseURL + "/api/public/scores"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("langfuse: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Basic "+basicAuth(c.publicKey, c.secretKey))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("langfuse: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Debug("[langfuse] Score submitted",
			"trace_id", req.TraceID,
			"name", req.Name,
			"value", req.Value,
			"status", resp.StatusCode,
		)
		return nil
	}

	// Read up to 1KB of the error body for context — Langfuse returns JSON
	// like {"error":"...","message":"..."} that's useful in logs.
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("langfuse: score POST returned %d: %s", resp.StatusCode, string(excerpt))
}

// basicAuth builds the "Basic base64(public:secret)" header value.
func basicAuth(public, secret string) string {
	creds := public + ":" + secret
	return base64.StdEncoding.EncodeToString([]byte(creds))
}
