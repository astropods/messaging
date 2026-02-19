package grpc

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"github.com/astromode-ai/astro-messaging/internal/adapter"
	"github.com/astromode-ai/astro-messaging/internal/store"
	"github.com/astromode-ai/astro-messaging/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements the AgentMessaging gRPC service
type Server struct {
	pb.UnimplementedAgentMessagingServer

	// Adapters for different platforms
	adapters map[string]adapter.Adapter
	mu       sync.RWMutex

	// Thread history store
	threadStore *store.ThreadHistoryStore

	// Agent config store
	agentConfigStore *store.AgentConfigStore

	// Conversation metadata cache
	conversationCache store.ConversationStore

	// Active streams (for bidirectional communication)
	streams   map[string]*conversationStream
	streamsMu sync.RWMutex

	// gRPC server instance
	grpcServer *grpc.Server
	listenAddr string
}

// conversationStream holds bidirectional stream state
type conversationStream struct {
	stream         pb.AgentMessaging_ProcessConversationServer
	conversationID string
	cancel         context.CancelFunc
}

// NewServer creates a new gRPC server
func NewServer(listenAddr string, threadStore *store.ThreadHistoryStore, convStore store.ConversationStore, agentConfigStore *store.AgentConfigStore) *Server {
	return &Server{
		adapters:          make(map[string]adapter.Adapter),
		threadStore:       threadStore,
		agentConfigStore:  agentConfigStore,
		conversationCache: convStore,
		streams:           make(map[string]*conversationStream),
		listenAddr:        listenAddr,
	}
}

// RegisterAdapter registers a platform adapter
func (s *Server) RegisterAdapter(name string, adpt adapter.Adapter) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.adapters[name] = adpt
	log.Printf("[gRPC] Registered adapter: %s", name)
}

// Start starts the gRPC server
func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.grpcServer = grpc.NewServer(
		grpc.MaxConcurrentStreams(100),
		grpc.MaxRecvMsgSize(4 * 1024 * 1024), // 4MB
		grpc.MaxSendMsgSize(4 * 1024 * 1024), // 4MB
	)

	pb.RegisterAgentMessagingServer(s.grpcServer, s)

	log.Printf("[gRPC] Server listening on %s", s.listenAddr)

	// Start server in goroutine
	go func() {
		if err := s.grpcServer.Serve(lis); err != nil {
			log.Printf("[gRPC] Server error: %v", err)
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()

	// Graceful shutdown
	log.Println("[gRPC] Shutting down server...")
	s.grpcServer.GracefulStop()

	return nil
}

// Stop stops the gRPC server
func (s *Server) Stop() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
}

// ProcessConversation handles bidirectional streaming
func (s *Server) ProcessConversation(stream pb.AgentMessaging_ProcessConversationServer) error {
	ctx := stream.Context()
	streamID := fmt.Sprintf("stream-%p", stream)

	log.Printf("[gRPC] New bidirectional stream: %s", streamID)

	// Wait for initial registration message from agent
	req, err := stream.Recv()
	if err != nil {
		log.Printf("[gRPC] Stream closed before registration: %v", err)
		return err
	}

	// Extract conversation ID from first message (agent should send a registration)
	var conversationID string
	switch payload := req.Request.(type) {
	case *pb.ConversationRequest_Message:
		conversationID = payload.Message.ConversationId
	case *pb.ConversationRequest_AgentConfig:
		// Agent sent config as first message; store it and wait for registration
		if s.agentConfigStore != nil {
			s.agentConfigStore.Set(payload.AgentConfig)
			log.Printf("[gRPC] Stored agent config from stream")
		}
		conversationID = "agent-stream"
	default:
		// For now, use a generic ID if no message provided
		conversationID = "agent-stream"
	}

	// Create stream context with cancellation
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register the stream
	s.streamsMu.Lock()
	s.streams[conversationID] = &conversationStream{
		stream:         stream,
		conversationID: conversationID,
		cancel:         cancel,
	}
	s.streamsMu.Unlock()

	log.Printf("[gRPC] Registered agent stream for conversation: %s", conversationID)

	// Clean up on exit
	defer func() {
		s.streamsMu.Lock()
		delete(s.streams, conversationID)
		s.streamsMu.Unlock()
		log.Printf("[gRPC] Unregistered agent stream: %s", conversationID)
	}()

	// Handle incoming requests from agent
	for {
		req, err := stream.Recv()
		if err != nil {
			log.Printf("[gRPC] Stream closed: %v", err)
			return err
		}

		switch payload := req.Request.(type) {
		case *pb.ConversationRequest_Message:
			// Agent sending a message — wrap as content and route through routeAgentResponse
			msg := payload.Message
			log.Printf("[gRPC] Agent message for conversation: %s", msg.ConversationId)

			response := &pb.AgentResponse{
				ConversationId: msg.ConversationId,
				ResponseId:     msg.Id,
				Payload: &pb.AgentResponse_Content{
					Content: &pb.ContentChunk{
						Type:    pb.ContentChunk_END,
						Content: msg.Content,
					},
				},
			}
			if err := s.routeAgentResponse(streamCtx, response); err != nil {
				log.Printf("[gRPC] Error routing agent message: %v", err)
			}

		case *pb.ConversationRequest_Feedback:
			// Agent acknowledging feedback
			log.Printf("[gRPC] Agent feedback: %s", payload.Feedback.ConversationId)

		case *pb.ConversationRequest_AgentConfig:
			// Agent sending/updating its config
			if s.agentConfigStore != nil {
				s.agentConfigStore.Set(payload.AgentConfig)
				log.Printf("[gRPC] Stored agent config from stream")
			}

		case *pb.ConversationRequest_AgentResponse:
			// Agent sending a typed response (ContentChunk, StatusUpdate, etc.)
			response := payload.AgentResponse
			log.Printf("[gRPC] Agent response for conversation: %s", response.ConversationId)
			if err := s.routeAgentResponse(streamCtx, response); err != nil {
				log.Printf("[gRPC] Error routing agent response: %v", err)
			}

		default:
			log.Printf("[gRPC] Unknown request type in stream")
		}

		// Check if context is cancelled
		select {
		case <-streamCtx.Done():
			return streamCtx.Err()
		default:
		}
	}
}

// ProcessMessage handles server-side streaming (message → stream of responses)
func (s *Server) ProcessMessage(req *pb.Message, stream pb.AgentMessaging_ProcessMessageServer) error {
	log.Printf("[gRPC] ProcessMessage: %s from %s", req.Id, req.Platform)
	return nil
}

// GetThreadHistory returns thread history from store
func (s *Server) GetThreadHistory(ctx context.Context, req *pb.ThreadHistoryRequest) (*pb.ThreadHistoryResponse, error) {
	log.Printf("[gRPC] GetThreadHistory: %s (max: %d)", req.ConversationId, req.MaxMessages)

	// Check if we need to hydrate from platform
	if s.threadStore.IsStale(req.ConversationId, s.getRefreshInterval()) {
		log.Printf("[gRPC] Thread history stale, hydrating from platform...")

		if err := s.hydrateThreadHistory(ctx, req.ConversationId); err != nil {
			log.Printf("[gRPC] Failed to hydrate: %v", err)
		}
	}

	maxMessages := int(req.MaxMessages)
	if maxMessages == 0 {
		maxMessages = 50
	}

	history := s.threadStore.GetHistory(req.ConversationId, maxMessages, req.IncludeDeleted)

	log.Printf("[gRPC] Returning %d messages for conversation %s", len(history.Messages), req.ConversationId)

	return history, nil
}

// GetConversationMetadata returns conversation metadata
func (s *Server) GetConversationMetadata(ctx context.Context, req *pb.ConversationMetadataRequest) (*pb.ConversationMetadataResponse, error) {
	var conversationID string

	switch id := req.Identifier.(type) {
	case *pb.ConversationMetadataRequest_ConversationId:
		conversationID = id.ConversationId

	case *pb.ConversationMetadataRequest_PlatformId:
		conversationID = buildConversationID(id.PlatformId.Platform, id.PlatformId.ChannelId, id.PlatformId.ThreadId)

	default:
		return &pb.ConversationMetadataResponse{Found: false}, nil
	}

	conv, err := s.conversationCache.Get(ctx, conversationID)
	if err != nil {
		return &pb.ConversationMetadataResponse{
			ConversationId: conversationID,
			Found:          false,
		}, nil
	}

	return &pb.ConversationMetadataResponse{
		ConversationId:  conversationID,
		Platform:        conv.Platform,
		ChannelId:       conv.ChannelID,
		ThreadId:        conv.ThreadID,
		LastMessageTime: timestamppb.New(conv.LastMessageAt),
		MessageCount:    int32(conv.MessageCount),
		Found:           true,
	}, nil
}

// HealthCheck returns server health status
func (s *Server) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	allHealthy := true
	for name, adpt := range s.adapters {
		if !adpt.IsHealthy(ctx) {
			log.Printf("[gRPC] Adapter unhealthy: %s", name)
			allHealthy = false
		}
	}

	status := pb.HealthCheckResponse_HEALTHY
	if !allHealthy {
		status = pb.HealthCheckResponse_DEGRADED
	}

	return &pb.HealthCheckResponse{
		Status:  status,
		Version: "1.0.0",
	}, nil
}

// HandleIncomingMessage is called by adapters when a message arrives from a platform.
// This routes the message to the agent via gRPC stream.
func (s *Server) HandleIncomingMessage(ctx context.Context, msg *pb.Message) error {
	log.Printf("[gRPC] Incoming platform message: %s from %s", msg.Id, msg.Platform)

	// Update conversation cache
	if err := s.updateConversationCache(ctx, msg); err != nil {
		log.Printf("[gRPC] Warning: failed to update conversation cache: %v", err)
	}

	// Find active agent stream
	s.streamsMu.RLock()
	var stream *conversationStream
	for _, s := range s.streams {
		stream = s
		break
	}
	s.streamsMu.RUnlock()

	if stream == nil {
		return fmt.Errorf("no active agent stream available")
	}

	// Send message to agent via stream wrapped in AgentResponse with incoming_message payload
	response := &pb.AgentResponse{
		ConversationId: msg.ConversationId,
		ResponseId:     msg.Id,
		Payload: &pb.AgentResponse_IncomingMessage{
			IncomingMessage: msg,
		},
	}

	if err := stream.stream.Send(response); err != nil {
		return fmt.Errorf("failed to send to agent: %w", err)
	}

	log.Printf("[gRPC] Message forwarded to agent via stream")
	return nil
}

// routeAgentResponse routes typed AgentResponse payloads back to the appropriate platform adapter
func (s *Server) routeAgentResponse(ctx context.Context, response *pb.AgentResponse) error {
	conversationID := response.ConversationId

	// Try to find the platform from conversation cache
	conv, err := s.conversationCache.Get(ctx, conversationID)
	if err == nil {
		// Found in cache — route to the specific adapter
		s.mu.RLock()
		adpt, exists := s.adapters[conv.Platform]
		s.mu.RUnlock()

		if exists {
			return adpt.HandleAgentResponse(ctx, response)
		}
	}

	// Conversation not in cache — broadcast to all adapters
	log.Printf("[gRPC] Conversation %s not in cache, broadcasting to all adapters", conversationID)
	s.mu.RLock()
	defer s.mu.RUnlock()

	for name, adpt := range s.adapters {
		if err := adpt.HandleAgentResponse(ctx, response); err != nil {
			log.Printf("[gRPC] Error routing agent response to %s: %v", name, err)
		}
	}

	return nil
}

// updateConversationCache updates the conversation metadata cache
func (s *Server) updateConversationCache(ctx context.Context, msg *pb.Message) error {
	conversationID := msg.ConversationId

	// Try to get existing conversation
	conv, err := s.conversationCache.Get(ctx, conversationID)
	if err != nil {
		// Create new conversation entry
		var channelID, threadID string
		if msg.PlatformContext != nil {
			channelID = msg.PlatformContext.ChannelId
			threadID = msg.PlatformContext.ThreadId
		}

		now := time.Now()
		return s.conversationCache.Create(ctx, &types.ConversationContext{
			ConversationID: conversationID,
			Platform:       msg.Platform,
			ChannelID:      channelID,
			ThreadID:       threadID,
			UserID:         msg.User.GetId(),
			CreatedAt:      now,
			LastMessageAt:  now,
			MessageCount:   1,
		})
	}

	// Update metadata
	conv.LastMessageAt = time.Now()
	conv.MessageCount++

	return s.conversationCache.Update(ctx, conv)
}

// SendToAgent sends a message to an agent stream
func (s *Server) SendToAgent(conversationID string, response *pb.AgentResponse) error {
	s.streamsMu.RLock()
	stream, exists := s.streams[conversationID]
	s.streamsMu.RUnlock()

	if !exists {
		return fmt.Errorf("no active stream for conversation: %s", conversationID)
	}

	return stream.stream.Send(response)
}

// hydrateThreadHistory fetches thread history from the appropriate adapter
func (s *Server) hydrateThreadHistory(ctx context.Context, conversationID string) error {
	conv, err := s.conversationCache.Get(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("conversation not found: %w", err)
	}

	s.mu.RLock()
	adpt, exists := s.adapters[conv.Platform]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("no adapter for platform: %s", conv.Platform)
	}

	return adpt.HydrateThread(ctx, conversationID, s.threadStore)
}

func (s *Server) getRefreshInterval() time.Duration {
	return 5 * time.Minute
}

// Helper function to build conversation ID
func buildConversationID(platform, channelID, threadID string) string {
	if threadID != "" {
		return fmt.Sprintf("%s-%s-%s", platform, channelID, threadID)
	}
	return fmt.Sprintf("%s-%s", platform, channelID)
}
