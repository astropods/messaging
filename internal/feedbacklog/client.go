package feedbacklog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/authz"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

const defaultTimeout = 5 * time.Second

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Client struct {
	httpClient httpClient
	serverURL  string
	token      string
}

type request struct {
	Source         string        `json:"source"`
	ConversationID string        `json:"conversation_id,omitempty"`
	ResponseID     string        `json:"response_id,omitempty"`
	TraceContext   traceContext  `json:"trace_context"`
	Feedback       feedbackValue `json:"feedback"`
	User           user          `json:"user,omitempty"`
	Timestamp      string        `json:"timestamp,omitempty"`
}

type traceContext struct {
	Traceparent string `json:"traceparent,omitempty"`
	Tracestate  string `json:"tracestate,omitempty"`
}

type feedbackValue struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

type user struct {
	ID       string `json:"id,omitempty"`
	Username string `json:"username,omitempty"`
}

func NewFromToken(token string) (*Client, error) {
	claims, err := authz.DecodeToken(token)
	if err != nil {
		return nil, fmt.Errorf("feedback log: decode token: %w", err)
	}
	if strings.TrimSpace(claims.Issuer) == "" {
		return nil, errors.New("feedback log: token missing iss claim")
	}
	return &Client{
		httpClient: &http.Client{Timeout: defaultTimeout},
		serverURL:  strings.TrimRight(claims.Issuer, "/"),
		token:      token,
	}, nil
}

func (c *Client) Record(ctx context.Context, fb *pb.PlatformFeedback) error {
	if fb == nil {
		return errors.New("feedback log: nil feedback")
	}
	reqBody, ok := buildRequest(fb)
	if !ok {
		return nil
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("feedback log: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL+"/api/v1/deployments/feedback/scores", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("feedback log: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("feedback log: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("feedback log: read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("feedback log: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func buildRequest(fb *pb.PlatformFeedback) (request, bool) {
	trace := fb.GetTraceContext()
	if trace == nil || trace.Traceparent == "" {
		return request{}, false
	}

	// The server requires these to attribute and dedupe the score, so drop
	// feedback that can't be identified rather than forwarding a doomed 400.
	if fb.GetResponseId() == "" || fb.GetUser().GetId() == "" {
		return request{}, false
	}

	feedback, ok := feedbackFromProto(fb)
	if !ok {
		return request{}, false
	}

	req := request{
		Source:         "slack",
		ConversationID: fb.GetConversationId(),
		ResponseID:     fb.GetResponseId(),
		TraceContext: traceContext{
			Traceparent: trace.Traceparent,
			Tracestate:  trace.Tracestate,
		},
		Feedback: feedback,
		User:     user{ID: fb.GetUser().GetId(), Username: fb.GetUser().GetUsername()},
	}
	if ts := fb.GetTimestamp(); ts != nil {
		req.Timestamp = ts.AsTime().UTC().Format(time.RFC3339Nano)
	}
	return req, true
}

func feedbackFromProto(fb *pb.PlatformFeedback) (feedbackValue, bool) {
	if reaction := fb.GetReaction(); reaction != nil {
		switch reaction.Type {
		case pb.MessageReaction_THUMBS_UP:
			return feedbackValue{Kind: "thumbs_up"}, true
		case pb.MessageReaction_THUMBS_DOWN:
			return feedbackValue{Kind: "thumbs_down"}, true
		default:
			return feedbackValue{}, false
		}
	}
	if text := fb.GetText(); text != nil {
		body := strings.TrimSpace(text.Text)
		if body == "" {
			return feedbackValue{}, false
		}
		return feedbackValue{Kind: "comment", Text: body}, true
	}
	return feedbackValue{}, false
}
