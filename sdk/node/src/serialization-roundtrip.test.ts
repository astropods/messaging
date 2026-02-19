import { describe, it, expect } from 'bun:test';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { join } from 'path';

/**
 * Proto-loader binary roundtrip tests
 *
 * These tests use the actual protobuf binary serializers exposed by @grpc/grpc-js
 * to verify that oneof fields survive a serialize → deserialize roundtrip.
 * This catches the exact class of bug where TS sends `{ payload: { content: {...} } }`
 * but proto-loader expects oneof fields flattened: `{ content: {...} }`.
 */

const protoPath = 'astro/messaging/v1/service.proto';
const protoRoot = join(__dirname, '../proto');

const packageDefinition = protoLoader.loadSync(protoPath, {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: true,
  oneofs: true,
  includeDirs: [protoRoot],
});

const protoDescriptor = grpc.loadPackageDefinition(packageDefinition) as any;
const AgentMessaging = protoDescriptor.astro.messaging.v1.AgentMessaging;

// Extract binary serializers from the ProcessConversation RPC method
const processConversation = AgentMessaging.service.ProcessConversation;
const serializeRequest: (obj: any) => Buffer = processConversation.requestSerialize;
const deserializeRequest: (buf: Buffer) => any = processConversation.requestDeserialize;
const serializeResponse: (obj: any) => Buffer = processConversation.responseSerialize;
const deserializeResponse: (buf: Buffer) => any = processConversation.responseDeserialize;

describe('Proto-loader binary roundtrip: ConversationRequest oneof', () => {
  it('should roundtrip agentResponse with content (ContentChunk DELTA) — the exact bug', () => {
    const request = {
      agentResponse: {
        conversationId: 'conv-001',
        responseId: 'resp-001',
        content: {
          type: 'DELTA',
          content: 'Hello world',
          platformMessageId: 'plat-msg-001',
        },
      },
    };

    const buf = serializeRequest(request);
    const result = deserializeRequest(buf);

    // The oneof "request" should resolve to agentResponse
    expect(result.request).toBe('agentResponse');
    expect(result.agentResponse).toBeDefined();

    // The oneof "payload" should resolve to content (flattened, not nested under payload)
    expect(result.agentResponse.payload).toBe('content');
    expect(result.agentResponse.content).toBeDefined();
    expect(result.agentResponse.content.type).toBe('DELTA');
    expect(result.agentResponse.content.content).toBe('Hello world');
    expect(result.agentResponse.content.platformMessageId).toBe('plat-msg-001');
    expect(result.agentResponse.conversationId).toBe('conv-001');
    expect(result.agentResponse.responseId).toBe('resp-001');
  });

  it('should roundtrip agentResponse with content (ContentChunk END)', () => {
    const request = {
      agentResponse: {
        conversationId: 'conv-002',
        responseId: 'resp-002',
        content: {
          type: 'END',
          content: 'Final message',
        },
      },
    };

    const buf = serializeRequest(request);
    const result = deserializeRequest(buf);

    expect(result.request).toBe('agentResponse');
    expect(result.agentResponse.payload).toBe('content');
    expect(result.agentResponse.content.type).toBe('END');
    expect(result.agentResponse.content.content).toBe('Final message');
  });

  it('should roundtrip agentResponse with status (StatusUpdate)', () => {
    const request = {
      agentResponse: {
        conversationId: 'conv-003',
        responseId: 'resp-003',
        status: {
          status: 'THINKING',
          customMessage: 'Processing your request...',
          emoji: ':brain:',
        },
      },
    };

    const buf = serializeRequest(request);
    const result = deserializeRequest(buf);

    expect(result.request).toBe('agentResponse');
    expect(result.agentResponse.payload).toBe('status');
    expect(result.agentResponse.status).toBeDefined();
    expect(result.agentResponse.status.status).toBe('THINKING');
    expect(result.agentResponse.status.customMessage).toBe('Processing your request...');
    expect(result.agentResponse.status.emoji).toBe(':brain:');
  });

  it('should roundtrip agentResponse with error (ErrorResponse)', () => {
    const request = {
      agentResponse: {
        conversationId: 'conv-004',
        responseId: 'resp-004',
        error: {
          code: 'RATE_LIMIT',
          message: 'Too many requests',
          details: 'Retry after 30s',
          retryable: true,
        },
      },
    };

    const buf = serializeRequest(request);
    const result = deserializeRequest(buf);

    expect(result.request).toBe('agentResponse');
    expect(result.agentResponse.payload).toBe('error');
    expect(result.agentResponse.error).toBeDefined();
    expect(result.agentResponse.error.code).toBe('RATE_LIMIT');
    expect(result.agentResponse.error.message).toBe('Too many requests');
    expect(result.agentResponse.error.details).toBe('Retry after 30s');
    expect(result.agentResponse.error.retryable).toBe(true);
  });

  it('should roundtrip message with platformContext (regression guard)', () => {
    const request = {
      message: {
        id: 'msg-001',
        platform: 'slack',
        conversationId: 'conv-005',
        content: 'Hello from TS',
        platformContext: {
          messageId: 'C123:ts',
          channelId: 'C123',
          threadId: '1234567890.000001',
          channelName: '#general',
          workspaceId: 'T999',
          platformData: { team_id: 'T999' },
        },
        user: {
          id: 'U123',
          username: 'testuser',
          avatarUrl: 'https://example.com/avatar.png',
          userData: { role: 'dev' },
        },
      },
    };

    const buf = serializeRequest(request);
    const result = deserializeRequest(buf);

    expect(result.request).toBe('message');
    expect(result.message).toBeDefined();
    expect(result.message.id).toBe('msg-001');
    expect(result.message.platform).toBe('slack');
    expect(result.message.conversationId).toBe('conv-005');
    expect(result.message.content).toBe('Hello from TS');

    // Nested objects preserved
    expect(result.message.platformContext).toBeDefined();
    expect(result.message.platformContext.messageId).toBe('C123:ts');
    expect(result.message.platformContext.channelId).toBe('C123');
    expect(result.message.platformContext.platformData).toEqual({ team_id: 'T999' });

    expect(result.message.user).toBeDefined();
    expect(result.message.user.id).toBe('U123');
    expect(result.message.user.avatarUrl).toBe('https://example.com/avatar.png');
    expect(result.message.user.userData).toEqual({ role: 'dev' });
  });
});

describe('Proto-loader binary roundtrip: AgentResponse oneof', () => {
  it('should roundtrip AgentResponse with incomingMessage (server→agent)', () => {
    const response = {
      conversationId: 'conv-010',
      responseId: 'resp-010',
      incomingMessage: {
        id: 'msg-incoming-001',
        platform: 'slack',
        conversationId: 'conv-010',
        content: 'User says hello',
        platformContext: {
          messageId: 'C456:ts',
          channelId: 'C456',
        },
        user: {
          id: 'U456',
          username: 'realuser',
        },
      },
    };

    const buf = serializeResponse(response);
    const result = deserializeResponse(buf);

    expect(result.payload).toBe('incomingMessage');
    expect(result.incomingMessage).toBeDefined();
    expect(result.incomingMessage.id).toBe('msg-incoming-001');
    expect(result.incomingMessage.platform).toBe('slack');
    expect(result.incomingMessage.content).toBe('User says hello');
    expect(result.incomingMessage.platformContext.channelId).toBe('C456');
    expect(result.incomingMessage.user.username).toBe('realuser');
    expect(result.conversationId).toBe('conv-010');
  });

  it('should roundtrip AgentResponse with content (ContentChunk)', () => {
    const response = {
      conversationId: 'conv-011',
      responseId: 'resp-011',
      content: {
        type: 'DELTA',
        content: 'Streaming token',
        platformMessageId: 'plat-msg-011',
      },
    };

    const buf = serializeResponse(response);
    const result = deserializeResponse(buf);

    expect(result.payload).toBe('content');
    expect(result.content).toBeDefined();
    expect(result.content.type).toBe('DELTA');
    expect(result.content.content).toBe('Streaming token');
    expect(result.content.platformMessageId).toBe('plat-msg-011');
  });

  it('should roundtrip AgentResponse with status (StatusUpdate)', () => {
    const response = {
      conversationId: 'conv-012',
      responseId: 'resp-012',
      status: {
        status: 'SEARCHING',
        customMessage: 'Querying knowledge base...',
      },
    };

    const buf = serializeResponse(response);
    const result = deserializeResponse(buf);

    expect(result.payload).toBe('status');
    expect(result.status).toBeDefined();
    expect(result.status.status).toBe('SEARCHING');
    expect(result.status.customMessage).toBe('Querying knowledge base...');
  });

  it('should roundtrip AgentResponse with error', () => {
    const response = {
      conversationId: 'conv-013',
      responseId: 'resp-013',
      error: {
        code: 'AGENT_ERROR',
        message: 'Internal failure',
        retryable: false,
      },
    };

    const buf = serializeResponse(response);
    const result = deserializeResponse(buf);

    expect(result.payload).toBe('error');
    expect(result.error).toBeDefined();
    expect(result.error.code).toBe('AGENT_ERROR');
    expect(result.error.message).toBe('Internal failure');
    expect(result.error.retryable).toBe(false);
  });
});
