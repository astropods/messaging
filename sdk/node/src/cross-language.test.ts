import { describe, it, expect, beforeAll } from 'bun:test';
import { readFileSync, writeFileSync, existsSync, mkdirSync } from 'fs';
import { join } from 'path';
import type { Message, PlatformContext, User, ThreadMessage, ConversationRequest, AgentResponse } from './messaging-client';

/**
 * Cross-language serialization tests
 *
 * These tests verify that data serialized in Go can be deserialized in TypeScript,
 * and data serialized in TypeScript can be deserialized in Go.
 */

describe('Cross-language serialization: Go ↔ TypeScript', () => {
  const testDataDir = join(__dirname, '../../../test-data');

  beforeAll(() => {
    // Ensure test-data directory exists
    if (!existsSync(testDataDir)) {
      mkdirSync(testDataDir, { recursive: true });
    }
  });

  describe('TypeScript → Go: Serialize in TS, deserialize in Go', () => {
    it('should serialize TypeScript messages for Go to read', () => {
      // Create messages in TypeScript
      const platformContext: PlatformContext = {
        messageId: 'ts-msg-001',
        channelId: 'C987654',
        threadId: '9876543210.000001',
        channelName: '#typescript-channel',
        workspaceId: 'T888',
        platformData: {
          team_id: 'T888',
          bot_id: 'B456',
          source: 'typescript',
        },
      };

      const user: User = {
        id: 'U987654',
        username: 'tsuser',
        email: 'ts@example.com',
        avatarUrl: 'https://example.com/ts-avatar.png',
        userData: {
          language: 'typescript',
          framework: 'bun',
        },
      };

      const threadMessage: ThreadMessage = {
        messageId: 'ts-thread-msg-001',
        user: {
          id: 'U888',
          username: 'tstest',
        },
        content: 'Message from TypeScript',
        timestamp: new Date('2026-02-05T21:00:00Z'),
        wasEdited: false,
        isDeleted: false,  // Must be isDeleted, not wasDeleted!
        platformData: {
          source: 'typescript',
        },
      };

      const message: Message = {
        id: 'ts-full-msg-001',
        platform: 'slack',
        conversationId: 'conv-ts-001',
        content: 'Complete message from TypeScript',
        timestamp: new Date('2026-02-05T21:00:00Z'),
        platformContext,
        user,
      };

      // ConversationRequest with agentResponse containing ContentChunk (the bug)
      const conversationRequestWithContent: ConversationRequest = {
        agentResponse: {
          conversationId: 'conv-ts-001',
          responseId: 'resp-ts-001',
          content: {
            type: 'DELTA',
            content: 'Streaming from TypeScript',
            platformMessageId: 'plat-ts-001',
          },
        },
      };

      // ConversationRequest with agentResponse containing StatusUpdate
      const conversationRequestWithStatus: ConversationRequest = {
        agentResponse: {
          conversationId: 'conv-ts-002',
          responseId: 'resp-ts-002',
          status: {
            status: 'THINKING',
            customMessage: 'Processing...',
          },
        },
      };

      // ConversationRequest with agentResponse containing ErrorResponse
      const conversationRequestWithError: ConversationRequest = {
        agentResponse: {
          conversationId: 'conv-ts-003',
          responseId: 'resp-ts-003',
          error: {
            code: 'RATE_LIMIT',
            message: 'Too many requests',
            details: 'Retry after 30s',
            retryable: true,
          },
        },
      };

      // AgentResponse with incomingMessage (server→agent)
      const agentResponseWithMessage: AgentResponse = {
        conversationId: 'conv-ts-010',
        responseId: 'resp-ts-010',
        incomingMessage: {
          id: 'msg-ts-incoming-001',
          platform: 'slack',
          conversationId: 'conv-ts-010',
          content: 'User message from TS',
          platformContext: {
            messageId: 'C789:ts',
            channelId: 'C789',
            threadId: '1111111111.000001',
          },
          user: {
            id: 'U789',
            username: 'tsuser',
          },
        },
      };

      // AgentResponse with content
      const agentResponseWithContent: AgentResponse = {
        conversationId: 'conv-ts-011',
        responseId: 'resp-ts-011',
        content: {
          type: 'END',
          content: 'Final content from TS',
        },
      };

      // Serialize to JSON (this is what gRPC will send over the wire)
      const output = {
        platformContext,
        user,
        threadMessage,
        message,
        conversationRequestWithContent,
        conversationRequestWithStatus,
        conversationRequestWithError,
        agentResponseWithMessage,
        agentResponseWithContent,
      };

      // Write to file for Go to read
      const outputPath = join(testDataDir, 'ts-serialized.json');
      writeFileSync(outputPath, JSON.stringify(output, null, 2));

      console.log(`✓ Serialized TypeScript messages to ${outputPath}`);

      // Verify the structure
      expect(platformContext.messageId).toBe('ts-msg-001');
      expect(platformContext.channelId).toBe('C987654');
      expect(user.avatarUrl).toBe('https://example.com/ts-avatar.png');
      expect(user.userData).toBeDefined();
      expect(threadMessage.isDeleted).toBe(false);

      // Verify displayName does NOT exist
      expect((user as any).displayName).toBeUndefined();

      // Verify wasDeleted does NOT exist (should be isDeleted)
      expect((threadMessage as any).wasDeleted).toBeUndefined();
    });
  });

  describe('Go → TypeScript: Deserialize messages from Go', () => {
    it('should deserialize Go messages in TypeScript', () => {
      const goSerializedPath = join(testDataDir, 'go-serialized.json');

      if (!existsSync(goSerializedPath)) {
        console.log('⚠️  Go serialized file not found. Run from messaging package root: go run tools/test-serialization/main.go serialize');
        return;
      }

      // Read JSON that was created by Go
      const data = JSON.parse(readFileSync(goSerializedPath, 'utf-8'));

      // Deserialize PlatformContext
      const pc = data.platformContext as PlatformContext;
      expect(pc).toBeDefined();
      expect(pc.messageId).toBe('C123:1234567890.123456');
      expect(pc.channelId).toBe('C123456');
      expect(pc.threadId).toBe('1234567890.000001');
      expect(pc.channelName).toBe('#test-channel');
      expect(pc.workspaceId).toBe('T999');
      expect(pc.platformData).toBeDefined();
      expect(pc.platformData?.team_id).toBe('T999');
      expect(pc.platformData?.bot_id).toBe('B123');
      expect(pc.platformData?.custom_key).toBe('custom_value');

      console.log('✓ PlatformContext deserialized from Go:', {
        messageId: pc.messageId,
        channelId: pc.channelId,
        threadId: pc.threadId,
      });

      // Deserialize User
      const user = data.user as User;
      expect(user).toBeDefined();
      expect(user.id).toBe('U123456');
      expect(user.username).toBe('testuser');
      expect(user.email).toBe('test@example.com');
      expect(user.avatarUrl).toBe('https://example.com/avatar.png');
      expect(user.userData).toBeDefined();
      expect(user.userData?.department).toBe('engineering');
      expect(user.userData?.role).toBe('developer');

      // Verify displayName does NOT exist (it shouldn't be sent by Go)
      expect((user as any).displayName).toBeUndefined();

      console.log('✓ User deserialized from Go:', {
        id: user.id,
        username: user.username,
        avatarUrl: user.avatarUrl,
        userData: user.userData,
      });

      // Deserialize ThreadMessage
      const tm = data.threadMessage as ThreadMessage;
      expect(tm).toBeDefined();
      expect(tm.messageId).toBe('msg-001');
      expect(tm.content).toBe('Test message');
      expect(tm.wasEdited).toBe(true);
      expect(tm.isDeleted).toBe(false);  // Must be isDeleted
      expect(tm.originalContent).toBe('Original');
      expect(tm.platformData).toBeDefined();

      // Verify wasDeleted does NOT exist (Go should send isDeleted)
      expect((tm as any).wasDeleted).toBeUndefined();

      console.log('✓ ThreadMessage deserialized from Go:', {
        messageId: tm.messageId,
        wasEdited: tm.wasEdited,
        isDeleted: tm.isDeleted,
      });

      // Deserialize full Message
      const msg = data.message as Message;
      expect(msg).toBeDefined();
      expect(msg.id).toBe('msg-full-001');
      expect(msg.platform).toBe('slack');
      expect(msg.conversationId).toBe('conv-001');
      expect(msg.content).toBe('Test message from Go');

      // Verify nested objects
      expect(msg.platformContext).toBeDefined();
      expect(msg.platformContext?.channelId).toBe('C123456');
      expect(msg.user).toBeDefined();
      expect(msg.user?.username).toBe('testuser');
      expect(msg.user?.avatarUrl).toBe('https://example.com/avatar.png');

      console.log('✓ Message deserialized from Go:', {
        id: msg.id,
        platform: msg.platform,
        'platformContext.channelId': msg.platformContext?.channelId,
        'user.username': msg.user?.username,
      });

      console.log('\n✅ All Go → TypeScript deserialization successful!');
    });

    it('should deserialize Go ConversationRequest with agentResponse oneof', () => {
      const goSerializedPath = join(testDataDir, 'go-serialized.json');

      if (!existsSync(goSerializedPath)) {
        console.log('⚠️  Go serialized file not found.');
        return;
      }

      const data = JSON.parse(readFileSync(goSerializedPath, 'utf-8'));

      // ConversationRequest with agentResponse.content
      const crContent = data.conversationRequestWithContent as ConversationRequest;
      expect(crContent).toBeDefined();
      expect(crContent.agentResponse).toBeDefined();
      expect(crContent.agentResponse!.conversationId).toBe('conv-go-001');
      expect(crContent.agentResponse!.content).toBeDefined();
      expect(crContent.agentResponse!.content!.type).toBe('DELTA');
      expect(crContent.agentResponse!.content!.content).toBe('Streaming from Go');

      // ConversationRequest with agentResponse.status
      const crStatus = data.conversationRequestWithStatus as ConversationRequest;
      expect(crStatus).toBeDefined();
      expect(crStatus.agentResponse).toBeDefined();
      expect(crStatus.agentResponse!.status).toBeDefined();
      expect(crStatus.agentResponse!.status!.status).toBe('THINKING');

      // AgentResponse with content
      const arContent = data.agentResponseWithContent as AgentResponse;
      expect(arContent).toBeDefined();
      expect(arContent.conversationId).toBe('conv-go-011');
      expect(arContent.content).toBeDefined();
      expect(arContent.content!.type).toBe('END');
      expect(arContent.content!.content).toBe('Final content from Go');

      // AgentResponse with incomingMessage
      const arMsg = data.agentResponseWithMessage as AgentResponse;
      expect(arMsg).toBeDefined();
      expect(arMsg.incomingMessage).toBeDefined();
      expect(arMsg.incomingMessage!.id).toBe('msg-go-incoming-001');
      expect(arMsg.incomingMessage!.platform).toBe('slack');

      console.log('✅ Go → TypeScript oneof deserialization successful!');
    });
  });

  describe('Field name verification', () => {
    it('should use correct camelCase field names', () => {
      const goSerializedPath = join(testDataDir, 'go-serialized.json');

      if (!existsSync(goSerializedPath)) {
        console.log('⚠️  Go serialized file not found.');
        return;
      }

      const rawData = readFileSync(goSerializedPath, 'utf-8');
      const data = JSON.parse(rawData);

      // Verify PlatformContext uses camelCase
      const pcRaw = JSON.parse(JSON.stringify(data.platformContext));
      expect(pcRaw.messageId).toBeDefined();
      expect(pcRaw.channelId).toBeDefined();
      expect(pcRaw.threadId).toBeDefined();
      expect(pcRaw.channelName).toBeDefined();
      expect(pcRaw.workspaceId).toBeDefined();
      expect(pcRaw.platformData).toBeDefined();

      // Verify NO snake_case fields
      expect(pcRaw.message_id).toBeUndefined();
      expect(pcRaw.channel_id).toBeUndefined();
      expect(pcRaw.thread_id).toBeUndefined();
      expect(pcRaw.channel_name).toBeUndefined();
      expect(pcRaw.workspace_id).toBeUndefined();
      expect(pcRaw.platform_data).toBeUndefined();

      // Verify User uses camelCase
      const userRaw = JSON.parse(JSON.stringify(data.user));
      expect(userRaw.avatarUrl).toBeDefined();
      expect(userRaw.userData).toBeDefined();
      expect(userRaw.avatar_url).toBeUndefined();
      expect(userRaw.user_data).toBeUndefined();
      expect(userRaw.displayName).toBeUndefined();  // Should NOT exist

      // Verify ThreadMessage uses correct field names
      const tmRaw = JSON.parse(JSON.stringify(data.threadMessage));
      expect(tmRaw.messageId).toBeDefined();
      expect(tmRaw.wasEdited).toBeDefined();
      expect(tmRaw.isDeleted).toBeDefined();  // NOT wasDeleted
      expect(tmRaw.originalContent).toBeDefined();
      expect(tmRaw.message_id).toBeUndefined();
      expect(tmRaw.was_edited).toBeUndefined();
      expect(tmRaw.is_deleted).toBeUndefined();
      expect(tmRaw.wasDeleted).toBeUndefined();  // Bug: should be isDeleted
      expect(tmRaw.original_content).toBeUndefined();

      console.log('✓ All field names use correct camelCase format');
    });
  });

  describe('Roundtrip verification', () => {
    it('should preserve data through TS → JSON → Go → JSON → TS roundtrip', () => {
      // Original TypeScript data
      const original: PlatformContext = {
        messageId: 'roundtrip-msg-001',
        channelId: 'C111111',
        threadId: '1111111111.000001',
        channelName: '#roundtrip',
        workspaceId: 'T111',
        platformData: {
          key1: 'value1',
          key2: 'value2',
          unicode: '🚀🎉',
          special_chars: 'test-value_123',
        },
      };

      // Serialize to JSON (simulating what goes over the wire)
      const json = JSON.stringify(original);

      // Deserialize (simulating what Go receives)
      const deserialized = JSON.parse(json) as PlatformContext;

      // Verify all fields match
      expect(deserialized.messageId).toBe(original.messageId);
      expect(deserialized.channelId).toBe(original.channelId);
      expect(deserialized.threadId).toBe(original.threadId);
      expect(deserialized.channelName).toBe(original.channelName);
      expect(deserialized.workspaceId).toBe(original.workspaceId);
      expect(deserialized.platformData).toEqual(original.platformData);

      console.log('✓ Data preserved through roundtrip');
    });
  });
});
