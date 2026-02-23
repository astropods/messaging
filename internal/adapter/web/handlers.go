package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handlers contains HTTP handlers for the web adapter
type Handlers struct {
	connManager      *ConnectionManager
	sessionManager   SessionManager
	msgHandler       adapter.MessageHandler
	threadStore      *store.ThreadHistoryStore
	agentConfigStore *store.AgentConfigStore
}

// NewHandlers creates a new Handlers instance
func NewHandlers(connManager *ConnectionManager, sessionManager SessionManager, threadStore *store.ThreadHistoryStore, agentConfigStore *store.AgentConfigStore) *Handlers {
	return &Handlers{
		connManager:      connManager,
		sessionManager:   sessionManager,
		threadStore:      threadStore,
		agentConfigStore: agentConfigStore,
	}
}

// SetMessageHandler sets the message handler
func (h *Handlers) SetMessageHandler(handler adapter.MessageHandler) {
	h.msgHandler = handler
}

// CreateConversationRequest represents a request to create a new conversation
type CreateConversationRequest struct {
	Title    string            `json:"title,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// CreateConversationResponse represents the response to a conversation creation
type CreateConversationResponse struct {
	ConversationID string `json:"conversation_id"`
	CreatedAt      string `json:"created_at"`
}

// SendMessageRequest represents a request to send a message
type SendMessageRequest struct {
	Content string `json:"content"`
}

// SendMessageResponse represents the response to a message send
type SendMessageResponse struct {
	MessageID string `json:"message_id"`
	Timestamp string `json:"timestamp"`
}

// HandleCreateConversation handles POST /api/conversations
func (h *Handlers) HandleCreateConversation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Validate session
	session, err := h.sessionManager.ValidateRequest(ctx, r)
	if err != nil {
		http.Error(w, "Authentication error", http.StatusInternalServerError)
		return
	}
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse request
	var req CreateConversationRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // Allow empty body
	}

	// Generate conversation ID
	conversationID := uuid.NewString()

	resp := CreateConversationResponse{
		ConversationID: conversationID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[Web] Encode error on create conversation: %v", err)
	}
	log.Printf("[Web] Conversation created: id=%s, user=%s", conversationID, session.UserID)
}

// HandleSendMessage handles POST /api/conversations/{id}/messages
func (h *Handlers) HandleSendMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Validate session
	session, err := h.sessionManager.ValidateRequest(ctx, r)
	if err != nil {
		http.Error(w, "Authentication error", http.StatusInternalServerError)
		return
	}
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract conversation ID from path
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	// Parse request
	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Content == "" {
		http.Error(w, "Content is required", http.StatusBadRequest)
		return
	}

	// Create message
	messageID := uuid.NewString()
	now := time.Now()

	msg := &pb.Message{
		Id:        messageID,
		Timestamp: timestamppb.New(now),
		Platform:  "web",
		PlatformContext: &pb.PlatformContext{
			MessageId: messageID,
			ChannelId: conversationID,
		},
		User: &pb.User{
			Id:        session.UserID,
			Username:  session.Username,
			Email:     session.Email,
			AvatarUrl: session.AvatarURL,
		},
		Content:        req.Content,
		ConversationId: conversationID,
	}

	// Forward to gRPC handler
	if h.msgHandler == nil {
		log.Printf("[Web] No message handler registered")
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.msgHandler(ctx, msg); err != nil {
		log.Printf("[Web] Error forwarding message: %v", err)
		h.sendErrorEvent(conversationID, "INTERNAL_ERROR", "Failed to process message")
		http.Error(w, "Failed to process message", http.StatusInternalServerError)
		return
	}

	// Add to thread store
	if h.threadStore != nil {
		h.threadStore.AddMessage(conversationID, &pb.ThreadMessage{
			MessageId: messageID,
			User: &pb.User{
				Id:       session.UserID,
				Username: session.Username,
			},
			Content:   req.Content,
			Timestamp: timestamppb.New(now),
		})
	}

	resp := SendMessageResponse{
		MessageID: messageID,
		Timestamp: now.UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[Web] Encode error on send message: %v", err)
	}
	log.Printf("[Web] Message sent: id=%s, conversation=%s, user=%s", messageID, conversationID, session.UserID)
}

// HandleStream handles GET /api/conversations/{id}/stream (SSE)
func (h *Handlers) HandleStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Validate session
	session, err := h.sessionManager.ValidateRequest(ctx, r)
	if err != nil {
		http.Error(w, "Authentication error", http.StatusInternalServerError)
		return
	}
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract conversation ID from path
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Flush headers
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	flusher.Flush()

	// Create connection
	connID := uuid.NewString()
	conn := &SSEConnection{
		ID:             connID,
		ConversationID: conversationID,
		EventChan:      make(chan SSEEvent, 100),
		Done:           make(chan struct{}),
		CreatedAt:      time.Now(),
		LastEventAt:    time.Now(),
	}

	// Register connection
	h.connManager.Add(conn)
	defer h.connManager.Remove(conversationID, connID)

	// Send connected event
	connectedEvent := NewConnectedEvent(conversationID, connID)
	fmt.Fprint(w, connectedEvent.Format())
	flusher.Flush()

	log.Printf("[Web] SSE stream started: connection=%s, conversation=%s, user=%s", connID, conversationID, session.UserID)

	// Event loop
	for {
		select {
		case <-ctx.Done():
			log.Printf("[Web] SSE stream context cancelled: connection=%s", connID)
			return
		case <-conn.Done:
			log.Printf("[Web] SSE stream closed: connection=%s", connID)
			return
		case event := <-conn.EventChan:
			fmt.Fprint(w, event.Format())
			flusher.Flush()
		}
	}
}

// HandleHistory handles GET /api/conversations/{id}/history
func (h *Handlers) HandleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Validate session
	session, err := h.sessionManager.ValidateRequest(ctx, r)
	if err != nil {
		http.Error(w, "Authentication error", http.StatusInternalServerError)
		return
	}
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract conversation ID from path
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	// Get history from thread store
	var history *pb.ThreadHistoryResponse
	if h.threadStore != nil {
		history = h.threadStore.GetHistory(conversationID, 50, false)
	} else {
		history = &pb.ThreadHistoryResponse{
			ConversationId: conversationID,
			Messages:       []*pb.ThreadMessage{},
			IsComplete:     true,
			FetchedAt:      timestamppb.Now(),
		}
	}

	// Convert to JSON-friendly format
	messages := make([]map[string]interface{}, 0, len(history.Messages))
	for _, msg := range history.Messages {
		m := map[string]interface{}{
			"message_id": msg.MessageId,
			"content":    msg.Content,
			"timestamp":  msg.Timestamp.AsTime().Format(time.RFC3339),
			"was_edited": msg.WasEdited,
		}
		if msg.User != nil {
			m["user"] = map[string]interface{}{
				"id":       msg.User.Id,
				"username": msg.User.Username,
			}
		}
		messages = append(messages, m)
	}

	resp := map[string]interface{}{
		"conversation_id": history.ConversationId,
		"messages":        messages,
		"is_complete":     history.IsComplete,
		"fetched_at":      history.FetchedAt.AsTime().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[Web] Encode error on get history: %v", err)
	}
}

// HandleHealth handles GET /health
func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"status":      "healthy",
		"connections": h.connManager.GetTotalConnections(),
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[Web] Encode error on health: %v", err)
	}
}

// HandleAgentConfig handles GET /api/agent/config
func (h *Handlers) HandleAgentConfig(w http.ResponseWriter, r *http.Request) {
	if h.agentConfigStore == nil {
		http.Error(w, "Agent config not available", http.StatusNotFound)
		return
	}

	config := h.agentConfigStore.Get()
	if config == nil {
		http.Error(w, "Agent config not yet received", http.StatusNotFound)
		return
	}

	// Build JSON response matching the playground's AgentConfig type
	type toolGraphNode struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	type toolGraphEdge struct {
		ID     string `json:"id"`
		Source string `json:"source"`
		Target string `json:"target"`
	}
	type toolGraph struct {
		Nodes []toolGraphNode `json:"nodes"`
		Edges []toolGraphEdge `json:"edges"`
	}
	type toolConfig struct {
		Name        string     `json:"name"`
		Title       string     `json:"title"`
		Description string     `json:"description"`
		Type        string     `json:"type"`
		Graph       *toolGraph `json:"graph,omitempty"`
	}
	type agentConfigResp struct {
		SystemPrompt string       `json:"systemPrompt"`
		Tools        []toolConfig `json:"tools"`
	}

	tools := make([]toolConfig, 0, len(config.Tools))
	for _, t := range config.Tools {
		tc := toolConfig{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			Type:        t.Type,
		}
		if t.Graph != nil {
			g := &toolGraph{
				Nodes: make([]toolGraphNode, 0, len(t.Graph.Nodes)),
				Edges: make([]toolGraphEdge, 0, len(t.Graph.Edges)),
			}
			for _, n := range t.Graph.Nodes {
				g.Nodes = append(g.Nodes, toolGraphNode{ID: n.Id, Name: n.Name, Type: n.Type})
			}
			for _, e := range t.Graph.Edges {
				g.Edges = append(g.Edges, toolGraphEdge{ID: e.Id, Source: e.Source, Target: e.Target})
			}
			tc.Graph = g
		}
		tools = append(tools, tc)
	}

	resp := agentConfigResp{
		SystemPrompt: config.SystemPrompt,
		Tools:        tools,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[Web] Encode error on agent config: %v", err)
	}
}

// sendErrorEvent broadcasts an error event to all connections for a conversation
func (h *Handlers) sendErrorEvent(conversationID, code, message string) {
	event := NewErrorEventFromMessage(code, message, false)
	h.connManager.Broadcast(conversationID, event)
}
