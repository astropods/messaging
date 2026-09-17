package peers

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

	"github.com/astropods/messaging/internal/a2awire"
	"github.com/google/uuid"
)

// DefaultAskTimeout bounds one call to a peer. Deliberately far shorter than
// the callee's own 5-minute send budget: the caller is usually a model waiting
// on a tool result, and a tool that blocks for minutes is worse than one that
// reports the task is still running.
const DefaultAskTimeout = 60 * time.Second

// Client calls other agents over A2A. It speaks the protocol so the agent
// never has to: an agent asks for text and gets text back.
type Client struct {
	httpClient httpClient
	timeout    time.Duration
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: DefaultAskTimeout + 5*time.Second},
		timeout:    DefaultAskTimeout,
	}
}

// Reply is a peer's answer to one Ask.
type Reply struct {
	// Text is the peer's answer, joined from the task's text artifacts.
	Text string
	// TaskID identifies the peer's task, for a follow-up poll when Done is false.
	TaskID string
	// ContextID threads a follow-up Ask onto the same conversation.
	ContextID string
	// State is the A2A task state the peer reported.
	State string
	// Done is false when the peer is still working, which means Text may be
	// partial or empty and the caller can poll TaskID.
	Done bool
}

// Ask sends one message to a peer and returns its reply.
//
// contextID threads onto an existing conversation with that peer; empty starts
// a new one, and the returned ContextID carries it for the next turn.
func (c *Client) Ask(ctx context.Context, peerURL, contextID, text string) (Reply, error) {
	if strings.TrimSpace(peerURL) == "" {
		return Reply{}, errors.New("peers: peer URL is required")
	}
	if strings.TrimSpace(text) == "" {
		return Reply{}, errors.New("peers: message text is required")
	}

	params := a2awire.SendParams{Message: a2awire.Message{
		Role:      "user",
		MessageID: uuid.NewString(),
		ContextID: contextID,
		Parts:     []a2awire.Part{{Kind: a2awire.PartKindText, Text: text}},
	}}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return Reply{}, fmt.Errorf("peers: encode params: %w", err)
	}
	rawID, err := json.Marshal(uuid.NewString())
	if err != nil {
		return Reply{}, fmt.Errorf("peers: encode request id: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var task a2awire.Task
	if err := c.rpc(callCtx, peerURL, a2awire.Request{
		JSONRPC: a2awire.JSONRPCVersion,
		ID:      rawID,
		Method:  "message/send",
		Params:  rawParams,
	}, &task); err != nil {
		return Reply{}, err
	}

	reply := Reply{
		Text:      artifactText(task),
		TaskID:    task.ID,
		ContextID: task.ContextID,
		State:     task.Status.State,
		Done:      task.Status.State == a2awire.StateCompleted,
	}
	if task.Status.State == a2awire.StateFailed {
		reason := task.Status.Message
		if reason == "" {
			reason = "peer reported no reason"
		}
		return reply, fmt.Errorf("peers: peer failed the task: %s", reason)
	}
	return reply, nil
}

// Card fetches a peer's agent card, which carries its description and skills.
// Discovery hands back URLs only, so this is how a caller learns what a peer
// can do.
func (c *Client) Card(ctx context.Context, peerURL string) (a2awire.Card, error) {
	cardCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cardCtx, http.MethodGet,
		strings.TrimRight(peerURL, "/")+"/.well-known/agent-card.json", nil)
	if err != nil {
		return a2awire.Card{}, fmt.Errorf("peers: build card request: %w", err)
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return a2awire.Card{}, fmt.Errorf("peers: fetch card: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode != http.StatusOK {
		return a2awire.Card{}, fmt.Errorf("peers: card request returned %d", res.StatusCode)
	}
	var card a2awire.Card
	if err := json.NewDecoder(res.Body).Decode(&card); err != nil {
		return a2awire.Card{}, fmt.Errorf("peers: decode card: %w", err)
	}
	return card, nil
}

// rpc posts one JSON-RPC request to a peer and decodes result into out.
func (c *Client) rpc(ctx context.Context, peerURL string, body a2awire.Request, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("peers: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(peerURL, "/")+"/", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("peers: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("peers: call peer: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return fmt.Errorf("peers: peer returned HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var envelope a2awire.Response
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("peers: decode reply: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("peers: peer rejected the call (%d): %s", envelope.Error.Code, envelope.Error.Message)
	}
	resultRaw, err := json.Marshal(envelope.Result)
	if err != nil {
		return fmt.Errorf("peers: re-encode result: %w", err)
	}
	if err := json.Unmarshal(resultRaw, out); err != nil {
		return fmt.Errorf("peers: decode result: %w", err)
	}
	return nil
}

// artifactText joins every text part of a task's artifacts. A2A allows a reply
// to be split across parts and artifacts, so reading only the first truncates.
func artifactText(task a2awire.Task) string {
	var b strings.Builder
	for _, artifact := range task.Artifacts {
		for _, part := range artifact.Parts {
			if part.Kind == a2awire.PartKindText {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}
