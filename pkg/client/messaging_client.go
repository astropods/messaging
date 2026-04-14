package client

import (
	"context"
	"errors"
	"fmt"
	"io"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// MessagingClient is a client for the gRPC messaging service
type MessagingClient struct {
	conn   *grpc.ClientConn
	client pb.AgentMessagingClient
}

// NewClient creates a new messaging client
func NewClient(addr string) (*MessagingClient, error) {
	// Connect to gRPC server
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC server: %w", err)
	}

	client := pb.NewAgentMessagingClient(conn)

	return &MessagingClient{
		conn:   conn,
		client: client,
	}, nil
}

// Close closes the client connection
func (c *MessagingClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ProcessConversation creates a bidirectional stream for processing conversations
func (c *MessagingClient) ProcessConversation(ctx context.Context) (*ConversationStream, error) {
	stream, err := c.client.ProcessConversation(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create conversation stream: %w", err)
	}

	return &ConversationStream{
		stream: stream,
	}, nil
}

// ProcessMessage sends a single message and receives streaming responses
func (c *MessagingClient) ProcessMessage(ctx context.Context, msg *pb.Message) (*MessageStream, error) {
	stream, err := c.client.ProcessMessage(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	return &MessageStream{
		stream: stream,
	}, nil
}

// GetThreadHistory retrieves thread history for a conversation
func (c *MessagingClient) GetThreadHistory(ctx context.Context, conversationID string, maxMessages int) (*pb.ThreadHistoryResponse, error) {
	req := &pb.ThreadHistoryRequest{
		ConversationId: conversationID,
		MaxMessages:    int32(maxMessages), //nolint:gosec // G115: maxMessages is bounded by caller
		IncludeEdited:  true,
		IncludeDeleted: false,
	}

	resp, err := c.client.GetThreadHistory(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get thread history: %w", err)
	}

	return resp, nil
}

// GetConversationMetadata retrieves metadata for a conversation
func (c *MessagingClient) GetConversationMetadata(ctx context.Context, conversationID string) (*pb.ConversationMetadataResponse, error) {
	req := &pb.ConversationMetadataRequest{
		Identifier: &pb.ConversationMetadataRequest_ConversationId{
			ConversationId: conversationID,
		},
	}

	resp, err := c.client.GetConversationMetadata(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get conversation metadata: %w", err)
	}

	return resp, nil
}

// HealthCheck checks the health of the messaging service
func (c *MessagingClient) HealthCheck(ctx context.Context) (*pb.HealthCheckResponse, error) {
	req := &pb.HealthCheckRequest{}

	resp, err := c.client.HealthCheck(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}

	return resp, nil
}

// ConversationStream wraps the bidirectional stream for conversations
type ConversationStream struct {
	stream pb.AgentMessaging_ProcessConversationClient
}

// Send sends a conversation request (message or feedback)
func (s *ConversationStream) Send(req *pb.ConversationRequest) error {
	return s.stream.Send(req)
}

// SendMessage sends a message through the stream
func (s *ConversationStream) SendMessage(msg *pb.Message) error {
	return s.Send(&pb.ConversationRequest{
		Request: &pb.ConversationRequest_Message{
			Message: msg,
		},
	})
}

// SendFeedback sends platform feedback through the stream
func (s *ConversationStream) SendFeedback(feedback *pb.PlatformFeedback) error {
	return s.Send(&pb.ConversationRequest{
		Request: &pb.ConversationRequest_Feedback{
			Feedback: feedback,
		},
	})
}

// Receive receives an agent response from the stream
func (s *ConversationStream) Receive() (*pb.AgentResponse, error) {
	return s.stream.Recv()
}

// ReceiveAll receives all responses and processes them with a handler
func (s *ConversationStream) ReceiveAll(handler func(*pb.AgentResponse) error) error {
	for {
		resp, err := s.stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive error: %w", err)
		}

		if err := handler(resp); err != nil {
			return fmt.Errorf("handler error: %w", err)
		}
	}
}

// Close closes the stream
func (s *ConversationStream) Close() error {
	return s.stream.CloseSend()
}

// MessageStream wraps the server-side stream for single messages
type MessageStream struct {
	stream pb.AgentMessaging_ProcessMessageClient
}

// Receive receives an agent response from the stream
func (s *MessageStream) Receive() (*pb.AgentResponse, error) {
	return s.stream.Recv()
}

// ReceiveAll receives all responses and processes them with a handler
func (s *MessageStream) ReceiveAll(handler func(*pb.AgentResponse) error) error {
	for {
		resp, err := s.stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive error: %w", err)
		}

		if err := handler(resp); err != nil {
			return fmt.Errorf("handler error: %w", err)
		}
	}
}

// Helper function to create a message
func NewMessage(conversationID, userID, username, content string) *pb.Message {
	return &pb.Message{
		ConversationId: conversationID,
		User: &pb.User{
			Id:       userID,
			Username: username,
		},
		Content: content,
	}
}

// Helper function to create status update response
func NewStatusResponse(conversationID string, status pb.StatusUpdate_Status, message string) *pb.AgentResponse {
	return &pb.AgentResponse{
		ConversationId: conversationID,
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status:        status,
				CustomMessage: message,
			},
		},
	}
}

// Helper function to create content response
func NewContentResponse(conversationID, content string, final bool) *pb.AgentResponse {
	chunkType := pb.ContentChunk_START
	if final {
		chunkType = pb.ContentChunk_END
	}

	return &pb.AgentResponse{
		ConversationId: conversationID,
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    chunkType,
				Content: content,
			},
		},
	}
}

// Helper function to create error response
func NewErrorResponse(conversationID string, code pb.ErrorResponse_ErrorCode, message string) *pb.AgentResponse {
	return &pb.AgentResponse{
		ConversationId: conversationID,
		Payload: &pb.AgentResponse_Error{
			Error: &pb.ErrorResponse{
				Code:    code,
				Message: message,
			},
		},
	}
}

// Example usage:
//
// ```go
// client, err := NewClient("localhost:9090")
// if err != nil {
//     log.Fatal(err)
// }
// defer client.Close()
//
// // Get thread history
// history, err := client.GetThreadHistory(ctx, "slack-C123-1234.5678", 50)
// if err != nil {
//     log.Fatal(err)
// }
//
// // Process single message
// msg := NewMessage("slack-C123-1234.5678", "U123", "alice", "Hello!")
// stream, err := client.ProcessMessage(ctx, msg)
// if err != nil {
//     log.Fatal(err)
// }
//
// err = stream.ReceiveAll(func(resp *pb.AgentResponse) error {
//     switch payload := resp.Payload.(type) {
//     case *pb.AgentResponse_Status:
//         log.Printf("Status: %s", payload.Status.CustomMessage)
//     case *pb.AgentResponse_Content:
//         log.Printf("Content: %s", payload.Content.Content)
//     }
//     return nil
// })
// if err != nil {
//     log.Fatal(err)
// }
//
// // Bidirectional conversation
// convStream, err := client.ProcessConversation(ctx)
// if err != nil {
//     log.Fatal(err)
// }
// defer convStream.Close()
//
// // Send messages
// go func() {
//     convStream.SendMessage(msg)
// }()
//
// // Receive responses
// convStream.ReceiveAll(func(resp *pb.AgentResponse) error {
//     // Handle response
//     return nil
// })
// ```

