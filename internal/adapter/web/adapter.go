package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/authz"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// WebAdapter implements adapter.Adapter for web browser clients via HTTP + SSE
type WebAdapter struct {
	config           adapter.Config
	msgHandler       adapter.MessageHandler
	connManager      *ConnectionManager
	sessionManager   SessionManager
	threadStore      *store.ThreadHistoryStore
	agentConfigStore *store.AgentConfigStore
	server           *http.Server
	handlers         *Handlers

	// Configuration
	listenAddr        string
	heartbeatInterval time.Duration
	allowedOrigins    []string
	servePlayground   bool
}

// WebAdapterOption configures the WebAdapter
type WebAdapterOption func(*WebAdapter)

// WithListenAddr sets the listen address for the HTTP server
func WithListenAddr(addr string) WebAdapterOption {
	return func(a *WebAdapter) {
		a.listenAddr = addr
	}
}

// WithSessionManager sets the session manager
func WithSessionManager(sm SessionManager) WebAdapterOption {
	return func(a *WebAdapter) {
		a.sessionManager = sm
	}
}

// WithHeartbeatInterval sets the SSE heartbeat interval
func WithHeartbeatInterval(d time.Duration) WebAdapterOption {
	return func(a *WebAdapter) {
		a.heartbeatInterval = d
	}
}

// WithAllowedOrigins sets the allowed CORS origins
func WithAllowedOrigins(origins []string) WebAdapterOption {
	return func(a *WebAdapter) {
		a.allowedOrigins = origins
	}
}

// WithServePlayground enables serving the embedded playground UI
func WithServePlayground(enabled bool) WebAdapterOption {
	return func(a *WebAdapter) {
		a.servePlayground = enabled
	}
}

// New creates a new WebAdapter
func New(opts ...WebAdapterOption) *WebAdapter {
	a := &WebAdapter{
		listenAddr:        ":8080",
		heartbeatInterval: 30 * time.Second,
		sessionManager:    &NoopSessionManager{},
	}

	for _, opt := range opts {
		opt(a)
	}

	return a
}

// Initialize sets up the web adapter with configuration
func (a *WebAdapter) Initialize(ctx context.Context, config adapter.Config) error {
	a.config = config

	// Initialize connection manager
	a.connManager = NewConnectionManager(a.heartbeatInterval)

	// Initialize handlers
	a.handlers = NewHandlers(a.connManager, a.sessionManager, a.threadStore, a.agentConfigStore)

	slog.Info("[Web] Adapter initialized", "listen", a.listenAddr)
	return nil
}

// Start begins the HTTP server and SSE connections
func (a *WebAdapter) Start(ctx context.Context) error {
	// Ensure adapter is initialized
	if a.connManager == nil {
		return fmt.Errorf("connection manager not initialized - call Initialize first")
	}
	if a.handlers == nil {
		return fmt.Errorf("handlers not initialized - call Initialize first")
	}

	// Start connection manager heartbeat
	a.connManager.Start(ctx)

	// Set up HTTP routes
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("POST /api/conversations", a.handlers.HandleCreateConversation)
	mux.HandleFunc("POST /api/conversations/{id}/messages", a.handlers.HandleSendMessage)
	mux.HandleFunc("GET /api/conversations/{id}/stream", a.handlers.HandleStream)
	mux.HandleFunc("GET /api/conversations/{id}/history", a.handlers.HandleHistory)
	mux.HandleFunc("GET /api/agent/config", a.handlers.HandleAgentConfig)
	mux.HandleFunc("GET /api/conversations/{id}/audio", a.handlers.HandleAudioStream)
	mux.HandleFunc("POST /api/conversations/{id}/audio", a.handlers.HandleAudioUpload)
	mux.HandleFunc("GET /health", a.handlers.HandleHealth)

	// Playground UI — must be registered last so API routes always take priority
	if a.servePlayground {
		registerPlaygroundRoutes(mux)
	}

	// Wrap with CORS middleware
	handler := a.corsMiddleware(mux)

	// Create server
	a.server = &http.Server{
		Addr:         a.listenAddr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // No timeout for SSE
		IdleTimeout:  120 * time.Second,
	}

	slog.Info("[Web] Starting HTTP server", "addr", a.listenAddr)

	// Start server in goroutine
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("[Web] HTTP server error", "err", err)
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()
	return a.Stop(context.Background())
}

// Stop gracefully shuts down the adapter
func (a *WebAdapter) Stop(ctx context.Context) error {
	slog.Info("[Web] Stopping adapter...")

	// Close all SSE connections
	if a.connManager != nil {
		a.connManager.CloseAll()
		a.connManager.Stop()
	}

	// Shutdown HTTP server
	if a.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			slog.Error("[Web] Error shutting down server", "err", err)
			return err
		}
	}

	slog.Info("[Web] Adapter stopped")
	return nil
}

// Capabilities returns the adapter's capabilities
func (a *WebAdapter) Capabilities() adapter.AdapterCapabilities {
	return adapter.WebCapabilities()
}

// GetPlatformName returns the platform identifier
func (a *WebAdapter) GetPlatformName() string {
	return "web"
}

// IsHealthy checks if the adapter is connected and healthy
func (a *WebAdapter) IsHealthy(ctx context.Context) bool {
	return a.server != nil && a.connManager != nil
}

// SetMessageHandler sets the handler for incoming messages from the web client
func (a *WebAdapter) SetMessageHandler(handler adapter.MessageHandler) {
	a.msgHandler = handler
	if a.handlers != nil {
		a.handlers.SetMessageHandler(handler)
	}
}

// SetAuthorizer wires the authorizer used to gate every API request. nil
// disables authz (dev mode).
func (a *WebAdapter) SetAuthorizer(az authz.Authorizer) {
	if a.handlers != nil {
		a.handlers.SetAuthorizer(az)
	}
}

// SetAudioForwarder sets the audio streaming forwarder
func (a *WebAdapter) SetAudioForwarder(fwd adapter.AudioForwarder) {
	if a.handlers != nil {
		a.handlers.SetAudioForwarder(fwd)
	}
}

// HandleAgentResponse processes responses from the agent and sends to SSE clients
func (a *WebAdapter) HandleAgentResponse(ctx context.Context, response *pb.AgentResponse) error {
	conversationID := response.ConversationId
	if conversationID == "" {
		return fmt.Errorf("missing conversation ID in response")
	}

	// Convert response to SSE events based on payload type
	switch payload := response.Payload.(type) {
	case *pb.AgentResponse_Content:
		// Content chunk
		event := NewChunkEvent(payload.Content, response.ResponseId)
		a.connManager.Broadcast(conversationID, event)

		// Send finish event on END chunk
		if payload.Content.Type == pb.ContentChunk_END {
			finishEvent := NewFinishEvent(response.ResponseId)
			a.connManager.Broadcast(conversationID, finishEvent)
		}

		// Store message content for thread history
		if a.threadStore != nil && payload.Content.Type == pb.ContentChunk_END {
			a.threadStore.AddMessage(conversationID, &pb.ThreadMessage{
				MessageId: response.ResponseId,
				User: &pb.User{
					Id:       "agent",
					Username: "Agent",
				},
				Content: payload.Content.Content,
			})
		}

	case *pb.AgentResponse_Status:
		// Status update
		event := NewStatusEvent(payload.Status)
		a.connManager.Broadcast(conversationID, event)

	case *pb.AgentResponse_Prompts:
		// Suggested prompts
		event := NewPromptsEvent(payload.Prompts)
		a.connManager.Broadcast(conversationID, event)

	case *pb.AgentResponse_Error:
		// Error response
		event := NewErrorEvent(payload.Error)
		a.connManager.Broadcast(conversationID, event)

	case *pb.AgentResponse_Transcript:
		// Audio transcript — update user message placeholder
		slog.Debug("[Web] Transcript received", "conversation", conversationID, "text", payload.Transcript.Text)
		event := NewTranscriptEvent(payload.Transcript)
		a.connManager.Broadcast(conversationID, event)

	case *pb.AgentResponse_ThreadMetadata:
		// Thread metadata
		slog.Debug("[Web] Thread metadata received", "metadata", payload.ThreadMetadata)

	default:
		slog.Warn("[Web] Unhandled response payload type", "type", fmt.Sprintf("%T", response.Payload))
	}

	return nil
}

// HydrateThread fetches thread history (web adapter maintains its own history)
func (a *WebAdapter) HydrateThread(ctx context.Context, conversationID string, threadStore *store.ThreadHistoryStore) error {
	// Web adapter doesn't need external hydration - history is maintained locally
	return nil
}

// StreamContent streams content chunks to SSE clients
func (a *WebAdapter) StreamContent(ctx context.Context, conversationID string, chunks []*pb.ContentChunk) error {
	for _, chunk := range chunks {
		event := NewChunkEvent(chunk, "")
		a.connManager.Broadcast(conversationID, event)

		// Send finish on END
		if chunk.Type == pb.ContentChunk_END {
			finishEvent := NewFinishEvent("")
			a.connManager.Broadcast(conversationID, finishEvent)
		}
	}
	return nil
}

// SetThreadStore sets the thread history store
func (a *WebAdapter) SetThreadStore(s *store.ThreadHistoryStore) {
	a.threadStore = s
	if a.handlers != nil {
		a.handlers.threadStore = s
	}
}

// SetAgentConfigStore sets the agent config store
func (a *WebAdapter) SetAgentConfigStore(s *store.AgentConfigStore) {
	a.agentConfigStore = s
	if a.handlers != nil {
		a.handlers.agentConfigStore = s
	}
}

// corsMiddleware wraps an http.Handler with CORS headers
func (a *WebAdapter) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if origin != "" && a.isOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
		}

		// Handle preflight
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isOriginAllowed checks if the given origin is in the allowed list
func (a *WebAdapter) isOriginAllowed(origin string) bool {
	for _, allowed := range a.allowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
		// Support wildcard subdomains
		if strings.HasPrefix(allowed, "*.") {
			domain := strings.TrimPrefix(allowed, "*")
			if strings.HasSuffix(origin, domain) {
				return true
			}
		}
	}
	return false
}
