// Package langfuse is a minimal read-only client the messaging sidecar uses to
// rebuild chat history from Langfuse traces after the local SQLite database is
// lost (pod reschedule). Langfuse is the durable source of truth: agents emit
// OTEL spans keyed by session_id = conversation_id and user_id = WorkOS user id,
// tagged deployment:<id>. This client reads those back; it never writes.
package langfuse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// metaTraceName tags the lightweight trace the sidecar writes to persist
// conversation metadata (e.g. user-set titles) that has no home in normal
// turn traces. It survives redeploys because it lives in Langfuse, not the
// ephemeral SQLite store. It is excluded from reconstructed message threads.
const metaTraceName = "astro.chat.meta"

// metaTraceTag marks the metadata trace so it can be distinguished/filtered.
const metaTraceTag = "astro-chat-meta"

// metaTraceID is the deterministic Langfuse trace id for a conversation's
// metadata. Re-ingesting with the same id updates the title in place.
func metaTraceID(conversationID string) string {
	return "astro-chat-meta-" + conversationID
}

// ChatMessage is a reconstructed chat turn (role + content).
type ChatMessage struct {
	Role    string
	Content string
}

// Session is a reconstructed conversation summary.
type Session struct {
	ConversationID string
	Title          string
	UpdatedAt      time.Time
}

// Client reads traces from the Langfuse public API using a pre-encoded basic
// auth token (base64 of "publicKey:secretKey").
type Client struct {
	baseURL      string
	authToken    string
	deploymentID string
	http         *http.Client
}

// NewFromEnv builds a client from the deploy-time environment, or returns nil
// when Langfuse is not configured (e.g. local dev) so callers can skip restore.
func NewFromEnv() *Client {
	baseURL := strings.TrimSuffix(strings.TrimSpace(os.Getenv("LANGFUSE_BASE_URL")), "/")
	token := strings.TrimSpace(os.Getenv("LANGFUSE_AUTH_TOKEN"))
	deploymentID := strings.TrimSpace(os.Getenv("ASTRO_AGENT_ID"))
	if baseURL == "" || token == "" || deploymentID == "" {
		return nil
	}
	return &Client{
		baseURL:      baseURL,
		authToken:    token,
		deploymentID: deploymentID,
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

type trace struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     any            `json:"input"`
	Output    any            `json:"output"`
	SessionID string         `json:"sessionId"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt string         `json:"createdAt"`
}

// titleFromMeta extracts the persisted title from a metadata trace, if present.
func (t trace) titleFromMeta() string {
	if t.Name != metaTraceName {
		return ""
	}
	if s, ok := t.Metadata["title"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

type tracesResponse struct {
	Data []trace `json:"data"`
}

// sessionTraceLimit bounds the per-call trace fetch when reconstructing.
const sessionTraceLimit = 500

// GetSessionMessages returns the reconstructed thread for one conversation,
// ordered oldest-first, plus the persisted title (from the metadata trace, if
// any). The deployment tag plus the user filter ensure a caller only ever reads
// their own conversation.
func (c *Client) GetSessionMessages(ctx context.Context, userID, conversationID string) ([]ChatMessage, string, error) {
	params := url.Values{}
	params.Set("tags", "deployment:"+c.deploymentID)
	params.Set("userId", userID)
	params.Set("sessionId", conversationID)
	params.Set("orderBy", "timestamp.asc")
	params.Set("limit", fmt.Sprintf("%d", sessionTraceLimit))

	var resp tracesResponse
	if err := c.doGet(ctx, "/api/public/traces", params, &resp); err != nil {
		return nil, "", err
	}
	sort.SliceStable(resp.Data, func(i, j int) bool {
		return resp.Data[i].CreatedAt < resp.Data[j].CreatedAt
	})

	title := ""
	turns := make([]trace, 0, len(resp.Data))
	for _, t := range resp.Data {
		if t.Name == metaTraceName {
			if mt := t.titleFromMeta(); mt != "" {
				title = mt
			}
			continue
		}
		turns = append(turns, t)
	}
	return tracesToMessages(turns), title, nil
}

// ListUserSessions returns the user's conversations for this deployment,
// grouped from traces by session id, most-recent first. The title is the
// persisted (user-set) title when present, otherwise the first user message.
func (c *Client) ListUserSessions(ctx context.Context, userID string) ([]Session, error) {
	params := url.Values{}
	params.Set("tags", "deployment:"+c.deploymentID)
	params.Set("userId", userID)
	params.Set("orderBy", "timestamp.asc")
	params.Set("limit", fmt.Sprintf("%d", sessionTraceLimit))

	var resp tracesResponse
	if err := c.doGet(ctx, "/api/public/traces", params, &resp); err != nil {
		return nil, err
	}

	type agg struct {
		firstUser string
		metaTitle string
		updatedAt time.Time
	}
	bySession := map[string]*agg{}
	for _, t := range resp.Data {
		if t.SessionID == "" {
			continue
		}
		a, ok := bySession[t.SessionID]
		if !ok {
			a = &agg{}
			bySession[t.SessionID] = a
		}
		if t.Name == metaTraceName {
			if mt := t.titleFromMeta(); mt != "" {
				a.metaTitle = mt
			}
		} else if a.firstUser == "" {
			if title := contentText(t.Input); title != "" {
				a.firstUser = truncate(title, 80)
			}
		}
		if ts, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil && ts.After(a.updatedAt) {
			a.updatedAt = ts
		}
	}

	out := make([]Session, 0, len(bySession))
	for id, a := range bySession {
		title := a.metaTitle
		if title == "" {
			title = a.firstUser
		}
		out = append(out, Session{ConversationID: id, Title: title, UpdatedAt: a.updatedAt})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// UpsertSessionTitle persists a conversation title to Langfuse as a lightweight
// metadata trace keyed by session id. This keeps user-set titles durable across
// pod reschedules (which wipe the sidecar's local store) without writing chat
// data to astro-server. Re-ingesting the same deterministic trace id updates the
// title in place.
func (c *Client) UpsertSessionTitle(ctx context.Context, userID, conversationID, title string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload := ingestionRequest{Batch: []ingestionEvent{{
		ID:        uuid.NewString(),
		Type:      "trace-create",
		Timestamp: now,
		Body: ingestionTraceBody{
			ID:        metaTraceID(conversationID),
			Timestamp: now,
			Name:      metaTraceName,
			SessionID: conversationID,
			UserID:    userID,
			Tags:      []string{"deployment:" + c.deploymentID, metaTraceTag},
			Metadata:  map[string]any{"title": title},
		},
	}}}
	return c.doPost(ctx, "/api/public/ingestion", payload)
}

// ingestionRequest is the batch envelope for POST /api/public/ingestion.
type ingestionRequest struct {
	Batch []ingestionEvent `json:"batch"`
}

type ingestionEvent struct {
	ID        string             `json:"id"`
	Type      string             `json:"type"`
	Timestamp string             `json:"timestamp"`
	Body      ingestionTraceBody `json:"body"`
}

type ingestionTraceBody struct {
	ID        string         `json:"id"`
	Timestamp string         `json:"timestamp"`
	Name      string         `json:"name"`
	SessionID string         `json:"sessionId"`
	UserID    string         `json:"userId"`
	Tags      []string       `json:"tags"`
	Metadata  map[string]any `json:"metadata"`
}

func (c *Client) doPost(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("langfuse: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("langfuse: create request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+c.authToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("langfuse: request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Ingestion returns 207 Multi-Status on success (per-event results).
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("langfuse: unexpected status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) doGet(ctx context.Context, path string, params url.Values, out any) error {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("langfuse: create request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+c.authToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("langfuse: request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("langfuse: unexpected status %d: %s", resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("langfuse: decode response: %w", err)
	}
	return nil
}

func tracesToMessages(traces []trace) []ChatMessage {
	messages := make([]ChatMessage, 0, len(traces)*2)
	for _, t := range traces {
		if userText := contentText(t.Input); userText != "" {
			messages = append(messages, ChatMessage{Role: "user", Content: userText})
		}
		if assistantText := contentText(t.Output); assistantText != "" {
			messages = append(messages, ChatMessage{Role: "assistant", Content: assistantText})
		}
	}
	return messages
}

// contentText best-effort extracts a display string from a Langfuse trace
// input/output value, which may be a plain string, a message object, or a list
// of message objects depending on the agent framework.
func contentText(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(val)
	case map[string]any:
		for _, key := range []string{"content", "text", "message", "output", "value", "response"} {
			if s, ok := val[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		if msgs, ok := val["messages"].([]any); ok {
			if s := lastMessageText(msgs); s != "" {
				return s
			}
		}
		return jsonFallback(val)
	case []any:
		if s := lastMessageText(val); s != "" {
			return s
		}
		return jsonFallback(val)
	default:
		return jsonFallback(val)
	}
}

func lastMessageText(items []any) string {
	for i := len(items) - 1; i >= 0; i-- {
		switch item := items[i].(type) {
		case string:
			if s := strings.TrimSpace(item); s != "" {
				return s
			}
		case map[string]any:
			for _, key := range []string{"content", "text"} {
				if s, ok := item[key].(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

func jsonFallback(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "{}" || s == "[]" {
		return ""
	}
	return s
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
