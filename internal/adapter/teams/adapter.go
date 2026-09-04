// Package teams implements adapter.Adapter for Microsoft Teams via the Bot
// Framework Activity protocol.
package teams

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"github.com/infracloudio/msbotbuilder-go/core"
	"github.com/infracloudio/msbotbuilder-go/core/activity"
	"github.com/infracloudio/msbotbuilder-go/schema"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TeamsAdapter implements adapter.Adapter for Microsoft Teams.
type TeamsAdapter struct {
	botAdapter core.Adapter
	listenAddr string
	server     *http.Server

	// unauthenticated is true when TeamsAppID is empty.
	unauthenticated bool

	msgHandler      adapter.MessageHandler
	feedbackHandler adapter.FeedbackHandler

	// refs stores the reference needed to reply later, since the agent's
	// response arrives asynchronously over gRPC, not from this handler.
	refMu sync.Mutex
	refs  map[string]schema.ConversationReference

	// contentBuffers accumulates DELTA chunks per conversation so END sends
	// one complete message.
	bufferMu       sync.Mutex
	contentBuffers map[string]string
}

// New creates a new Teams adapter.
func New() *TeamsAdapter {
	return &TeamsAdapter{
		refs:           make(map[string]schema.ConversationReference),
		contentBuffers: make(map[string]string),
	}
}

// Initialize sets up the Teams adapter with configuration.
func (a *TeamsAdapter) Initialize(ctx context.Context, config adapter.Config) error {
	botAdapter, err := core.NewBotAdapter(core.AdapterSetting{
		AppID:       config.TeamsAppID,
		AppPassword: config.TeamsAppPassword,
	})
	if err != nil {
		return fmt.Errorf("teams: create bot adapter: %w", err)
	}

	if bfa, ok := botAdapter.(*core.BotFrameworkAdapter); ok {
		if config.TeamsAppID == "" {
			slog.Warn("[Teams] TEAMS_APP_ID not set; running unauthenticated. Local dev only — never point this at a real Azure Bot Service registration.")
			a.unauthenticated = true
			bfa.Client = skipEmptyClient{newNoAuthClient(config.TeamsDevHostOverride)}
		} else {
			bfa.Client = skipEmptyClient{bfa.Client}
		}
	}

	a.botAdapter = botAdapter
	a.listenAddr = config.TeamsListenAddr
	if a.listenAddr == "" {
		a.listenAddr = ":3978"
	}
	return nil
}

// Start begins the HTTP server serving Bot Framework activities.
func (a *TeamsAdapter) Start(ctx context.Context) error {
	if a.botAdapter == nil {
		return fmt.Errorf("teams: bot adapter not initialized - call Initialize first")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/messages", a.handleMessages)

	a.server = &http.Server{
		Addr:         a.listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	slog.Info("[Teams] Starting HTTP server", "addr", a.listenAddr)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("[Teams] HTTP server error", "err", err)
		}
	}()

	<-ctx.Done()
	return a.Stop(context.Background())
}

// Stop gracefully shuts down the adapter.
func (a *TeamsAdapter) Stop(ctx context.Context) error {
	slog.Info("[Teams] Stopping adapter...")
	if a.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			slog.Error("[Teams] Error shutting down server", "err", err)
			return err
		}
	}
	slog.Info("[Teams] Adapter stopped")
	return nil
}

// GetPlatformName returns the platform identifier.
func (a *TeamsAdapter) GetPlatformName() string {
	return "teams"
}

// IsHealthy checks if the adapter's HTTP server is running.
func (a *TeamsAdapter) IsHealthy(ctx context.Context) bool {
	return a.botAdapter != nil && a.server != nil
}

// Capabilities returns the adapter's capabilities.
func (a *TeamsAdapter) Capabilities() adapter.AdapterCapabilities {
	return adapter.TeamsCapabilities()
}

// SetMessageHandler sets the handler for incoming messages from Teams.
func (a *TeamsAdapter) SetMessageHandler(handler adapter.MessageHandler) {
	a.msgHandler = handler
}

// SetFeedbackHandler sets the handler for incoming feedback events; Teams has
// no feedback UI here yet, so it's never invoked.
func (a *TeamsAdapter) SetFeedbackHandler(handler adapter.FeedbackHandler) {
	a.feedbackHandler = handler
}

// HydrateThread is a no-op; Teams has no platform-side history fetch yet
// (see Slack's threadTranscript for the pattern).
func (a *TeamsAdapter) HydrateThread(ctx context.Context, conversationID string, threadStore *store.ThreadHistoryStore) error {
	return nil
}

// handleMessages serves POST /api/messages, the Bot Framework's fixed
// endpoint path.
func (a *TeamsAdapter) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// ParseRequest requires an Authorization header even when unauthenticated;
	// Agents Playground sends none.
	if a.unauthenticated && r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer unauthenticated-local-dev")
	}

	act, err := a.botAdapter.ParseRequest(ctx, r)
	if err != nil {
		slog.Warn("[Teams] parse request failed", "err", err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	handler := activity.HandlerFuncs{
		OnMessageFunc: func(turn *activity.TurnContext) (schema.Activity, error) {
			a.onMessage(ctx, turn.Activity)
			return schema.Activity{}, nil
		},
	}

	if err := a.botAdapter.ProcessActivity(ctx, act, handler); err != nil {
		slog.Warn("[Teams] process activity failed", "err", err)
	}
	w.WriteHeader(http.StatusOK)
}

func (a *TeamsAdapter) onMessage(ctx context.Context, act schema.Activity) {
	conversationID := act.Conversation.ID
	if conversationID == "" {
		slog.Warn("[Teams] message activity missing conversation ID")
		return
	}

	ref := activity.GetCoversationReference(act)
	// Clearing ActivityID makes replies post as new messages instead of
	// activity-replies: msbotbuilder-go's reply-to path builds its URL from
	// activity.ID (always empty on a reply) instead of ReplyToID, so a reply
	// would otherwise silently fail to render.
	ref.ActivityID = ""
	a.refMu.Lock()
	a.refs[conversationID] = ref
	a.refMu.Unlock()

	if a.msgHandler == nil {
		return
	}

	msg := &pb.Message{
		Id:        uuid.NewString(),
		Timestamp: timestamppb.Now(),
		Platform:  "teams",
		PlatformContext: &pb.PlatformContext{
			MessageId: act.ID,
			ChannelId: conversationID,
			UserId:    act.From.ID,
		},
		User: &pb.User{
			Id:       act.From.ID,
			Username: act.From.Name,
		},
		Content:        act.Text,
		ConversationId: conversationID,
	}

	if err := a.msgHandler(ctx, msg); err != nil {
		slog.Warn("[Teams] message handler failed", "err", err)
	}
}
