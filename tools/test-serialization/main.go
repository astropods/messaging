package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go [serialize|deserialize]")
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "serialize":
		serializeFromGo()
	case "deserialize":
		deserializeFromTS()
	default:
		fmt.Printf("Unknown command: %s\n", command)
		os.Exit(1)
	}
}

// serializeFromGo creates protobuf messages in Go and serializes them to JSON
func serializeFromGo() {
	// Create PlatformContext
	pc := &pb.PlatformContext{
		MessageId:   "C123:1234567890.123456",
		ChannelId:   "C123456",
		ThreadId:    "1234567890.000001",
		ChannelName: "#test-channel",
		WorkspaceId: "T999",
		PlatformData: map[string]string{
			"team_id":    "T999",
			"bot_id":     "B123",
			"custom_key": "custom_value",
		},
	}

	// Create User
	user := &pb.User{
		Id:        "U123456",
		Username:  "testuser",
		Email:     "test@example.com",
		AvatarUrl: "https://example.com/avatar.png",
		UserData: map[string]string{
			"department": "engineering",
			"role":       "developer",
		},
	}

	// Create ThreadMessage
	now := time.Date(2026, 2, 5, 21, 0, 0, 0, time.UTC)
	tm := &pb.ThreadMessage{
		MessageId: "msg-001",
		User: &pb.User{
			Id:       "U999",
			Username: "test",
		},
		Content:         "Test message",
		Timestamp:       timestamppb.New(now),
		WasEdited:       true,
		IsDeleted:       false,
		OriginalContent: "Original",
		PlatformData: map[string]string{
			"key": "value",
		},
	}

	// Create Message with all fields
	msg := &pb.Message{
		Id:             "msg-full-001",
		Platform:       "slack",
		ConversationId: "conv-001",
		Content:        "Test message from Go",
		Timestamp:      timestamppb.New(now),
		PlatformContext: pc,
		User:           user,
	}

	// Serialize using protojson (what gRPC uses)
	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false, // Use JSON names (camelCase)
		EmitUnpopulated: true,  // Include fields with default values (for testing)
		Indent:          "  ",
	}

	// ConversationRequest with agentResponse containing ContentChunk
	crWithContent := &pb.ConversationRequest{
		Request: &pb.ConversationRequest_AgentResponse{
			AgentResponse: &pb.AgentResponse{
				ConversationId: "conv-go-001",
				ResponseId:     "resp-go-001",
				Payload: &pb.AgentResponse_Content{
					Content: &pb.ContentChunk{
						Type:              pb.ContentChunk_DELTA,
						Content:           "Streaming from Go",
						PlatformMessageId: "plat-go-001",
					},
				},
			},
		},
	}

	// ConversationRequest with agentResponse containing StatusUpdate
	crWithStatus := &pb.ConversationRequest{
		Request: &pb.ConversationRequest_AgentResponse{
			AgentResponse: &pb.AgentResponse{
				ConversationId: "conv-go-002",
				ResponseId:     "resp-go-002",
				Payload: &pb.AgentResponse_Status{
					Status: &pb.StatusUpdate{
						Status:        pb.StatusUpdate_THINKING,
						CustomMessage: "Processing in Go...",
					},
				},
			},
		},
	}

	// AgentResponse with incomingMessage
	arWithMessage := &pb.AgentResponse{
		ConversationId: "conv-go-010",
		ResponseId:     "resp-go-010",
		Payload: &pb.AgentResponse_IncomingMessage{
			IncomingMessage: &pb.Message{
				Id:             "msg-go-incoming-001",
				Platform:       "slack",
				ConversationId: "conv-go-010",
				Content:        "User message from Go",
				PlatformContext: &pb.PlatformContext{
					MessageId: "C999:go",
					ChannelId: "C999",
				},
				User: &pb.User{
					Id:       "U999",
					Username: "gouser",
				},
			},
		},
	}

	// AgentResponse with content
	arWithContent := &pb.AgentResponse{
		ConversationId: "conv-go-011",
		ResponseId:     "resp-go-011",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "Final content from Go",
			},
		},
	}

	// Serialize each type
	pcJSON, _ := marshaler.Marshal(pc)
	userJSON, _ := marshaler.Marshal(user)
	tmJSON, _ := marshaler.Marshal(tm)
	msgJSON, _ := marshaler.Marshal(msg)
	crContentJSON, _ := marshaler.Marshal(crWithContent)
	crStatusJSON, _ := marshaler.Marshal(crWithStatus)
	arMessageJSON, _ := marshaler.Marshal(arWithMessage)
	arContentJSON, _ := marshaler.Marshal(arWithContent)

	// Create output structure
	output := map[string]json.RawMessage{
		"platformContext":                pcJSON,
		"user":                           userJSON,
		"threadMessage":                  tmJSON,
		"message":                        msgJSON,
		"conversationRequestWithContent": crContentJSON,
		"conversationRequestWithStatus":  crStatusJSON,
		"agentResponseWithMessage":       arMessageJSON,
		"agentResponseWithContent":       arContentJSON,
	}

	// Write to file
	outputJSON, _ := json.MarshalIndent(output, "", "  ")
	if err := os.WriteFile("test-data/go-serialized.json", outputJSON, 0644); err != nil {
		fmt.Printf("Error writing file: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✓ Serialized messages from Go to test-data/go-serialized.json")
	fmt.Printf("PlatformContext JSON:\n%s\n\n", string(pcJSON))
	fmt.Printf("User JSON:\n%s\n\n", string(userJSON))
	fmt.Printf("ThreadMessage JSON:\n%s\n\n", string(tmJSON))
	fmt.Printf("Message JSON:\n%s\n\n", string(msgJSON))
}

// deserializeFromTS reads JSON that was created by TypeScript and deserializes it in Go
func deserializeFromTS() {
	data, err := os.ReadFile("test-data/ts-serialized.json")
	if err != nil {
		fmt.Printf("Error reading file: %v\n", err)
		fmt.Println("Run the TypeScript serialization test first to generate ts-serialized.json")
		os.Exit(1)
	}

	var input map[string]json.RawMessage
	if err := json.Unmarshal(data, &input); err != nil {
		fmt.Printf("Error parsing JSON: %v\n", err)
		os.Exit(1)
	}

	unmarshaler := protojson.UnmarshalOptions{
		DiscardUnknown: false,
	}

	// Deserialize PlatformContext
	if pcJSON, ok := input["platformContext"]; ok {
		pc := &pb.PlatformContext{}
		if err := unmarshaler.Unmarshal(pcJSON, pc); err != nil {
			fmt.Printf("❌ Failed to deserialize PlatformContext: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized PlatformContext from TypeScript:")
		fmt.Printf("  messageId: %s\n", pc.MessageId)
		fmt.Printf("  channelId: %s\n", pc.ChannelId)
		fmt.Printf("  threadId: %s\n", pc.ThreadId)
		fmt.Printf("  channelName: %s\n", pc.ChannelName)
		fmt.Printf("  workspaceId: %s\n", pc.WorkspaceId)
		fmt.Printf("  platformData: %v\n\n", pc.PlatformData)
	}

	// Deserialize User
	if userJSON, ok := input["user"]; ok {
		user := &pb.User{}
		if err := unmarshaler.Unmarshal(userJSON, user); err != nil {
			fmt.Printf("❌ Failed to deserialize User: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized User from TypeScript:")
		fmt.Printf("  id: %s\n", user.Id)
		fmt.Printf("  username: %s\n", user.Username)
		fmt.Printf("  email: %s\n", user.Email)
		fmt.Printf("  avatarUrl: %s\n", user.AvatarUrl)
		fmt.Printf("  userData: %v\n\n", user.UserData)

		// Verify no displayName field was received (it shouldn't exist)
		var rawUser map[string]interface{}
		json.Unmarshal(userJSON, &rawUser)
		if _, hasDisplayName := rawUser["displayName"]; hasDisplayName {
			fmt.Println("❌ ERROR: User has displayName field (shouldn't exist in proto!)")
			os.Exit(1)
		}
	}

	// Deserialize ThreadMessage
	if tmJSON, ok := input["threadMessage"]; ok {
		tm := &pb.ThreadMessage{}
		if err := unmarshaler.Unmarshal(tmJSON, tm); err != nil {
			fmt.Printf("❌ Failed to deserialize ThreadMessage: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized ThreadMessage from TypeScript:")
		fmt.Printf("  messageId: %s\n", tm.MessageId)
		fmt.Printf("  wasEdited: %v\n", tm.WasEdited)
		fmt.Printf("  isDeleted: %v\n", tm.IsDeleted)
		fmt.Printf("  content: %s\n\n", tm.Content)

		// Verify correct field names
		var rawTM map[string]interface{}
		json.Unmarshal(tmJSON, &rawTM)
		if _, hasWasDeleted := rawTM["wasDeleted"]; hasWasDeleted {
			fmt.Println("❌ ERROR: ThreadMessage has wasDeleted field (should be isDeleted!)")
			os.Exit(1)
		}
	}

	// Deserialize full Message
	if msgJSON, ok := input["message"]; ok {
		msg := &pb.Message{}
		if err := unmarshaler.Unmarshal(msgJSON, msg); err != nil {
			fmt.Printf("❌ Failed to deserialize Message: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized Message from TypeScript:")
		fmt.Printf("  id: %s\n", msg.Id)
		fmt.Printf("  platform: %s\n", msg.Platform)
		fmt.Printf("  conversationId: %s\n", msg.ConversationId)
		fmt.Printf("  content: %s\n", msg.Content)
		if msg.PlatformContext != nil {
			fmt.Printf("  platformContext.channelId: %s\n", msg.PlatformContext.ChannelId)
		}
		if msg.User != nil {
			fmt.Printf("  user.username: %s\n", msg.User.Username)
		}
		fmt.Println()
	}

	// Deserialize ConversationRequest with agentResponse.content
	if crJSON, ok := input["conversationRequestWithContent"]; ok {
		cr := &pb.ConversationRequest{}
		if err := unmarshaler.Unmarshal(crJSON, cr); err != nil {
			fmt.Printf("❌ Failed to deserialize ConversationRequest (content): %v\n", err)
			os.Exit(1)
		}
		ar := cr.GetAgentResponse()
		if ar == nil {
			fmt.Println("❌ ERROR: ConversationRequest.agentResponse is nil (oneof not set!)")
			os.Exit(1)
		}
		if ar.GetContent() == nil {
			fmt.Println("❌ ERROR: AgentResponse.content is nil (payload oneof not set!)")
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized ConversationRequest with agentResponse.content from TypeScript:")
		fmt.Printf("  conversationId: %s\n", ar.ConversationId)
		fmt.Printf("  content.type: %v\n", ar.GetContent().Type)
		fmt.Printf("  content.content: %s\n\n", ar.GetContent().Content)
	}

	// Deserialize ConversationRequest with agentResponse.status
	if crJSON, ok := input["conversationRequestWithStatus"]; ok {
		cr := &pb.ConversationRequest{}
		if err := unmarshaler.Unmarshal(crJSON, cr); err != nil {
			fmt.Printf("❌ Failed to deserialize ConversationRequest (status): %v\n", err)
			os.Exit(1)
		}
		ar := cr.GetAgentResponse()
		if ar == nil {
			fmt.Println("❌ ERROR: ConversationRequest.agentResponse is nil!")
			os.Exit(1)
		}
		if ar.GetStatus() == nil {
			fmt.Println("❌ ERROR: AgentResponse.status is nil (payload oneof not set!)")
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized ConversationRequest with agentResponse.status from TypeScript:")
		fmt.Printf("  status: %v\n\n", ar.GetStatus().Status)
	}

	// Deserialize ConversationRequest with agentResponse.error
	if crJSON, ok := input["conversationRequestWithError"]; ok {
		cr := &pb.ConversationRequest{}
		if err := unmarshaler.Unmarshal(crJSON, cr); err != nil {
			fmt.Printf("❌ Failed to deserialize ConversationRequest (error): %v\n", err)
			os.Exit(1)
		}
		ar := cr.GetAgentResponse()
		if ar == nil {
			fmt.Println("❌ ERROR: ConversationRequest.agentResponse is nil!")
			os.Exit(1)
		}
		if ar.GetError() == nil {
			fmt.Println("❌ ERROR: AgentResponse.error is nil (payload oneof not set!)")
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized ConversationRequest with agentResponse.error from TypeScript:")
		fmt.Printf("  error.code: %v\n", ar.GetError().Code)
		fmt.Printf("  error.message: %s\n\n", ar.GetError().Message)
	}

	// Deserialize AgentResponse with incomingMessage
	if arJSON, ok := input["agentResponseWithMessage"]; ok {
		ar := &pb.AgentResponse{}
		if err := unmarshaler.Unmarshal(arJSON, ar); err != nil {
			fmt.Printf("❌ Failed to deserialize AgentResponse (incomingMessage): %v\n", err)
			os.Exit(1)
		}
		if ar.GetIncomingMessage() == nil {
			fmt.Println("❌ ERROR: AgentResponse.incomingMessage is nil!")
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized AgentResponse with incomingMessage from TypeScript:")
		fmt.Printf("  message.id: %s\n", ar.GetIncomingMessage().Id)
		fmt.Printf("  message.platform: %s\n\n", ar.GetIncomingMessage().Platform)
	}

	// Deserialize AgentResponse with content
	if arJSON, ok := input["agentResponseWithContent"]; ok {
		ar := &pb.AgentResponse{}
		if err := unmarshaler.Unmarshal(arJSON, ar); err != nil {
			fmt.Printf("❌ Failed to deserialize AgentResponse (content): %v\n", err)
			os.Exit(1)
		}
		if ar.GetContent() == nil {
			fmt.Println("❌ ERROR: AgentResponse.content is nil!")
			os.Exit(1)
		}
		fmt.Println("✓ Deserialized AgentResponse with content from TypeScript:")
		fmt.Printf("  content.type: %v\n", ar.GetContent().Type)
		fmt.Printf("  content.content: %s\n\n", ar.GetContent().Content)
	}

	fmt.Println("✓ All TypeScript → Go deserialization successful!")
}
