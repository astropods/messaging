package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/authz"
	"github.com/astropods/messaging/internal/logctx"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/files"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// chatPersistThrottle bounds how often a streaming assistant turn is
// progressively written to the chat store — roughly one write per interval
// rather than one per token. The terminal END chunk always flushes the final
// text regardless of this throttle.
const chatPersistThrottle = 250 * time.Millisecond

// defaultTurnIdleTimeout reaps a turn whose agent has produced no output for this
// long. Generous so a long quiet tool call (a slow model or tool with no
// intermediate streaming) isn't mistaken for a hang; any agent activity resets
// it. Tunable per deployment via WithTurnIdleTimeout. The client keeps its own
// far-larger absolute backstop, so this can favor avoiding false reaps.
const defaultTurnIdleTimeout = 5 * time.Minute

// WebAdapter implements adapter.Adapter for web browser clients via HTTP + SSE
type WebAdapter struct {
	config           adapter.Config
	msgHandler       adapter.MessageHandler
	connManager      *ConnectionManager
	sessionManager   SessionManager
	threadStore      *store.ThreadHistoryStore
	agentConfigStore *store.AgentConfigStore
	chatStore        *sqlite.Store
	fileStore        files.FileStore
	interactions     store.InteractionStore
	server           *http.Server
	handlers         *Handlers
	turns            *turnTracker

	// Configuration
	listenAddr        string
	heartbeatInterval time.Duration
	turnIdleTimeout   time.Duration
	allowedOrigins    []string
	// freshSubscribeSettle: how long a no-cursor subscribe observes the wire before
	// the store-derived terminal fallback (see settleFreshSubscribe).
	freshSubscribeSettle time.Duration
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

// WithTurnIdleTimeout overrides the idle-turn reap window. Zero disables it.
func WithTurnIdleTimeout(d time.Duration) WebAdapterOption {
	return func(a *WebAdapter) {
		a.turnIdleTimeout = d
	}
}

// WithFreshSubscribeSettle sets how long a fresh SSE subscribe waits for a live
// turn event before synthesizing a terminal finish from the store. Zero decides
// immediately (no settle).
func WithFreshSubscribeSettle(d time.Duration) WebAdapterOption {
	return func(a *WebAdapter) {
		a.freshSubscribeSettle = d
	}
}

// WithAllowedOrigins sets the allowed CORS origins
func WithAllowedOrigins(origins []string) WebAdapterOption {
	return func(a *WebAdapter) {
		a.allowedOrigins = origins
	}
}

// WithInteractionStore overrides the interaction store (defaults to in-memory).
func WithInteractionStore(s store.InteractionStore) WebAdapterOption {
	return func(a *WebAdapter) {
		a.interactions = s
	}
}

// New creates a new WebAdapter
func New(opts ...WebAdapterOption) *WebAdapter {
	a := &WebAdapter{
		listenAddr:           ":8080",
		heartbeatInterval:    30 * time.Second,
		turnIdleTimeout:      defaultTurnIdleTimeout,
		freshSubscribeSettle: 300 * time.Millisecond,
		sessionManager:       &NoopSessionManager{},
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

	// Per-turn streaming state, shared with the HTTP handlers so a client stop
	// (HandleCancel) and the agent response loop (HandleAgentResponse) agree on
	// which turns are stopped and what partial text was streamed.
	a.turns = newTurnTracker()
	a.turns.setIdleReaper(a.turnIdleTimeout, a.reapIdleTurn)

	if a.interactions == nil {
		a.interactions = store.NewMemoryInteractionStore()
	}

	// Initialize handlers
	a.handlers = NewHandlers(a.connManager, a.sessionManager, a.threadStore, a.agentConfigStore)
	a.handlers.turns = a.turns
	a.handlers.freshSubscribeSettle = a.freshSubscribeSettle
	a.handlers.interactions = a.interactions

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
	mux.HandleFunc("POST /api/conversations/{id}/cancel", a.handlers.HandleCancel)
	mux.HandleFunc("GET /api/conversations/{id}/stream", a.handlers.HandleStream)
	mux.HandleFunc("GET /api/conversations/{id}/history", a.handlers.HandleHistory)
	mux.HandleFunc("GET /api/agent/config", a.handlers.HandleAgentConfig)

	// Platform chat-page contract (served via astro-server /chat/* proxy).
	mux.HandleFunc("GET /api/chat/conversations", a.handlers.HandleListChatConversations)
	mux.HandleFunc("GET /api/chat/conversations/{id}", a.handlers.HandleGetChatConversation)
	mux.HandleFunc("PUT /api/chat/conversations/{id}/title", a.handlers.HandleSetChatConversationTitle)
	mux.HandleFunc("DELETE /api/chat/conversations/{id}", a.handlers.HandleDeleteChatConversation)
	mux.HandleFunc("POST /api/chat/conversations/{id}/interactions/{interactionId}", a.handlers.HandleInteractionResponse)
	mux.HandleFunc("GET /api/conversations/{id}/audio", a.handlers.HandleAudioStream)
	mux.HandleFunc("POST /api/conversations/{id}/audio", a.handlers.HandleAudioUpload)

	// Agent files API (served via astro-server /files/* proxy). Opaque-key
	// contract: create reserves a key + upload target, content PUT/GET move the
	// bytes, metadata + delete manage the entry.
	mux.HandleFunc("GET /api/files", a.handlers.HandleListFiles)
	mux.HandleFunc("GET /api/files/usage", a.handlers.HandleFilesUsage)
	mux.HandleFunc("POST /api/files", a.handlers.HandleCreateFile)
	mux.HandleFunc("GET /api/files/{key}", a.handlers.HandleGetFile)
	mux.HandleFunc("DELETE /api/files/{key}", a.handlers.HandleDeleteFile)
	mux.HandleFunc("PUT /api/files/{key}/content", a.handlers.HandlePutFileContent)
	mux.HandleFunc("GET /api/files/{key}/content", a.handlers.HandleGetFileContent)

	mux.HandleFunc("GET /health", a.handlers.HandleHealth)

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

// SetFeedbackHandler wires the handler that forwards platform feedback (the chat
// "stop generating" StreamControl) to the agent over the gRPC stream. The only
// feedback the web adapter emits today is the stop signal from HandleCancel.
func (a *WebAdapter) SetFeedbackHandler(handler adapter.FeedbackHandler) {
	if a.handlers != nil {
		a.handlers.SetFeedbackHandler(handler)
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

// conversationOwner returns the WorkOS user id that owns a conversation, used to
// attribute agent-produced files for per-user access control. Empty when the
// chat store is disabled or the conversation is unknown (in which case the
// agent's output file stays unattributed and is not user-downloadable).
func (a *WebAdapter) conversationOwner(ctx context.Context, conversationID string) string {
	if a.chatStore == nil {
		return ""
	}
	conv, err := a.chatStore.Get(ctx, conversationID)
	if err != nil || conv == nil {
		return ""
	}
	return conv.UserID
}

// reapIdleTurn is the turn tracker's idle callback: the turn produced no agent
// activity within the idle window, so finalize it as stalled. failTurn logs the
// surfaced error, so this only records the reason at debug.
func (a *WebAdapter) reapIdleTurn(conversationID string) {
	slog.Debug("[Web] turn idle timeout; finalizing stalled turn", "conversation", conversationID)
	a.failTurn(context.Background(), conversationID, "The agent stopped responding. You can try sending again.")
}

// HandleAgentDisconnect finalizes in-flight turns when an agent stream ends
// (implements adapter.AgentDisconnectHandler). The shared single-agent stream
// (adapter.AgentStreamID) owns every in-flight turn, so all are reaped; a
// per-conversation stream owns only its own conversation, so only that turn is
// reaped and other live streams' turns are left untouched.
func (a *WebAdapter) HandleAgentDisconnect(ctx context.Context, conversationID string) {
	if a.turns == nil {
		return
	}
	const msg = "The agent disconnected. You can try sending again."
	if conversationID != adapter.AgentStreamID {
		a.failTurn(ctx, conversationID, msg)
		return
	}
	for _, id := range a.turns.activeConversations() {
		a.failTurn(ctx, id, msg)
	}
}

// failTurn abnormally terminates an in-flight turn: finalize the store row (so it
// stops deriving assistant_streaming), broadcast a retryable error, and close the
// conversation's SSE connections. failActive claims the turn atomically, so this
// is a no-op once the turn has ended and the terminal event fires exactly once.
func (a *WebAdapter) failTurn(ctx context.Context, conversationID, message string) {
	if a.turns == nil {
		return
	}
	partial, ok := a.turns.failActive(conversationID)
	if !ok {
		return
	}
	// Log every surfaced abnormal termination in the sidecar (the source of truth
	// for chat errors). This is delivered to the client as an in-band SSE error
	// event on the 200 stream, not an HTTP 5xx, so it does not inflate the
	// astro-server per-route 5xx rate.
	slog.Warn("[Web] finalizing in-flight turn with terminal error", "conversation", conversationID, "message", message)
	if a.chatStore != nil {
		if _, err := a.chatStore.FinalizeTerminal(ctx, conversationID, partial); err != nil {
			if errors.Is(err, sqlite.ErrMessageLimitReached) {
				slog.Debug("[Web] chat at message limit; stalled turn not finalized", "conversation", conversationID)
			} else {
				slog.Error("[Web] chat finalize stalled turn failed", "conversation", conversationID, "err", err)
			}
		}
	}
	if a.connManager != nil {
		a.connManager.Broadcast(conversationID, NewErrorEventFromMessage("AGENT_UNAVAILABLE", message, true))
		a.connManager.CloseConversation(conversationID)
	}
}

// HandleAgentResponse processes responses from the agent and sends to SSE clients
func (a *WebAdapter) HandleAgentResponse(ctx context.Context, response *pb.AgentResponse) error {
	conversationID := response.ConversationId
	log := logctx.FromContext(ctx)
	if conversationID == "" {
		return fmt.Errorf("missing conversation ID in response")
	}

	// Any agent response is liveness; reset the idle watchdog.
	if a.turns != nil {
		a.turns.touch(conversationID)
	}

	// Convert response to SSE events based on payload type
	switch payload := response.Payload.(type) {
	case *pb.AgentResponse_Content:
		// The user stopped this turn: drop the agent's remaining output so a
		// non-cooperating agent's late/complete reply can't reach the client or
		// be persisted to the deployment-local chat store. The gate stays closed
		// until the agent starts a NEW turn (a START chunk); it is deliberately
		// NOT lifted by the next user send, so the stopped turn's trailing output
		// can't bleed into the following message on the same conversation.
		if a.turns != nil && a.turns.gateContent(conversationID, payload.Content.Type == pb.ContentChunk_START) {
			// Dropped because the turn is stopped. If this is the stopped
			// generation's terminal chunk, clear the gate so the entry doesn't
			// linger for a stopped-then-abandoned conversation.
			if payload.Content.Type == pb.ContentChunk_END {
				a.turns.clear(conversationID)
			}
			return nil
		}

		// Buffer the partial so a mid-stream stop can persist exactly what the
		// user saw so far (the stop path reads it via turnTracker.stop).
		if a.turns != nil {
			a.turns.record(conversationID, payload.Content)
		}

		// Resolve any agent-produced file attachments (present on the END chunk)
		// to the canonical shape, attributing them to the conversation owner so
		// per-user file access control applies to the agent's outputs too.
		agentAttachments := resolveResponseAttachments(ctx, a.fileStore, payload.Content.Attachments, a.conversationOwner(ctx, conversationID))

		// Content chunk
		event := NewChunkEvent(payload.Content, response.ResponseId, agentAttachments)
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

		// Persist the assistant reply to the deployment-local chat store. Write
		// progressively (throttled) during the stream and always flush on END, so
		// the store holds a stable in-flight assistant row throughout the turn.
		// This is what lets a client that switches conversations mid-stream,
		// reloads, or reconnects reconcile to the store instead of rebuilding the
		// reply from SSE deltas (which the server never replays). Persist the full
		// buffered text (what the user saw), not this chunk's own content — agents
		// stream deltas and usually send an empty END, so a single chunk alone
		// would persist a partial or blank reply.
		if a.chatStore != nil {
			isEnd := payload.Content.Type == pb.ContentChunk_END
			if isEnd || (a.turns != nil && a.turns.dueForPersist(conversationID, chatPersistThrottle)) {
				content := payload.Content.Content
				if a.turns != nil {
					if buffered := a.turns.content(conversationID); buffered != "" {
						content = buffered
					}
				}
				if _, err := a.chatStore.UpsertAssistantProgress(ctx, conversationID, content, marshalAttachments(agentAttachments)); err != nil {
					if errors.Is(err, sqlite.ErrMessageLimitReached) {
						// Terminal per-conversation state, not a real failure — don't
						// spam ERROR on every throttled write near the cap.
						log.Debug("[Web] chat at message limit; assistant reply not persisted", "conversation", conversationID)
					} else {
						log.Error("[Web] chat persist assistant message failed", "conversation", conversationID, "err", err)
					}
				}
			}
		}

		// Turn completed normally — drop per-turn tracker state.
		if payload.Content.Type == pb.ContentChunk_END && a.turns != nil {
			a.turns.clear(conversationID)
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
		// Finalize the turn in the store so it doesn't report assistant_streaming
		// forever on reload (the persisted flag is "the latest message is the
		// user's"). An error can terminate a turn before any assistant row is
		// written, leaving the user row last; persist whatever partial the user
		// saw (empty if none) as the terminal assistant row. FinalizeTerminal
		// appends only when the last row is still the user's, so a spurious error
		// after a completed turn (buffer already cleared) can't blank the reply.
		// Runs before clear() so the buffered partial is still available.
		if a.chatStore != nil {
			partial := ""
			if a.turns != nil {
				partial = a.turns.content(conversationID)
			}
			if _, err := a.chatStore.FinalizeTerminal(ctx, conversationID, partial); err != nil {
				if errors.Is(err, sqlite.ErrMessageLimitReached) {
					log.Debug("[Web] chat at message limit; errored turn not finalized", "conversation", conversationID)
				} else {
					log.Error("[Web] chat finalize errored turn failed", "conversation", conversationID, "err", err)
				}
			}
		}
		// The error terminates the turn; drop the per-turn tracker state (also
		// lifts any stop-gate — a no-op when the conversation isn't stopped).
		if a.turns != nil {
			a.turns.clear(conversationID)
		}
		event := NewErrorEvent(payload.Error)
		a.connManager.Broadcast(conversationID, event)

	case *pb.AgentResponse_Renderable:
		a.handleRenderable(ctx, conversationID, payload.Renderable)

	case *pb.AgentResponse_Transcript:
		// Audio transcript — update user message placeholder
		log.Debug("[Web] Transcript received", "conversation", conversationID, "text", payload.Transcript.Text)
		event := NewTranscriptEvent(payload.Transcript)
		a.connManager.Broadcast(conversationID, event)

	case *pb.AgentResponse_ThreadMetadata:
		// Thread metadata
		log.Debug("[Web] Thread metadata received", "metadata", payload.ThreadMetadata)

	default:
		log.Warn("[Web] Unhandled response payload type", "type", fmt.Sprintf("%T", response.Payload))
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
	owner := a.conversationOwner(ctx, conversationID)
	for _, chunk := range chunks {
		event := NewChunkEvent(chunk, "", resolveResponseAttachments(ctx, a.fileStore, chunk.Attachments, owner))
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

// SetChatStore wires the sidecar-local SQLite chat store. nil disables chat
// persistence (e.g. local dev without CHAT_DB_PATH).
func (a *WebAdapter) SetChatStore(s *sqlite.Store) {
	a.chatStore = s
	if a.handlers != nil {
		a.handlers.chatStore = s
	}
}

// SetFileStore wires the agent files store. nil disables the files API (e.g.
// local dev without FILES_DIR).
func (a *WebAdapter) SetFileStore(s files.FileStore) {
	a.fileStore = s
	if a.handlers != nil {
		a.handlers.fileStore = s
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
