package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/logctx"
	"github.com/astropods/messaging/internal/peers"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CardPath is where A2A clients look for an agent card.
const CardPath = "/.well-known/agent-card.json"

// defaultSendTimeout caps how long message/send blocks. On expiry the task is
// returned still working rather than failed, so the caller polls tasks/get
// instead of losing a turn that is simply slow.
const defaultSendTimeout = 5 * time.Minute

// platformName is the Message.platform value for A2A traffic, and the adapter
// key it registers under.
const platformName = "a2a"

// Adapter implements adapter.Adapter for agent-to-agent calls over A2A.
//
// Peers reach it on cluster Service DNS only. The namespace's
// allow-namespace-traffic NetworkPolicy already confines that to one account,
// which is why there is no caller authorization here yet.
type Adapter struct {
	config           adapter.Config
	msgHandler       adapter.MessageHandler
	agentConfigStore *store.AgentConfigStore
	agentReadiness   adapter.AgentReadiness
	server           *http.Server
	tasks            *registry
	peers            *peers.Registry

	listenAddr  string
	agentName   string
	description string
	publicURL   string
	sendTimeout time.Duration
}

// Option configures an Adapter.
type Option func(*Adapter)

func WithListenAddr(addr string) Option {
	return func(a *Adapter) { a.listenAddr = addr }
}

// WithAgentIdentity sets the name, description and absolute URL the agent card
// advertises. The URL cannot be derived in-process; it is the sidecar's own
// Service DNS name, which only astro-server knows.
func WithAgentIdentity(name, description, publicURL string) Option {
	return func(a *Adapter) {
		a.agentName = name
		a.description = description
		a.publicURL = publicURL
	}
}

func WithSendTimeout(d time.Duration) Option {
	return func(a *Adapter) { a.sendTimeout = d }
}

func New(opts ...Option) *Adapter {
	a := &Adapter{
		listenAddr:  ":8100",
		agentName:   "agent",
		sendTimeout: defaultSendTimeout,
		tasks:       newRegistry(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// SetAgentConfigStore wires the store the agent card reads skills from.
func (a *Adapter) SetAgentConfigStore(s *store.AgentConfigStore) {
	a.agentConfigStore = s
}

// SetAgentReadiness wires the check that reports whether an agent has dialled
// in, so a call arriving before the agent is up fails with a clear reason.
func (a *Adapter) SetAgentReadiness(fn adapter.AgentReadiness) {
	a.agentReadiness = fn
}

// SetPeerRegistry wires discovery of the other A2A agents in this account.
func (a *Adapter) SetPeerRegistry(r *peers.Registry) {
	a.peers = r
}

// Peers returns the A2A agents this agent can call. Empty when discovery is
// not configured, so a caller need not distinguish "no peers" from "no
// registry".
func (a *Adapter) Peers(ctx context.Context) ([]peers.Peer, error) {
	if a.peers == nil {
		return []peers.Peer{}, nil
	}
	return a.peers.List(ctx)
}

func (a *Adapter) Initialize(ctx context.Context, config adapter.Config) error {
	a.config = config
	if a.tasks == nil {
		a.tasks = newRegistry()
	}
	slog.Info("[A2A] Adapter initialized", "listen", a.listenAddr, "url", a.publicURL)
	return nil
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.msgHandler == nil {
		return fmt.Errorf("message handler not set - call SetMessageHandler first")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+CardPath, a.handleCard)
	// The A2A card's url points at the root, so the JSON-RPC endpoint is the
	// root itself rather than a sub-path.
	mux.HandleFunc("POST /", a.handleRPC)
	mux.HandleFunc("GET /health", a.handleHealth)

	a.server = &http.Server{
		Addr:              a.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a turn can legitimately outlast any fixed budget,
		// and sendTimeout is the real bound.
		IdleTimeout: 120 * time.Second,
	}

	slog.Info("[A2A] Starting HTTP server", "addr", a.listenAddr)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("[A2A] HTTP server error", "err", err)
		}
	}()

	go a.warmPeers(ctx)

	<-ctx.Done()
	return a.Stop(context.Background())
}

func (a *Adapter) Stop(ctx context.Context) error {
	slog.Info("[A2A] Stopping adapter...")
	if a.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			slog.Error("[A2A] Error shutting down server", "err", err)
			return err
		}
	}
	slog.Info("[A2A] Adapter stopped")
	return nil
}

func (a *Adapter) GetPlatformName() string { return platformName }

func (a *Adapter) IsHealthy(ctx context.Context) bool { return a.server != nil }

func (a *Adapter) Capabilities() adapter.AdapterCapabilities {
	return adapter.A2ACapabilities()
}

func (a *Adapter) SetMessageHandler(handler adapter.MessageHandler) { a.msgHandler = handler }

// SetFeedbackHandler is a no-op: A2A has no feedback widget for a peer to use.
func (a *Adapter) SetFeedbackHandler(handler adapter.FeedbackHandler) {}

// HydrateThread is a no-op: an A2A caller sends the whole context it wants the
// agent to see on each call, so there is no platform-side history to fetch.
func (a *Adapter) HydrateThread(ctx context.Context, conversationID string, store *store.ThreadHistoryStore) error {
	return nil
}

// HandleAgentResponse routes the agent's streamed reply to the task waiting on
// that conversation.
func (a *Adapter) HandleAgentResponse(ctx context.Context, res *pb.AgentResponse) error {
	if res == nil || res.ConversationId == "" {
		return fmt.Errorf("missing conversation ID in response")
	}
	task := a.tasks.active(res.ConversationId)
	if task == nil {
		return nil
	}
	switch payload := res.Payload.(type) {
	case *pb.AgentResponse_Content:
		task.record(payload.Content)
	case *pb.AgentResponse_Error:
		message := "agent reported an error"
		if payload.Error != nil && payload.Error.Message != "" {
			message = payload.Error.Message
		}
		task.fail(message)
	}
	return nil
}

// HandleAgentDisconnect finalizes tasks whose agent stream ended, so a caller
// gets a terminal task instead of blocking until sendTimeout.
func (a *Adapter) HandleAgentDisconnect(ctx context.Context, conversationID string) {
	if conversationID == adapter.AgentStreamID {
		for _, task := range a.tasks.all() {
			task.fail("agent disconnected")
		}
		return
	}
	if task := a.tasks.active(conversationID); task != nil {
		task.fail("agent disconnected")
	}
}

// warmPeers primes the discovery cache at startup. A failure is logged and
// left alone: discovery retries on the next List, and inbound A2A calls do not
// depend on it.
func (a *Adapter) warmPeers(ctx context.Context) {
	if a.peers == nil {
		return
	}
	found, err := a.peers.List(ctx)
	if err != nil {
		slog.Warn("[A2A] Initial peer discovery failed", "err", err)
		return
	}
	names := make([]string, 0, len(found))
	for _, p := range found {
		names = append(names, p.Name())
	}
	slog.Info("[A2A] Discovered peer agents", "count", len(found), "peers", names)
}

func (a *Adapter) handleCard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildCard(a.agentName, a.description, a.publicURL, a.agentConfigStore))
}

func (a *Adapter) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRPC dispatches one JSON-RPC call. Every outcome, including a failure,
// is an HTTP 200 carrying a JSON-RPC error object, per the transport spec.
func (a *Adapter) handleRPC(w http.ResponseWriter, r *http.Request) {
	ctx := logctx.WithTraceparent(r.Context(), r.Header.Get("traceparent"))

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, errorResponse(nil, codeParseError, "invalid JSON"))
		return
	}
	if req.JSONRPC != jsonRPCVersion || req.Method == "" {
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeInvalidRequest, "jsonrpc must be 2.0 and method is required"))
		return
	}

	switch req.Method {
	case "message/send":
		writeJSON(w, http.StatusOK, a.send(ctx, req))
	case "tasks/get":
		writeJSON(w, http.StatusOK, a.getTask(req))
	case "tasks/cancel":
		writeJSON(w, http.StatusOK, a.cancelTask(req))
	case "message/stream", "tasks/resubscribe":
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeUnsupportedOperation, "streaming is not supported; the agent card advertises capabilities.streaming=false"))
	default:
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeMethodNotFound, "unknown method "+req.Method))
	}
}

func (a *Adapter) send(ctx context.Context, req request) response {
	var params sendParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "params must carry a message object")
	}
	text := params.Message.Text()
	if text == "" {
		return errorResponse(req.ID, codeInvalidParams, "message must carry at least one non-empty text part")
	}
	if a.agentReadiness != nil && !a.agentReadiness() {
		return errorResponse(req.ID, codeInternalError, "agent is not connected")
	}

	// A2A contextId is our conversation ID: both mean "the thread this turn
	// belongs to", so reusing it keeps the agent's own threading intact.
	contextID := params.Message.ContextID
	if contextID == "" {
		contextID = uuid.NewString()
	}
	task := newTaskEntry(uuid.NewString(), contextID)
	a.tasks.add(task)

	messageID := params.Message.MessageID
	if messageID == "" {
		messageID = uuid.NewString()
	}
	msg := &pb.Message{
		Id:             messageID,
		Timestamp:      timestamppb.Now(),
		Platform:       platformName,
		Content:        text,
		ConversationId: contextID,
		User: &pb.User{
			Id:       platformName,
			Username: "Peer agent",
		},
		PlatformContext: &pb.PlatformContext{
			MessageId: messageID,
			ChannelId: contextID,
			ThreadId:  contextID,
			EventKind: pb.PlatformContext_EVENT_KIND_DM,
			PlatformData: map[string]string{
				"a2a_task_id": task.id,
			},
		},
	}

	if err := a.msgHandler(ctx, msg); err != nil {
		reason := err.Error()
		if errors.Is(err, adapter.ErrNoAgentStream) {
			reason = "no agent is connected to serve this call"
		}
		task.fail(reason)
		return resultResponse(req.ID, task.snapshot())
	}

	select {
	case <-task.done:
	case <-ctx.Done():
	case <-time.After(a.sendTimeout):
		logctx.FromContext(ctx).Warn("[A2A] send timed out, task left working", "task_id", task.id)
	}
	return resultResponse(req.ID, task.snapshot())
}

func (a *Adapter) getTask(req request) response {
	var params taskIDParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.ID == "" {
		return errorResponse(req.ID, codeInvalidParams, "params must carry a task id")
	}
	task := a.tasks.byTaskID(params.ID)
	if task == nil {
		return errorResponse(req.ID, codeTaskNotFound, "no task with id "+params.ID)
	}
	return resultResponse(req.ID, task.snapshot())
}

func (a *Adapter) cancelTask(req request) response {
	var params taskIDParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.ID == "" {
		return errorResponse(req.ID, codeInvalidParams, "params must carry a task id")
	}
	task := a.tasks.byTaskID(params.ID)
	if task == nil {
		return errorResponse(req.ID, codeTaskNotFound, "no task with id "+params.ID)
	}
	if !task.cancel() {
		return errorResponse(req.ID, codeTaskNotCancelable, "task "+params.ID+" already reached a terminal state")
	}
	return resultResponse(req.ID, task.snapshot())
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("[A2A] Error writing response", "err", err)
	}
}
