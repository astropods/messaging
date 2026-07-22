package grpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/metrics"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// AgentInvoker is the seam between the invoke-per-turn transport and *how* a turn
// physically reaches the agent runtime. Given a session id and a JSON payload it
// returns the runtime's raw SSE byte stream (the /invocations wire, §3), which the
// transport scans identically regardless of backend.
//
//   - httpInvoker   — plain unsigned POST {endpoint}/invocations. Local dev; the
//     agent container is reachable directly (no AWS creds). This is the original
//     behavior, unchanged.
//   - sigv4Invoker  — aws-sdk-go-v2 bedrockagentcore.InvokeAgentRuntime against a
//     runtime ARN, SigV4-signed. The real AWS path.
//
// Selection is by ASTRO_DEPLOY_TARGET (see config + main.go): "aws" ⇒ sigv4,
// anything else ⇒ http. The caller never guesses an ARN — deploy tooling
// (wrapper deploy) creates the runtime and injects AGENT_RUNTIME_ARN.
type AgentInvoker interface {
	// Invoke sends one turn and returns the runtime's SSE stream. The caller
	// closes the returned ReadCloser.
	Invoke(ctx context.Context, sessionID string, payload []byte) (io.ReadCloser, error)
	// Describe returns a short human label for logs (e.g. "http:http://localhost:8080").
	Describe() string
}

// AgentCoreTransport implements the invoke-per-turn model: instead of holding a
// bidirectional gRPC stream to a dialed-in agent, it invokes the AgentCore Runtime
// once per turn (via an AgentInvoker), parses the SSE reply, and routes each chunk
// back through the SAME routeAgentResponse path the gRPC server uses. The
// client-facing wire (Slack/web SSE) is therefore unchanged.
//
// This is the counterpart of adapter-core's AgentCoreServer. The two share the
// /invocations SSE contract (specs/P1-wrapper.md §3):
//
//	{"type":"START"} | {"type":"DELTA","content":...} |
//	{"type":"ELICIT",...} | {"type":"ERROR","code":...,"message":...} | {"type":"END"}
//
// It is selected only when AGENT_TRANSPORT=agentcore; the default gRPC dial-in
// path is untouched.
type AgentCoreTransport struct {
	server  *Server
	invoker AgentInvoker
}

// httpInvoker is the local, unsigned backend: POST {endpoint}/invocations.
type httpInvoker struct {
	endpoint string
	client   *http.Client
}

func (h *httpInvoker) Describe() string { return "http:" + h.endpoint }

func (h *httpInvoker) Invoke(ctx context.Context, sessionID string, payload []byte) (io.ReadCloser, error) {
	url := h.endpoint + "/invocations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build invocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invoke runtime: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("invoke runtime: status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// invocationEvent is one decoded SSE `data:` line on the /invocations wire.
type invocationEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// ELICIT fields (routed for logging in v1; full elicit round-trip is P2).
	RenderableID string `json:"renderableId,omitempty"`
}

// invocationRequest is the JSON body POSTed to /invocations.
type invocationRequest struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"sessionId"`
}

// NewAgentCoreTransport builds the invoke-per-turn transport around an invoker.
// The Server is reused for its routeAgentResponse/cache wiring.
func NewAgentCoreTransport(server *Server, invoker AgentInvoker) *AgentCoreTransport {
	return &AgentCoreTransport{server: server, invoker: invoker}
}

// NewHTTPInvoker builds the local, unsigned backend against a runtime base URL
// (POST {endpoint}/invocations). Used when ASTRO_DEPLOY_TARGET != "aws".
func NewHTTPInvoker(endpoint string) AgentInvoker {
	return &httpInvoker{
		endpoint: strings.TrimRight(endpoint, "/"),
		// A turn may stream for a while; align with the AgentCore sync limit.
		client: &http.Client{Timeout: 15 * time.Minute},
	}
}

// HandleIncomingMessage has the adapter.MessageHandler signature, so it drops in
// where grpcServer.HandleIncomingMessage would be wired. It invokes the runtime
// and streams the reply back to the originating platform.
func (t *AgentCoreTransport) HandleIncomingMessage(ctx context.Context, msg *pb.Message) error {
	logIncomingMessage(msg)
	metrics.MessagesReceived.WithLabelValues(msg.Platform).Inc()
	start := time.Now()

	// Keep conversation-cache behavior identical to the gRPC path so response
	// routing (which reads the cache for the platform) works the same.
	if err := t.server.updateConversationCache(ctx, msg); err != nil {
		slog.Warn("[agentcore] Failed to update conversation cache", "err", err)
	}

	// runtimeSessionId = conversationID: AgentCore keeps the same warm microVM
	// for later turns of this conversation and idle-reaps it after. The invoker
	// derives a contract-valid session id (AgentCore requires >=33 chars).
	body, err := json.Marshal(invocationRequest{
		Prompt:    msg.Content,
		SessionID: msg.ConversationId,
	})
	if err != nil {
		return fmt.Errorf("marshal invocation: %w", err)
	}

	stream, err := t.invoker.Invoke(ctx, msg.ConversationId, body)
	if err != nil {
		metrics.MessagesDropped.WithLabelValues(msg.Platform, "invoke_error").Inc()
		return fmt.Errorf("invoke runtime: %w", err)
	}
	defer stream.Close()

	metrics.MessagesForwarded.WithLabelValues(msg.Platform).Inc()
	if err := t.streamSSE(ctx, stream, msg); err != nil {
		return err
	}

	metrics.MessageLatency.WithLabelValues(msg.Platform).Observe(time.Since(start).Seconds())
	slog.Debug("[agentcore] Turn complete", "conversation", msg.ConversationId)
	return nil
}

// streamSSE reads `data:` lines, maps each event to an AgentResponse, and routes
// it back to the platform exactly as the gRPC content path does.
func (t *AgentCoreTransport) streamSSE(ctx context.Context, r io.Reader, msg *pb.Message) error {
	scanner := bufio.NewScanner(r)
	// Allow long DELTA lines (default 64KiB is too small for big token bursts).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev invocationEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			slog.Warn("[agentcore] Skipping malformed SSE event", "err", err, "raw", payload)
			continue
		}

		resp := t.toAgentResponse(msg, ev)
		if resp == nil {
			continue
		}
		if err := t.server.routeAgentResponse(ctx, resp); err != nil {
			slog.Error("[agentcore] Failed to route response", "err", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}
	return nil
}

// toAgentResponse maps one /invocations SSE event to the internal AgentResponse
// the platform adapters already understand (START/DELTA/END → ContentChunk).
func (t *AgentCoreTransport) toAgentResponse(msg *pb.Message, ev invocationEvent) *pb.AgentResponse {
	resp := &pb.AgentResponse{
		ConversationId: msg.ConversationId,
		ResponseId:     msg.Id,
	}

	switch ev.Type {
	case "START":
		resp.Payload = &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{Type: pb.ContentChunk_START},
		}
	case "DELTA":
		resp.Payload = &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: ev.Content},
		}
	case "END":
		resp.Payload = &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{Type: pb.ContentChunk_END},
		}
	case "ERROR":
		resp.Payload = &pb.AgentResponse_Error{
			Error: &pb.ErrorResponse{Code: pb.ErrorResponse_AGENT_ERROR, Message: ev.Message},
		}
	case "ELICIT":
		// Blocking renderables over invoke-per-turn are a P2 round-trip; for now
		// surface as an error so the turn terminates cleanly rather than hanging.
		slog.Warn("[agentcore] ELICIT received; blocking renderables not yet supported over invoke",
			"renderable", ev.RenderableID)
		resp.Payload = &pb.AgentResponse_Error{
			Error: &pb.ErrorResponse{
				Code:    pb.ErrorResponse_AGENT_ERROR,
				Message: "interactive prompts are not supported on this runtime yet",
			},
		}
	default:
		slog.Warn("[agentcore] Unknown SSE event type", "type", ev.Type)
		return nil
	}
	return resp
}
