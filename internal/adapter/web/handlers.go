package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/authz"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/files"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handlers contains HTTP handlers for the web adapter
type Handlers struct {
	connManager      *ConnectionManager
	sessionManager   SessionManager
	authz            authz.Authorizer // nil = skip authz (dev convenience)
	msgHandler       adapter.MessageHandler
	feedbackHandler  adapter.FeedbackHandler
	audioForwarder   adapter.AudioForwarder
	threadStore      *store.ThreadHistoryStore
	agentConfigStore *store.AgentConfigStore
	// chatStore persists the platform chat UI thread (sidebar + bodies) in
	// the sidecar-local SQLite database on a shared persistent volume.
	chatStore *sqlite.Store
	// fileStore backs the agent files API (upload/download) on the shared
	// persistent volume. nil disables the feature (no FILES_DIR configured).
	fileStore files.FileStore
	// turns tracks per-conversation streaming state so a client stop can drop
	// the agent's late output and persist the partial. Shared with WebAdapter.
	turns *turnTracker
	// freshSubscribeSettle bounds how long a fresh subscribe waits for a live turn
	// event before the terminal-state fallback (see settleFreshSubscribe). Zero
	// decides immediately.
	freshSubscribeSettle time.Duration
	interactions         store.InteractionStore // shared with WebAdapter
	degraded             *degradeTracker        // shared with WebAdapter
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

// SetAuthorizer wires the authorizer used to gate every API request. nil
// disables the check (dev mode); production wiring sets a real Authorizer
// in main.
func (h *Handlers) SetAuthorizer(a authz.Authorizer) {
	h.authz = a
}

// authenticate runs both authn (session) and authz (allowed-to-use-this-deployment).
// On success it returns the session; on failure it has already written the
// response and returns nil — the caller should `return` immediately.
//
// Centralising authn+authz here keeps every protected handler a single guard
// line and makes it impossible to forget the authz check on a new endpoint.
func (h *Handlers) authenticate(w http.ResponseWriter, r *http.Request) *Session {
	ctx := r.Context()

	session, err := h.sessionManager.ValidateRequest(ctx, r)
	if err != nil {
		http.Error(w, "Authentication error", http.StatusInternalServerError)
		return nil
	}
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil
	}

	if h.authz != nil {
		// Web identity is a globally-unique WorkOS user_id, so no scope.
		// The resolved user_id is the same value we sent in — no need to
		// thread it back through the handler.
		res, err := h.authz.Authorize(ctx, authz.IdentityTypeUser, session.UserID, authz.AdapterWeb, "")
		if err != nil {
			// Fail closed on authz transport errors — better to return a 503
			// than to silently drop the check.
			slog.Warn("[Web] authz check failed", "user_id", session.UserID, "err", err) //nolint:gosec // session.UserID is from a trusted ALB OIDC header
			http.Error(w, "Authorization unavailable", http.StatusServiceUnavailable)
			return nil
		}
		if !res.Allowed {
			slog.Warn("[Web] authz denied", "user_id", session.UserID) //nolint:gosec // session.UserID is from a trusted ALB OIDC header
			http.Error(w, "Forbidden", http.StatusForbidden)
			return nil
		}
	}

	return session
}

// SetMessageHandler sets the message handler
func (h *Handlers) SetMessageHandler(handler adapter.MessageHandler) {
	h.msgHandler = handler
}

// SetFeedbackHandler sets the handler that forwards platform feedback (the chat
// "stop generating" StreamControl) to the agent over the gRPC stream.
func (h *Handlers) SetFeedbackHandler(handler adapter.FeedbackHandler) {
	h.feedbackHandler = handler
}

// SetAudioForwarder sets the audio streaming forwarder
func (h *Handlers) SetAudioForwarder(fwd adapter.AudioForwarder) {
	h.audioForwarder = fwd
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

// maxAttachmentsPerMessage bounds how many files one message may reference,
// keeping a send from fanning out into unbounded metadata reads / storage.
const maxAttachmentsPerMessage = 16

// SendMessageRequest represents a request to send a message
type SendMessageRequest struct {
	Content string `json:"content"`
	// Attachments references files already uploaded via the files API (by key).
	// The bytes are not inlined — only the key + display metadata ride the send.
	Attachments []sendAttachmentInput `json:"attachments,omitempty"`
}

// sendAttachmentInput is a client-declared attachment reference on a send. Only
// Key is trusted; the authoritative name/size/content-type are re-read from the
// file store so a client can't misrepresent a file it doesn't own metadata for.
type sendAttachmentInput struct {
	Key string `json:"key"`
}

// SendMessageResponse represents the response to a message send
type SendMessageResponse struct {
	MessageID string `json:"message_id"`
	Timestamp string `json:"timestamp"`
}

// HandleCreateConversation handles POST /api/conversations
func (h *Handlers) HandleCreateConversation(w http.ResponseWriter, r *http.Request) {
	// Authenticate the request (session) and authorize against this
	// deployment's grants. authenticate writes the response on failure.
	session := h.authenticate(w, r)
	if session == nil {
		return
	}

	// Parse request
	var req CreateConversationRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // Allow empty body
	}

	// Generate conversation ID
	conversationID := uuid.NewString()

	// Persist the conversation row (title is derived later, on first send).
	if h.chatStore != nil {
		if err := h.chatStore.Upsert(r.Context(), conversationID, session.UserID, ""); err != nil {
			slog.Error("[Web] chat persist create conversation failed", "err", err)
		}
	}

	resp := CreateConversationResponse{
		ConversationID: conversationID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error(fmt.Sprintf("[Web] Encode error on create conversation: %v", err))
	}
	slog.Debug(fmt.Sprintf("[Web] Conversation created: id=%q, user=%q", conversationID, session.UserID)) //nolint:gosec // G706 false positive: %q escapes control characters
}

// HandleSendMessage handles POST /api/conversations/{id}/messages
func (h *Handlers) HandleSendMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Authenticate the request (session) and authorize against this
	// deployment's grants. authenticate writes the response on failure.
	session := h.authenticate(w, r)
	if session == nil {
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

	// A message must carry text or at least one attachment (a file can be sent
	// with no accompanying prose, matching Claude-style attach-and-send).
	if req.Content == "" && len(req.Attachments) == 0 {
		http.Error(w, "Content or attachment is required", http.StatusBadRequest)
		return
	}

	// A degraded free-text-tolerant ask: the owner's reply is its RESPOND answer,
	// not a new turn (a non-owner falls through and leaves it pending).
	if h.degraded != nil {
		if interactionID, ok := h.degraded.take(conversationID, session.UserID); ok {
			h.resolveDegradedRespond(ctx, conversationID, session, interactionID, req.Content)
			writeJSON(w, http.StatusOK, SendMessageResponse{
				MessageID: uuid.NewString(),
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
	}

	// Bound the attachment count so a single send can't fan out into thousands
	// of metadata reads + an oversized message/DB row (the proxy body cap alone
	// would still allow that many keys).
	if len(req.Attachments) > maxAttachmentsPerMessage {
		http.Error(w, "too many attachments", http.StatusRequestEntityTooLarge)
		return
	}

	// Resolve declared attachments to authoritative metadata from the file store,
	// scoped to the sender: an unknown key OR a key owned by another user is
	// rejected (a stale/forged chip, or an attempt to attach someone else's file).
	var (
		attachments []chatAttachment
		protoAtts   []*pb.Attachment
	)
	for _, in := range req.Attachments {
		att, ok := resolveAttachment(ctx, h.fileStore, in.Key, session.UserID)
		if !ok {
			http.Error(w, "unknown attachment", http.StatusBadRequest)
			return
		}
		attachments = append(attachments, att)
		protoAtts = append(protoAtts, toProtoAttachment(att))
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
			EventKind: pb.PlatformContext_EVENT_KIND_DM,
		},
		User: &pb.User{
			Id:        session.UserID,
			Username:  session.Username,
			Email:     session.Email,
			AvatarUrl: session.AvatarURL,
		},
		Content:        req.Content,
		ConversationId: conversationID,
		Attachments:    protoAtts,
	}

	if h.msgHandler == nil {
		slog.Warn("[Web] No message handler registered")
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Persist the user turn BEFORE forwarding: HandleAgentResponse runs on another
	// goroutine, so a fast agent could persist its reply before this write lands,
	// inverting turn order or dropping the reply. EnsureForSend also enforces
	// ownership, rejecting a foreign conversation before the agent runs. (See the
	// changelog for the full ordering/ownership rationale.)
	if h.chatStore != nil {
		title := sqlite.TruncateRunes(req.Content, chatTitleMaxRunes)
		owned, err := h.chatStore.EnsureForSend(ctx, conversationID, session.UserID, title)
		if err != nil {
			// Couldn't verify ownership (transient store error) — fail closed with
			// a retryable 503, not an authz denial.
			slog.Error("[Web] chat ensure conversation failed", "err", err)
			http.Error(w, "chat temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if !owned {
			// Missing or foreign conversation: 404 (not 403) to avoid leaking existence.
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		// A failed user-turn write must not run the agent (would persist an
		// assistant-first / user-less turn).
		if _, err := h.chatStore.AppendMessage(ctx, conversationID, session.UserID, "user", req.Content, marshalAttachments(attachments)); err != nil {
			// The message cap is terminal, not transient — return 409 with a
			// machine-readable code so the client keys on it (not the bare status).
			if errors.Is(err, sqlite.ErrMessageLimitReached) {
				slog.Warn("[Web] chat conversation at message limit", "conversation", conversationID)
				writeJSON(w, http.StatusConflict, map[string]string{
					"error":             "message_limit_reached",
					"error_description": "conversation message limit reached; start a new chat",
				})
				return
			}
			slog.Error("[Web] chat persist user message failed", "err", err)
			http.Error(w, "chat temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	if err := h.msgHandler(ctx, msg); err != nil {
		slog.Error(fmt.Sprintf("[Web] Error forwarding message: %v", err))
		// Forwarding failed after the user turn was persisted, and the agent will
		// never respond — so nothing else finalizes the store. Without this the
		// latest row stays the user's and the thread derives assistant_streaming
		// forever. Mirror the AgentResponse_Error path (empty partial: nothing
		// streamed); FinalizeTerminal appends only when the last row is the user's.
		if h.chatStore != nil {
			if _, ferr := h.chatStore.FinalizeTerminal(ctx, conversationID, ""); ferr != nil {
				slog.Error("[Web] chat finalize on forward failure failed", "conversation", conversationID, "err", ferr)
			}
		}
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
		slog.Error(fmt.Sprintf("[Web] Encode error on send message: %v", err))
	}
	slog.Debug(fmt.Sprintf("[Web] Message sent: id=%q, conversation=%q, user=%q", messageID, conversationID, session.UserID)) //nolint:gosec // G706 false positive: %q escapes control characters
}

// HandleCancel handles POST /api/conversations/{id}/cancel — the chat
// "stop generating" action.
//
// The agent's model call runs in the agent process, so the sidecar cannot halt
// generation directly. Instead it (1) marks the turn stopped so the agent's
// remaining chunks are dropped until the next turn's START (see
// HandleAgentResponse), (2) persists the partial the client saw and flips the
// conversation out of the streaming state in the deployment-local chat store,
// (3) sends a terminal finish event and closes the conversation's SSE
// connections so the client turn ends promptly, and (4) forwards a StreamControl
// STOP to the agent as a best-effort signal for agents/SDKs that honor it.
// Agents that don't honor it keep running, but their output is discarded.
func (h *Handlers) HandleCancel(w http.ResponseWriter, r *http.Request) {
	// Authenticate the request (session) and authorize against this
	// deployment's grants. authenticate writes the response on failure.
	session := h.authenticate(w, r)
	if session == nil {
		return
	}

	// Extract conversation ID from path
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	// Enforce conversation ownership BEFORE any side effect, matching the
	// 404-on-foreign pattern used by send/rename/delete. FinalizeStopped is
	// already user-scoped, but the in-memory stop-gate and the SSE teardown below
	// act on the raw id — without this guard a deployment user who learned another
	// user's conversation UUID could drop their in-flight output and close their
	// stream. Without a chat store there is no ownership to check (dev
	// convenience); deployment-level authz above still applies.
	if h.chatStore != nil {
		conv, err := h.chatStore.Get(r.Context(), conversationID)
		if err != nil {
			slog.Error("[Web] chat cancel owner lookup failed", "conversation", conversationID, "err", err)
			http.Error(w, "chat temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if conv == nil || conv.UserID != session.UserID {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
	}

	// Mark the turn stopped and capture the partial the client has seen so far.
	partial := ""
	if h.turns != nil {
		partial = h.turns.stop(conversationID)
	}

	// Make the turn terminal in the deployment-local chat store so the chat page
	// stops showing it as streaming. Never shrinks an already-finished reply;
	// when the partial was progressively persisted this is a no-op.
	if h.chatStore != nil {
		if _, err := h.chatStore.FinalizeStopped(r.Context(), conversationID, session.UserID, partial); err != nil {
			slog.Error("[Web] chat finalize stopped failed", "conversation", conversationID, "err", err)
		}
	}

	// End any live SSE turn: emit finish, then close the conversation's SSE
	// connections so the client turn ends promptly instead of lingering.
	h.connManager.Broadcast(conversationID, NewFinishEvent(""))
	h.connManager.CloseConversation(conversationID)

	// Best-effort: signal the agent to stop generating. Honored by SDKs/agents
	// that consume StreamControl; ignored otherwise (their output is dropped).
	if h.feedbackHandler != nil {
		fb := &pb.PlatformFeedback{
			ConversationId: conversationID,
			Timestamp:      timestamppb.Now(),
			Feedback: &pb.PlatformFeedback_StreamControl{
				StreamControl: &pb.StreamControl{
					Action: pb.StreamControl_STOP,
					Reason: "user stopped generation",
				},
			},
			User: &pb.User{
				Id:       session.UserID,
				Username: session.Username,
			},
		}
		if err := h.feedbackHandler(r.Context(), fb); err != nil {
			slog.Debug("[Web] stop signal not delivered to agent", "conversation", conversationID, "err", err)
		}
	}

	slog.Debug(fmt.Sprintf("[Web] Conversation stopped: id=%q, user=%q", conversationID, session.UserID)) //nolint:gosec // G706 false positive: %q escapes control characters

	w.WriteHeader(http.StatusNoContent)
}

// streamTurnTerminal reports whether conversationID's latest turn has already
// finished — the assistant reply is persisted and this sidecar is not actively
// streaming it. It mirrors the assistant_streaming derivation in
// HandleGetChatConversation so a subscribe-time replay agrees with what a
// history GET (and therefore a page refresh) would show.
//
// Returns false whenever the state is unknown or the turn may still be live (no
// chat store, an active local stream, an unreadable store, or the latest
// message still the user's), so a finish is only ever replayed for a turn that
// genuinely ended.
func (h *Handlers) streamTurnTerminal(ctx context.Context, conversationID string) bool {
	if h.chatStore == nil {
		return false
	}
	// An actively streaming turn delivers its finish over the live broadcast;
	// replaying one here would end the turn early.
	if h.turns != nil && h.turns.isStreaming(conversationID) {
		return false
	}
	//nolint:dogsled // PageMessages returns (msgs, hasMore, oldestSeq, lastRole, err); only the last role and error matter here.
	_, _, _, lastRole, err := h.chatStore.PageMessages(ctx, conversationID, 1, 0)
	if err != nil {
		slog.Error("[Web] chat stream terminal-state check failed", "conversation", conversationID, "err", err)
		return false
	}
	// Latest message is the assistant's => the reply landed and the turn is over.
	// "user" (awaiting the first chunk) or "" (empty thread) => still in flight.
	return lastRole == "assistant"
}

// settleFreshSubscribe resolves the ambiguity a fresh subscribe (no resume
// cursor) faces before the terminal-state fallback. The store snapshot alone
// can't tell a finished turn from a new turn whose POST /messages hasn't
// persisted its user row yet, and a just-finished turn may have broadcast its
// finish to zero connections. Rather than guess synchronously — which misfires
// in two opposite windows: a spurious finish on a follow-up turn that raced
// ahead of its user-row write, or a missed finish while isStreaming still lingers
// from the turn that just ended — the connection is already registered, so
// observe what actually happens next for a bounded window:
//
//   - a live content chunk => the turn is live (a new turn's first output, or a
//     lagging one); return false so the caller's event loop streams it.
//   - a live finish/error => the turn ended on the wire; deliver it and stop.
//   - silence for freshSubscribeSettle => consult the store. By now any
//     concurrent send has had time to persist its user row and any just-
//     broadcast finish's turn has had time to clear its streaming flag, so a
//     store-derived terminal replay is unambiguous.
//
// Returns true when a terminal finish was delivered (the caller should return),
// false when the turn is live and the caller should enter its event loop. A zero
// settle window degrades to an immediate store-derived decision.
func (h *Handlers) settleFreshSubscribe(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	conn *SSEConnection,
	conversationID string,
) bool {
	timer := time.NewTimer(h.freshSubscribeSettle)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return true
		case <-conn.Done:
			return true
		case event := <-conn.EventChan:
			_, _ = fmt.Fprint(w, event.Format()) //nolint:gosec // buffered SSE events are internally constructed
			flusher.Flush()
			switch event.Event {
			case EventFinish, EventError:
				return true
			case EventHeartbeat:
				// A heartbeat says nothing about the turn — keep settling.
				continue
			default:
				// A content chunk: the turn is live. Hand back to the event loop.
				return false
			}
		case <-timer.C:
			if h.streamTurnTerminal(ctx, conversationID) {
				finishEvent := NewFinishEvent("")
				_, _ = fmt.Fprint(w, finishEvent.Format()) //nolint:gosec // SSE event data is constructed internally, not from user input
				flusher.Flush()
				slog.Debug(fmt.Sprintf("[Web] SSE replayed finish for already-finished turn: connection=%q, conversation=%q", conn.ID, conversationID)) //nolint:gosec // G706 false positive: %q escapes control characters
				return true
			}
			return false
		}
	}
}

// parseLastEventID reads the SSE resume cursor the browser replays on an
// EventSource reconnect. Returns (0, false) when absent or unparseable, so the
// caller treats it as a fresh subscribe rather than resuming from a bogus point.
func parseLastEventID(r *http.Request) (uint64, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		return 0, false
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// HandleStream handles GET /api/conversations/{id}/stream (SSE)
func (h *Handlers) HandleStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Authenticate the request (session) and authorize against this
	// deployment's grants. authenticate writes the response on failure.
	session := h.authenticate(w, r)
	if session == nil {
		return
	}

	// Extract conversation ID from path
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	// Enforce conversation ownership before subscribing, matching the
	// send/cancel/get boundary. The SSE stream is a live read of the
	// conversation's agent output, so a caller who learned another user's
	// conversation UUID must not be able to subscribe and watch their turn. Done
	// before any SSE header/flush so a foreign request gets a clean 404 rather
	// than a hijacked stream. Without a chat store there is no ownership to check
	// (dev convenience); deployment-level authz above still applies.
	if h.chatStore != nil {
		conv, err := h.chatStore.Get(r.Context(), conversationID)
		if err != nil {
			slog.Error("[Web] chat stream owner lookup failed", "conversation", conversationID, "err", err)
			http.Error(w, "chat temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if conv == nil || conv.UserID != session.UserID {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
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

	// Register connection. EventSource replays its Last-Event-ID header on
	// reconnect; when present, register and atomically snapshot the events missed
	// since that id so no event is both replayed and delivered live (see
	// AddWithResume). When absent — a fresh subscribe — register normally and rely
	// on the terminal-state replay below.
	lastEventID, resuming := parseLastEventID(r)
	var missed []SSEEvent
	if resuming {
		missed = h.connManager.AddWithResume(conn, lastEventID)
	} else {
		h.connManager.Add(conn)
	}
	defer h.connManager.Remove(conversationID, connID)

	// Send connected event
	connectedEvent := NewConnectedEvent(conversationID, connID)
	_, _ = fmt.Fprint(w, connectedEvent.Format()) //nolint:gosec // SSE event data is constructed internally, not from user input
	flusher.Flush()

	slog.Debug(fmt.Sprintf("[Web] SSE stream started: connection=%q, conversation=%q, user=%q, resume=%v", connID, conversationID, session.UserID, resuming)) //nolint:gosec // G706 false positive: %q escapes control characters

	if resuming {
		// Replay everything missed while disconnected, in order, before any live
		// event. Their ids advance the client's Last-Event-ID so a later reconnect
		// resumes from the right point. If the turn is still live the loop below
		// then delivers the rest; if it already ended, the replay carried the
		// finish and the client will close.
		replayedTerminal := false
		for _, ev := range missed {
			_, _ = fmt.Fprint(w, ev.Format()) //nolint:gosec // buffered SSE events are internally constructed
			if ev.Event == EventFinish || ev.Event == EventError {
				replayedTerminal = true
			}
		}
		flusher.Flush()
		if len(missed) > 0 {
			slog.Debug(fmt.Sprintf("[Web] SSE resumed: connection=%q, conversation=%q, replayed=%d", connID, conversationID, len(missed))) //nolint:gosec // G706 false positive
		}
		// Safety net for a cursor that predates the retained buffer (or a buffer
		// evicted under memory pressure): if the replay didn't carry a terminal
		// event but the store shows the turn ended, release the client anyway.
		if !replayedTerminal && h.streamTurnTerminal(ctx, conversationID) {
			finishEvent := NewFinishEvent("")
			_, _ = fmt.Fprint(w, finishEvent.Format()) //nolint:gosec // SSE event data is constructed internally, not from user input
			flusher.Flush()
			return
		}
	} else if h.settleFreshSubscribe(ctx, w, flusher, conn, conversationID) {
		// The turn is unambiguously over — a fast reply that completed inside the
		// send→subscribe window, or a finish that broadcast to zero connections.
		// settleFreshSubscribe replayed the terminal finish; nothing more to stream.
		return
	}

	// Event loop
	for {
		select {
		case <-ctx.Done():
			slog.Debug(fmt.Sprintf("[Web] SSE stream context cancelled: connection=%s", connID))
			return
		case <-conn.Done:
			// The connection was closed (e.g. HandleCancel broadcasts a terminal
			// finish and then closes in the same call). Because select picks a
			// ready case at random, Done can win over an already-enqueued finish;
			// drain and flush any buffered events before returning so that finish
			// isn't dropped.
			for {
				select {
				case event := <-conn.EventChan:
					_, _ = fmt.Fprint(w, event.Format())
					flusher.Flush()
				default:
					slog.Debug(fmt.Sprintf("[Web] SSE stream closed: connection=%s", connID))
					return
				}
			}
		case event := <-conn.EventChan:
			_, _ = fmt.Fprint(w, event.Format())
			flusher.Flush()
		}
	}
}

// HandleHistory handles GET /api/conversations/{id}/history
func (h *Handlers) HandleHistory(w http.ResponseWriter, r *http.Request) {
	// Authenticate the request (session) and authorize against this
	// deployment's grants. authenticate writes the response on failure.
	session := h.authenticate(w, r)
	if session == nil {
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
		slog.Error(fmt.Sprintf("[Web] Encode error on get history: %v", err))
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
		slog.Error(fmt.Sprintf("[Web] Encode error on health: %v", err))
	}
}

// HandleAgentConfig handles GET /api/agent/config
func (h *Handlers) HandleAgentConfig(w http.ResponseWriter, r *http.Request) {
	// Authenticate the request (session) and authorize against this
	// deployment's grants. The agent config exposes the system prompt and
	// tool list — leaking it to a denied principal is a real disclosure.
	if h.authenticate(w, r) == nil {
		return
	}

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
		slog.Error(fmt.Sprintf("[Web] Encode error on agent config: %v", err))
	}
}

// sendErrorEvent broadcasts an error event to all connections for a conversation
func (h *Handlers) sendErrorEvent(conversationID, code, message string) {
	event := NewErrorEventFromMessage(code, message, false)
	h.connManager.Broadcast(conversationID, event)
}
