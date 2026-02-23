package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/astromode-ai/astro-messaging/internal/adapter"
	"github.com/astromode-ai/astro-messaging/internal/store"
	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultListenAddr  = ":8081"
	responseTimeout    = 5 * time.Minute
	defaultInputSchema = `{"type":"object"}`
)

// MCPAdapter implements adapter.Adapter for MCP (Model Context Protocol) clients.
// It exposes an MCP server over HTTP (streamable transport) with a send_message tool
// that forwards to the agent via the messaging gRPC handler.
type MCPAdapter struct {
	config     adapter.Config
	msgHandler adapter.MessageHandler
	server     *http.Server
	listenAddr string

	// mcpServer is the current MCP server (send_message + dynamic tools from AgentConfig). Swapped on UpdateFromAgentConfig.
	mu        sync.Mutex
	mcpServer *mcp.Server
	// pending: conversationID -> channel to send final response text when agent responds
	pending map[string]chan string
	// buffers: conversationID -> accumulated content until END
	buffers map[string]*strings.Builder
}

// SendMessageParams is the MCP tool input for send_message.
// jsonschema tags must be plain description text only (no "required,description=..." format).
type SendMessageParams struct {
	Content string `json:"content" jsonschema:"Message text to send to the agent (required)"`
}

// New creates a new MCPAdapter.
func New(opts ...Option) *MCPAdapter {
	a := &MCPAdapter{
		listenAddr: defaultListenAddr,
		pending:    make(map[string]chan string),
		buffers:    make(map[string]*strings.Builder),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Option configures the MCP adapter.
type Option func(*MCPAdapter)

// WithListenAddr sets the listen address for the MCP HTTP server.
func WithListenAddr(addr string) Option {
	return func(a *MCPAdapter) {
		a.listenAddr = addr
	}
}

// Initialize sets up the adapter with configuration.
func (a *MCPAdapter) Initialize(ctx context.Context, config adapter.Config) error {
	a.config = config
	log.Printf("[MCP] Adapter initialized (listen: %s)", a.listenAddr)
	return nil
}

// Start starts the MCP HTTP server with the streamable transport.
func (a *MCPAdapter) Start(ctx context.Context) error {
	a.mu.Lock()
	a.mcpServer = a.buildMCPServerLocked(nil)
	a.mu.Unlock()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return a.getMCPServer()
	}, nil)

	a.server = &http.Server{
		Addr:         a.listenAddr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("[MCP] Starting HTTP server on %s", a.listenAddr)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[MCP] HTTP server error: %v", err)
		}
	}()

	<-ctx.Done()
	return a.Stop(context.Background())
}

// Stop gracefully shuts down the MCP HTTP server.
func (a *MCPAdapter) Stop(ctx context.Context) error {
	log.Println("[MCP] Stopping adapter...")
	if a.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			log.Printf("[MCP] Error shutting down server: %v", err)
			return err
		}
	}
	log.Println("[MCP] Adapter stopped")
	return nil
}

// GetPlatformName returns the platform identifier.
func (a *MCPAdapter) GetPlatformName() string {
	return "mcp"
}

// IsHealthy returns true if the HTTP server is running.
func (a *MCPAdapter) IsHealthy(ctx context.Context) bool {
	return a.server != nil
}

// Capabilities returns the adapter's capabilities.
func (a *MCPAdapter) Capabilities() adapter.AdapterCapabilities {
	return adapter.MCPCapabilities()
}

// SetMessageHandler sets the handler for incoming messages (from MCP tool calls).
func (a *MCPAdapter) SetMessageHandler(handler adapter.MessageHandler) {
	a.msgHandler = handler
}

// HandleAgentResponse receives agent responses and delivers them to pending tool calls.
func (a *MCPAdapter) HandleAgentResponse(ctx context.Context, response *pb.AgentResponse) error {
	conversationID := response.ConversationId
	if conversationID == "" {
		return nil
	}

	a.mu.Lock()
	buf, hasBuf := a.buffers[conversationID]
	a.mu.Unlock()

	switch payload := response.Payload.(type) {
	case *pb.AgentResponse_Content:
		if payload.Content == nil {
			return nil
		}
		if hasBuf {
			if payload.Content.Type == pb.ContentChunk_DELTA || payload.Content.Content != "" {
				buf.WriteString(payload.Content.Content)
			}
		}
		if payload.Content.Type == pb.ContentChunk_END {
			a.mu.Lock()
			defer a.mu.Unlock()
			if ch, ok := a.pending[conversationID]; ok {
				text := ""
				if b, ok := a.buffers[conversationID]; ok {
					text = b.String()
				}
				select {
				case ch <- text:
				default:
				}
				close(ch)
				delete(a.pending, conversationID)
				delete(a.buffers, conversationID)
			}
		}

	case *pb.AgentResponse_Error:
		a.mu.Lock()
		defer a.mu.Unlock()
		if ch, ok := a.pending[conversationID]; ok {
			msg := "agent error"
			if payload.Error != nil && payload.Error.Message != "" {
				msg = payload.Error.Message
			}
			select {
			case ch <- "[Error] " + msg:
			default:
			}
			close(ch)
			delete(a.pending, conversationID)
			delete(a.buffers, conversationID)
		}
	}

	return nil
}

// HydrateThread is a no-op for MCP (no external thread history).
func (a *MCPAdapter) HydrateThread(ctx context.Context, conversationID string, _ *store.ThreadHistoryStore) error {
	return nil
}

// UpdateFromAgentConfig implements adapter.AgentConfigReceiver. It rebuilds the MCP server with
// send_message plus one MCP tool per agent tool; dynamic tool calls are forwarded to the agent
// as messages with content {"_toolCall":true,"name":"...","arguments":{...}}.
func (a *MCPAdapter) UpdateFromAgentConfig(config *pb.AgentConfig) {
	if config == nil {
		return
	}
	newServer := a.buildMCPServerLocked(config)
	a.mu.Lock()
	a.mcpServer = newServer
	a.mu.Unlock()
}

// getMCPServer returns the current MCP server (caller must not mutate it).
func (a *MCPAdapter) getMCPServer() *mcp.Server {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mcpServer
}

// buildMCPServerLocked creates a new MCP server. When config has mcp_definition, those are exposed.
// When mcp_definition is empty, only send_message is exposed. Agent tools (config.tools) are used for
// execution; mcp_definition defines what the MCP interface exposes.
// Caller must not hold a.mu (buildMCPServerLocked does not access shared mutable state).
func (a *MCPAdapter) buildMCPServerLocked(config *pb.AgentConfig) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "astro-messaging-mcp",
		Version: "1.0.0",
	}, nil)
	tools := config.GetMcpDefinition()
	if len(tools) == 0 {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "send_message",
			Description: "Send a message to the Astro AI agent and receive the agent's response.",
		}, a.handleSendMessage)
		return server
	}
	for _, t := range tools {
		name := strings.TrimSpace(t.GetName())
		if name == "" {
			continue
		}
		desc := t.GetDescription()
		schemaJSON := t.GetInputSchemaJson()
		if schemaJSON == "" {
			schemaJSON = defaultInputSchema
		}
		schema := json.RawMessage(schemaJSON)
		tool := &mcp.Tool{
			Name:        name,
			Description: desc,
			InputSchema: schema,
		}
		server.AddTool(tool, a.handleDynamicTool(name))
	}
	return server
}

// handleDynamicTool returns a ToolHandler that forwards the tool call to the agent and waits for the response.
// The agent receives a message with content {"_toolCall":true,"name":toolName,"arguments":<raw>}.
func (a *MCPAdapter) handleDynamicTool(toolName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args json.RawMessage = json.RawMessage("{}")
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			args = req.Params.Arguments
		}
		payload := map[string]any{
			"_toolCall": true,
			"name":      toolName,
			"arguments": args,
		}
		contentBytes, err := json.Marshal(payload)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: " + err.Error()}},
				IsError: true,
			}, nil
		}
		conversationID := uuid.New().String()
		messageID := uuid.New().String()
		msg := &pb.Message{
			Id:        messageID,
			Timestamp: timestamppb.New(time.Now()),
			Platform:  "mcp",
			PlatformContext: &pb.PlatformContext{
				MessageId: messageID,
				ChannelId: conversationID,
			},
			User: &pb.User{
				Id:       "mcp-client",
				Username: "MCP Client",
			},
			Content:        string(contentBytes),
			ConversationId: conversationID,
		}
		ch := make(chan string, 1)
		a.mu.Lock()
		a.pending[conversationID] = ch
		a.buffers[conversationID] = &strings.Builder{}
		a.mu.Unlock()
		if a.msgHandler == nil {
			a.mu.Lock()
			delete(a.pending, conversationID)
			delete(a.buffers, conversationID)
			a.mu.Unlock()
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: message handler not configured"}},
				IsError: true,
			}, nil
		}
		if err := a.msgHandler(ctx, msg); err != nil {
			a.mu.Lock()
			delete(a.pending, conversationID)
			delete(a.buffers, conversationID)
			a.mu.Unlock()
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: " + err.Error()}},
				IsError: true,
			}, nil
		}
		select {
		case responseText := <-ch:
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: responseText}},
			}, nil
		case <-ctx.Done():
			a.mu.Lock()
			delete(a.pending, conversationID)
			delete(a.buffers, conversationID)
			a.mu.Unlock()
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: request cancelled or timed out"}},
				IsError: true,
			}, nil
		case <-time.After(responseTimeout):
			a.mu.Lock()
			delete(a.pending, conversationID)
			delete(a.buffers, conversationID)
			a.mu.Unlock()
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: no response within %v", responseTimeout)}},
				IsError: true,
			}, nil
		}
	}
}

// handleSendMessage is the MCP tool handler: it forwards the message to the agent and waits for the response.
func (a *MCPAdapter) handleSendMessage(ctx context.Context, req *mcp.CallToolRequest, params *SendMessageParams) (*mcp.CallToolResult, any, error) {
	if params == nil || strings.TrimSpace(params.Content) == "" {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Error: content is required"}},
			IsError: true,
		}, nil, nil
	}

	// This ID is sent with the message so the agent/gRPC pipeline echo it back; we use it to
	// route the response to this blocked call via a.pending. We generate it here because the
	// MCP Go SDK does not expose the JSON-RPC request "id" to tool handlers.
	conversationID := uuid.New().String()
	messageID := uuid.New().String()
	now := time.Now()

	msg := &pb.Message{
		Id:        messageID,
		Timestamp: timestamppb.New(now),
		Platform:  "mcp",
		PlatformContext: &pb.PlatformContext{
			MessageId: messageID,
			ChannelId: conversationID,
		},
		User: &pb.User{
			Id:       "mcp-client",
			Username: "MCP Client",
		},
		Content:        params.Content,
		ConversationId: conversationID,
	}

	// Register pending response channel and buffer before sending
	ch := make(chan string, 1)
	a.mu.Lock()
	a.pending[conversationID] = ch
	a.buffers[conversationID] = &strings.Builder{}
	a.mu.Unlock()

	if a.msgHandler == nil {
		a.mu.Lock()
		delete(a.pending, conversationID)
		delete(a.buffers, conversationID)
		a.mu.Unlock()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Error: message handler not configured"}},
			IsError: true,
		}, nil, nil
	}

	if err := a.msgHandler(ctx, msg); err != nil {
		a.mu.Lock()
		delete(a.pending, conversationID)
		delete(a.buffers, conversationID)
		a.mu.Unlock()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Error: " + err.Error()}},
			IsError: true,
		}, nil, nil
	}

	// Wait for agent response with timeout
	select {
	case responseText := <-ch:
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: responseText}},
		}, nil, nil
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.pending, conversationID)
		delete(a.buffers, conversationID)
		a.mu.Unlock()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Error: request cancelled or timed out"}},
			IsError: true,
		}, nil, nil
	case <-time.After(responseTimeout):
		a.mu.Lock()
		delete(a.pending, conversationID)
		delete(a.buffers, conversationID)
		a.mu.Unlock()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: no response within %v", responseTimeout)}},
			IsError: true,
		}, nil, nil
	}
}
