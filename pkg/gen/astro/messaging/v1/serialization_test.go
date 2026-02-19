package messagingv1

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestPlatformContextJSONSerialization verifies that PlatformContext serializes to camelCase JSON
// This is critical for TypeScript interop since proto-loader with keepCase:false expects camelCase
func TestPlatformContextJSONSerialization(t *testing.T) {
	pc := &PlatformContext{
		MessageId:   "msg-001",
		ChannelId:   "C123456",
		ThreadId:    "1234567890.000001",
		ChannelName: "#general",
		WorkspaceId: "T123456",
		PlatformData: map[string]string{
			"team_id": "T123456",
			"bot_id":  "B123",
		},
	}

	// Serialize using protojson (what gRPC uses)
	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false, // Use JSON names (camelCase) instead of proto names (snake_case)
		EmitUnpopulated: false,
	}

	jsonBytes, err := marshaler.Marshal(pc)
	if err != nil {
		t.Fatalf("failed to marshal PlatformContext: %v", err)
	}

	// Parse the JSON to verify field names
	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	// Verify camelCase field names (TypeScript expects these)
	if _, ok := parsed["messageId"]; !ok {
		t.Errorf("expected 'messageId' field in JSON, got: %v", parsed)
	}
	if _, ok := parsed["channelId"]; !ok {
		t.Errorf("expected 'channelId' field in JSON, got: %v", parsed)
	}
	if _, ok := parsed["threadId"]; !ok {
		t.Errorf("expected 'threadId' field in JSON, got: %v", parsed)
	}
	if _, ok := parsed["channelName"]; !ok {
		t.Errorf("expected 'channelName' field in JSON, got: %v", parsed)
	}
	if _, ok := parsed["workspaceId"]; !ok {
		t.Errorf("expected 'workspaceId' field in JSON, got: %v", parsed)
	}
	if _, ok := parsed["platformData"]; !ok {
		t.Errorf("expected 'platformData' field in JSON, got: %v", parsed)
	}

	// Verify NO snake_case field names (these would break TypeScript)
	if _, ok := parsed["message_id"]; ok {
		t.Errorf("should NOT have snake_case 'message_id' field in JSON")
	}
	if _, ok := parsed["channel_id"]; ok {
		t.Errorf("should NOT have snake_case 'channel_id' field in JSON")
	}

	t.Logf("PlatformContext JSON: %s", string(jsonBytes))
}

// TestUserJSONSerialization verifies User serializes correctly
func TestUserJSONSerialization(t *testing.T) {
	user := &User{
		Id:        "U123456",
		Username:  "testuser",
		Email:     "test@example.com",
		AvatarUrl: "https://example.com/avatar.png",
		UserData: map[string]string{
			"department": "engineering",
			"role":       "developer",
		},
	}

	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: false,
	}

	jsonBytes, err := marshaler.Marshal(user)
	if err != nil {
		t.Fatalf("failed to marshal User: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	// Verify camelCase field names
	if _, ok := parsed["id"]; !ok {
		t.Errorf("expected 'id' field in JSON")
	}
	if _, ok := parsed["username"]; !ok {
		t.Errorf("expected 'username' field in JSON")
	}
	if _, ok := parsed["email"]; !ok {
		t.Errorf("expected 'email' field in JSON")
	}
	if _, ok := parsed["avatarUrl"]; !ok {
		t.Errorf("expected 'avatarUrl' field in JSON, got: %v", parsed)
	}
	if _, ok := parsed["userData"]; !ok {
		t.Errorf("expected 'userData' field in JSON")
	}

	// Verify NO snake_case
	if _, ok := parsed["avatar_url"]; ok {
		t.Errorf("should NOT have snake_case 'avatar_url' field")
	}
	if _, ok := parsed["user_data"]; ok {
		t.Errorf("should NOT have snake_case 'user_data' field")
	}

	// Verify displayName does NOT exist (it's not in the proto)
	if _, ok := parsed["displayName"]; ok {
		t.Errorf("should NOT have 'displayName' field (not in proto)")
	}

	t.Logf("User JSON: %s", string(jsonBytes))
}

// TestMessageJSONSerialization verifies Message serializes correctly
func TestMessageJSONSerialization(t *testing.T) {
	msg := &Message{
		Id:             "msg-001",
		Timestamp:      timestamppb.Now(),
		Platform:       "slack",
		ConversationId: "conv-001",
		Content:        "Hello world",
		PlatformContext: &PlatformContext{
			MessageId: "C123:1234567890.123456",
			ChannelId: "C123456",
			ThreadId:  "1234567890.000001",
		},
		User: &User{
			Id:       "U123",
			Username: "testuser",
		},
	}

	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: false,
	}

	jsonBytes, err := marshaler.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal Message: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	// Verify camelCase nested fields
	if _, ok := parsed["conversationId"]; !ok {
		t.Errorf("expected 'conversationId' field")
	}
	if _, ok := parsed["platformContext"]; !ok {
		t.Errorf("expected 'platformContext' field")
	}

	// Verify NO snake_case
	if _, ok := parsed["conversation_id"]; ok {
		t.Errorf("should NOT have snake_case 'conversation_id'")
	}
	if _, ok := parsed["platform_context"]; ok {
		t.Errorf("should NOT have snake_case 'platform_context'")
	}

	t.Logf("Message JSON: %s", string(jsonBytes))
}

// TestThreadMessageJSONSerialization verifies ThreadMessage field names
func TestThreadMessageJSONSerialization(t *testing.T) {
	tm := &ThreadMessage{
		MessageId: "msg-001",
		User: &User{
			Id:       "U123",
			Username: "test",
		},
		Content:         "Original content",
		Timestamp:       timestamppb.Now(),
		WasEdited:       true,
		IsDeleted:       false,
		OriginalContent: "Old content",
		EditedAt:        timestamppb.Now(),
		PlatformData: map[string]string{
			"key": "value",
		},
	}

	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: true, // Include false booleans
	}

	jsonBytes, err := marshaler.Marshal(tm)
	if err != nil {
		t.Fatalf("failed to marshal ThreadMessage: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	// Verify camelCase
	if _, ok := parsed["messageId"]; !ok {
		t.Errorf("expected 'messageId' field")
	}
	if _, ok := parsed["wasEdited"]; !ok {
		t.Errorf("expected 'wasEdited' field")
	}
	if _, ok := parsed["isDeleted"]; !ok {
		t.Errorf("expected 'isDeleted' field (not 'wasDeleted')")
	}
	if _, ok := parsed["originalContent"]; !ok {
		t.Errorf("expected 'originalContent' field")
	}
	if _, ok := parsed["editedAt"]; !ok {
		t.Errorf("expected 'editedAt' field")
	}
	if _, ok := parsed["platformData"]; !ok {
		t.Errorf("expected 'platformData' field")
	}

	// Verify incorrect field names do NOT exist
	if _, ok := parsed["wasDeleted"]; ok {
		t.Errorf("should NOT have 'wasDeleted' field (proto has 'is_deleted' → 'isDeleted')")
	}
	if _, ok := parsed["was_deleted"]; ok {
		t.Errorf("should NOT have snake_case 'was_deleted'")
	}

	t.Logf("ThreadMessage JSON: %s", string(jsonBytes))
}

// TestJSONRoundtrip verifies that we can serialize and deserialize without data loss
func TestPlatformContextJSONRoundtrip(t *testing.T) {
	original := &PlatformContext{
		MessageId:   "msg-roundtrip",
		ChannelId:   "C999",
		ThreadId:    "1234567890.000001",
		ChannelName: "#test",
		WorkspaceId: "T999",
		PlatformData: map[string]string{
			"key1": "value1",
			"key2": "value2",
			"unicode": "emoji-🚀-test",
		},
	}

	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: false,
	}

	// Serialize
	jsonBytes, err := marshaler.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Deserialize
	unmarshaler := protojson.UnmarshalOptions{
		DiscardUnknown: false,
	}

	deserialized := &PlatformContext{}
	if err := unmarshaler.Unmarshal(jsonBytes, deserialized); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Verify all fields match
	if deserialized.MessageId != original.MessageId {
		t.Errorf("MessageId mismatch: expected %q, got %q", original.MessageId, deserialized.MessageId)
	}
	if deserialized.ChannelId != original.ChannelId {
		t.Errorf("ChannelId mismatch: expected %q, got %q", original.ChannelId, deserialized.ChannelId)
	}
	if deserialized.ThreadId != original.ThreadId {
		t.Errorf("ThreadId mismatch: expected %q, got %q", original.ThreadId, deserialized.ThreadId)
	}
	if deserialized.ChannelName != original.ChannelName {
		t.Errorf("ChannelName mismatch: expected %q, got %q", original.ChannelName, deserialized.ChannelName)
	}
	if deserialized.WorkspaceId != original.WorkspaceId {
		t.Errorf("WorkspaceId mismatch: expected %q, got %q", original.WorkspaceId, deserialized.WorkspaceId)
	}

	// Verify map
	if len(deserialized.PlatformData) != len(original.PlatformData) {
		t.Errorf("PlatformData length mismatch: expected %d, got %d", len(original.PlatformData), len(deserialized.PlatformData))
	}
	for k, v := range original.PlatformData {
		if deserialized.PlatformData[k] != v {
			t.Errorf("PlatformData[%q] mismatch: expected %q, got %q", k, v, deserialized.PlatformData[k])
		}
	}

	t.Log("✓ PlatformContext roundtrip successful")
}

// TestTypeScriptCompatibility simulates what TypeScript proto-loader will do
func TestTypeScriptCompatibility(t *testing.T) {
	// Create a message with all fields populated
	msg := &Message{
		Id:             "msg-ts-compat",
		Platform:       "slack",
		ConversationId: "conv-ts-compat",
		Content:        "Test TS compatibility",
		PlatformContext: &PlatformContext{
			MessageId:   "C123:1234567890.123456",
			ChannelId:   "C123456",
			ThreadId:    "1234567890.000001",
			ChannelName: "#typescript",
			WorkspaceId: "T999",
			PlatformData: map[string]string{
				"team_id": "T999",
				"bot_id":  "B123",
			},
		},
		User: &User{
			Id:        "U123",
			Username:  "tsuser",
			Email:     "ts@example.com",
			AvatarUrl: "https://example.com/ts.png",
			UserData: map[string]string{
				"real_name": "TypeScript User",
			},
		},
		Timestamp: timestamppb.Now(),
	}

	// Serialize with options that match what gRPC uses
	marshaler := protojson.MarshalOptions{
		UseProtoNames:   false, // Use JSON names (camelCase)
		EmitUnpopulated: false,
	}

	jsonBytes, err := marshaler.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Parse as generic JSON (simulating what TypeScript will see)
	var tsView map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &tsView); err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	// Verify TypeScript can access all fields with expected names
	expectedFields := []string{
		"id",
		"platform",
		"conversationId",
		"content",
		"platformContext",
		"user",
		"timestamp",
	}

	for _, field := range expectedFields {
		if _, ok := tsView[field]; !ok {
			t.Errorf("TypeScript would not see field: %s\nAvailable fields: %v", field, getKeys(tsView))
		}
	}

	// Verify nested platformContext has camelCase fields
	if pcRaw, ok := tsView["platformContext"]; ok {
		pc := pcRaw.(map[string]interface{})
		expectedPCFields := []string{"messageId", "channelId", "threadId", "channelName", "workspaceId", "platformData"}
		for _, field := range expectedPCFields {
			if _, ok := pc[field]; !ok {
				t.Errorf("TypeScript would not see platformContext.%s", field)
			}
		}
	} else {
		t.Error("platformContext missing from JSON")
	}

	// Verify user has camelCase fields
	if userRaw, ok := tsView["user"]; ok {
		user := userRaw.(map[string]interface{})
		expectedUserFields := []string{"id", "username", "email", "avatarUrl", "userData"}
		for _, field := range expectedUserFields {
			if _, ok := user[field]; !ok {
				t.Errorf("TypeScript would not see user.%s", field)
			}
		}

		// Verify displayName does NOT exist
		if _, ok := user["displayName"]; ok {
			t.Error("TypeScript would see user.displayName but it's not in proto!")
		}
	} else {
		t.Error("user missing from JSON")
	}

	t.Logf("✓ TypeScript compatibility verified\nJSON: %s", string(jsonBytes))
}

func getKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
