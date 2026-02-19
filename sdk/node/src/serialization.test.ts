import { describe, it, expect } from 'bun:test';
import * as protoLoader from '@grpc/proto-loader';
import { join } from 'path';

/**
 * Serialization tests to verify Go ↔ TypeScript compatibility
 *
 * These tests verify that the proto-loader configuration correctly
 * converts snake_case proto fields to camelCase JavaScript fields,
 * ensuring compatibility with Go's JSON serialization.
 */

describe('Proto field name serialization', () => {
  let packageDefinition: protoLoader.PackageDefinition;

  // Load proto definitions once
  const protoPath = 'astro/messaging/v1/service.proto';
  const protoRoot = join(__dirname, '../proto');

  packageDefinition = protoLoader.loadSync(protoPath, {
    keepCase: false,  // Convert snake_case to camelCase
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
    includeDirs: [protoRoot],
  });

  function getFieldNames(typeDef: any): string[] {
    const fields = typeDef?.type?.field ?? [];
    return Object.values(fields).map((f: any) => f.name);
  }

  describe('PlatformContext fields', () => {
    it('should have camelCase field names', () => {
      const pcType = packageDefinition['astro.messaging.v1.PlatformContext'];
      expect(pcType).toBeDefined();

      const fieldNames = getFieldNames(pcType);

      // These are what TypeScript expects (camelCase)
      expect(fieldNames).toContain('messageId');
      expect(fieldNames).toContain('channelId');
      expect(fieldNames).toContain('threadId');
      expect(fieldNames).toContain('channelName');
      expect(fieldNames).toContain('workspaceId');
      expect(fieldNames).toContain('platformData');

      // These should NOT exist (snake_case would break TS interop)
      expect(fieldNames).not.toContain('message_id');
      expect(fieldNames).not.toContain('channel_id');
      expect(fieldNames).not.toContain('thread_id');
      expect(fieldNames).not.toContain('channel_name');
      expect(fieldNames).not.toContain('workspace_id');
      expect(fieldNames).not.toContain('platform_data');
    });

    it('should match TypeScript interface field names', () => {
      const pcType = packageDefinition['astro.messaging.v1.PlatformContext'];
      const protoFields = getFieldNames(pcType);

      // These match the TypeScript PlatformContext interface
      const expectedFields = [
        'messageId',
        'channelId',
        'threadId',
        'channelName',
        'workspaceId',
        'platformData',
      ];

      for (const field of expectedFields) {
        expect(protoFields).toContain(field);
      }
    });
  });

  describe('User fields', () => {
    it('should have camelCase field names', () => {
      const userType = packageDefinition['astro.messaging.v1.User'];
      expect(userType).toBeDefined();

      const fieldNames = getFieldNames(userType);

      // Correct camelCase names
      expect(fieldNames).toContain('id');
      expect(fieldNames).toContain('username');
      expect(fieldNames).toContain('avatarUrl');
      expect(fieldNames).toContain('email');
      expect(fieldNames).toContain('userData');

      // Should NOT have snake_case
      expect(fieldNames).not.toContain('avatar_url');
      expect(fieldNames).not.toContain('user_data');
    });

    it('should NOT have displayName field', () => {
      const userType = packageDefinition['astro.messaging.v1.User'];
      const fieldNames = getFieldNames(userType);

      // displayName is not in the proto definition
      expect(fieldNames).not.toContain('displayName');
      expect(fieldNames).not.toContain('display_name');
    });

    it('should match TypeScript interface (without displayName)', () => {
      const userType = packageDefinition['astro.messaging.v1.User'];
      const protoFields = getFieldNames(userType);

      // These match the corrected TypeScript User interface
      const expectedFields = [
        'id',
        'username',
        'email',
        'avatarUrl',
        'userData',
      ];

      for (const field of expectedFields) {
        expect(protoFields).toContain(field);
      }

      // This should NOT exist
      expect(protoFields).not.toContain('displayName');
    });
  });

  describe('Message fields', () => {
    it('should have camelCase nested fields', () => {
      const msgType = packageDefinition['astro.messaging.v1.Message'];
      expect(msgType).toBeDefined();

      const fieldNames = getFieldNames(msgType);

      expect(fieldNames).toContain('platformContext');
      expect(fieldNames).toContain('conversationId');
      expect(fieldNames).toContain('content');
      expect(fieldNames).toContain('user');
      expect(fieldNames).toContain('attachments');

      // Should NOT have snake_case
      expect(fieldNames).not.toContain('platform_context');
      expect(fieldNames).not.toContain('conversation_id');
    });
  });

  describe('ThreadMessage fields', () => {
    it('should have isDeleted (not wasDeleted)', () => {
      const tmType = packageDefinition['astro.messaging.v1.ThreadMessage'];
      expect(tmType).toBeDefined();

      const fieldNames = getFieldNames(tmType);

      // Correct field names from proto
      expect(fieldNames).toContain('messageId');
      expect(fieldNames).toContain('wasEdited');
      expect(fieldNames).toContain('isDeleted');  // proto: is_deleted
      expect(fieldNames).toContain('originalContent');
      expect(fieldNames).toContain('editedAt');
      expect(fieldNames).toContain('deletedAt');
      expect(fieldNames).toContain('platformData');

      // Should NOT have incorrect names
      expect(fieldNames).not.toContain('wasDeleted');  // This was the bug!
      expect(fieldNames).not.toContain('was_deleted');
      expect(fieldNames).not.toContain('is_deleted');
    });

    it('should match TypeScript ThreadMessage interface', () => {
      const tmType = packageDefinition['astro.messaging.v1.ThreadMessage'];
      const protoFields = getFieldNames(tmType);

      // These match the corrected TypeScript interface
      const expectedFields = [
        'messageId',
        'user',
        'content',
        'attachments',
        'timestamp',
        'wasEdited',
        'isDeleted',      // NOT wasDeleted
        'originalContent',
        'editedAt',
        'deletedAt',
        'platformData',
      ];

      for (const field of expectedFields) {
        expect(protoFields).toContain(field);
      }
    });
  });

  describe('ThreadHistoryResponse fields', () => {
    it('should have isComplete (not is_complete)', () => {
      const thrType = packageDefinition['astro.messaging.v1.ThreadHistoryResponse'];
      expect(thrType).toBeDefined();

      const fieldNames = getFieldNames(thrType);

      expect(fieldNames).toContain('conversationId');
      expect(fieldNames).toContain('messages');
      expect(fieldNames).toContain('isComplete');
      expect(fieldNames).toContain('fetchedAt');

      // Should NOT have snake_case
      expect(fieldNames).not.toContain('conversation_id');
      expect(fieldNames).not.toContain('is_complete');
      expect(fieldNames).not.toContain('fetched_at');
    });
  });

  describe('AgentResponse fields', () => {
    it('should have camelCase oneof fields', () => {
      const respType = packageDefinition['astro.messaging.v1.AgentResponse'];
      expect(respType).toBeDefined();

      const fieldNames = getFieldNames(respType);

      expect(fieldNames).toContain('conversationId');
      expect(fieldNames).toContain('responseId');
      expect(fieldNames).toContain('incomingMessage');
      expect(fieldNames).toContain('status');
      expect(fieldNames).toContain('content');
      expect(fieldNames).toContain('prompts');
      expect(fieldNames).toContain('threadMetadata');
      expect(fieldNames).toContain('error');
      expect(fieldNames).toContain('contextRequest');

      // Should NOT have snake_case
      expect(fieldNames).not.toContain('conversation_id');
      expect(fieldNames).not.toContain('response_id');
      expect(fieldNames).not.toContain('incoming_message');
      expect(fieldNames).not.toContain('thread_metadata');
      expect(fieldNames).not.toContain('context_request');
    });
  });

  describe('ContentChunk fields', () => {
    it('should have platformMessageId (not platform_message_id)', () => {
      const ccType = packageDefinition['astro.messaging.v1.ContentChunk'];
      expect(ccType).toBeDefined();

      const fieldNames = getFieldNames(ccType);

      expect(fieldNames).toContain('type');
      expect(fieldNames).toContain('content');
      expect(fieldNames).toContain('attachments');
      expect(fieldNames).toContain('platformMessageId');
      expect(fieldNames).toContain('options');

      // Should NOT have snake_case
      expect(fieldNames).not.toContain('platform_message_id');
    });
  });

  describe('Complete serialization verification', () => {
    it('should verify all critical fields use camelCase', () => {
      // This test ensures that ALL fields that could cause serialization issues
      // are correctly using camelCase

      const criticalTypes = [
        'astro.messaging.v1.Message',
        'astro.messaging.v1.PlatformContext',
        'astro.messaging.v1.User',
        'astro.messaging.v1.AgentResponse',
        'astro.messaging.v1.ThreadMessage',
        'astro.messaging.v1.ThreadHistoryResponse',
        'astro.messaging.v1.ContentChunk',
      ];

      for (const typeName of criticalTypes) {
        const typeDef = packageDefinition[typeName];
        expect(typeDef).toBeDefined();

        const fieldNames = getFieldNames(typeDef);

        // Verify NO field has underscore (all should be camelCase)
        for (const fieldName of fieldNames) {
          if (fieldName.includes('_')) {
            throw new Error(`Field '${fieldName}' in ${typeName} contains underscore - should be camelCase!`);
          }
        }
      }
    });

    it('should confirm keepCase: false is working correctly', () => {
      // With keepCase: false, proto field names like "message_id" become "messageId"
      // This is essential for Go ↔ TypeScript interop

      const pcType = packageDefinition['astro.messaging.v1.PlatformContext'];
      const fieldNames = getFieldNames(pcType);

      // All these proto fields have underscores and should be converted
      const conversions = [
        { proto: 'message_id', js: 'messageId' },
        { proto: 'channel_id', js: 'channelId' },
        { proto: 'thread_id', js: 'threadId' },
        { proto: 'channel_name', js: 'channelName' },
        { proto: 'workspace_id', js: 'workspaceId' },
        { proto: 'platform_data', js: 'platformData' },
      ];

      for (const { proto, js } of conversions) {
        expect(fieldNames).toContain(js);
        expect(fieldNames).not.toContain(proto);
      }
    });
  });

  describe('Regression tests for specific bugs', () => {
    it('User should not have displayName field', () => {
      // BUG: TypeScript interface had displayName but proto doesn't
      const userType = packageDefinition['astro.messaging.v1.User'];
      const fieldNames = getFieldNames(userType);

      expect(fieldNames).not.toContain('displayName');
    });

    it('ThreadMessage should have isDeleted not wasDeleted', () => {
      // BUG: TypeScript interface had wasDeleted but proto has is_deleted
      const tmType = packageDefinition['astro.messaging.v1.ThreadMessage'];
      const fieldNames = getFieldNames(tmType);

      expect(fieldNames).toContain('isDeleted');
      expect(fieldNames).not.toContain('wasDeleted');
    });

    it('User should have userData map field', () => {
      // This field exists in proto but may have been missing from TS interface
      const userType = packageDefinition['astro.messaging.v1.User'];
      const fieldNames = getFieldNames(userType);

      expect(fieldNames).toContain('userData');
      expect(fieldNames).not.toContain('user_data');
    });

    it('PlatformContext should have platformData map field', () => {
      const pcType = packageDefinition['astro.messaging.v1.PlatformContext'];
      const fieldNames = getFieldNames(pcType);

      expect(fieldNames).toContain('platformData');
      expect(fieldNames).not.toContain('platform_data');
    });
  });
});
