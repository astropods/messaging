import { describe, it, expect, beforeEach } from 'bun:test';
import { EventEmitter } from 'events';
import * as protoLoader from '@grpc/proto-loader';
import { join } from 'path';
import {
  MessagingClient,
  ConversationStream,
  MessageStream,
  Helpers,
  type Message,
  type PlatformContext,
  type AgentResponse,
  type ConversationRequest,
} from './messaging-client';

// --- Helper factories ---

function createPlatformContext(overrides: Partial<PlatformContext> = {}): PlatformContext {
  return {
    messageId: 'msg-001',
    channelId: 'ch-001',
    threadId: 'thread-001',
    ...overrides,
  };
}

function createMessage(overrides: Partial<Message> = {}): Message {
  return {
    id: 'msg-001',
    platform: 'web',
    conversationId: 'conv-001',
    content: 'Hello',
    user: { id: 'user-1', username: 'testuser' },
    ...overrides,
  };
}

// --- Mock stream that captures writes ---

class MockGrpcStream extends EventEmitter {
  public written: any[] = [];
  public ended = false;

  write(data: any) {
    this.written.push(data);
  }

  end() {
    this.ended = true;
  }
}

// =============================================
// Proto-loader field name mapping tests
// =============================================

describe('proto-loader field mapping', () => {
  let packageDefinition: protoLoader.PackageDefinition;

  beforeEach(() => {
    const protoPath = 'astro/messaging/v1/service.proto';
    const protoRoot = join(__dirname, '../proto');

    packageDefinition = protoLoader.loadSync(protoPath, {
      keepCase: false, // same as MessagingClient uses
      longs: String,
      enums: String,
      defaults: true,
      oneofs: true,
      includeDirs: [protoRoot],
    });
  });

  it('should load proto definitions successfully', () => {
    expect(packageDefinition).toBeDefined();
    // Service should be present
    expect(packageDefinition['astro.messaging.v1.AgentMessaging']).toBeDefined();
  });

  // Helper: extract field names from a proto-loader type definition
  function getFieldNames(typeDef: any): string[] {
    const fields = typeDef?.type?.field ?? [];
    return Object.values(fields).map((f: any) => f.name);
  }

  it('should have Message type with camelCase field names', () => {
    const messageType = packageDefinition['astro.messaging.v1.Message'];
    expect(messageType).toBeDefined();

    const fieldNames = getFieldNames(messageType);

    // These proto fields: platform_context, conversation_id
    // should become: platformContext, conversationId with keepCase=false
    expect(fieldNames).toContain('platformContext');
    expect(fieldNames).toContain('conversationId');
    expect(fieldNames).toContain('platform');
    expect(fieldNames).toContain('content');
    expect(fieldNames).toContain('user');
  });

  it('should have PlatformContext type with camelCase field names', () => {
    const pcType = packageDefinition['astro.messaging.v1.PlatformContext'];
    expect(pcType).toBeDefined();

    const fieldNames = getFieldNames(pcType);

    // message_id → messageId, channel_id → channelId, etc.
    expect(fieldNames).toContain('messageId');
    expect(fieldNames).toContain('channelId');
    expect(fieldNames).toContain('threadId');
    expect(fieldNames).toContain('channelName');
    expect(fieldNames).toContain('workspaceId');
    expect(fieldNames).toContain('platformData');
  });

  it('should have AgentResponse with camelCase oneof fields', () => {
    const responseType = packageDefinition['astro.messaging.v1.AgentResponse'];
    expect(responseType).toBeDefined();

    const fieldNames = getFieldNames(responseType);

    expect(fieldNames).toContain('conversationId');
    expect(fieldNames).toContain('responseId');
    // oneof payload fields
    expect(fieldNames).toContain('incomingMessage');
    expect(fieldNames).toContain('status');
    expect(fieldNames).toContain('content');
  });

  it('should have ConversationRequest with message, feedback, and agentResponse fields', () => {
    const reqType = packageDefinition['astro.messaging.v1.ConversationRequest'];
    expect(reqType).toBeDefined();

    const fieldNames = getFieldNames(reqType);

    expect(fieldNames).toContain('message');
    expect(fieldNames).toContain('feedback');
    expect(fieldNames).toContain('agentResponse');
  });

  it('should match keepCase=false with the TS interface field names', () => {
    // Verify that the proto-loader camelCase output matches our TS interfaces
    const pcType = packageDefinition['astro.messaging.v1.PlatformContext'];
    const protoFields = getFieldNames(pcType);

    // These are the fields defined in the PlatformContext TS interface
    const tsInterfaceFields: (keyof PlatformContext)[] = [
      'messageId',
      'channelId',
      'threadId',
      'channelName',
      'workspaceId',
      'platformData',
    ];

    for (const field of tsInterfaceFields) {
      expect(protoFields).toContain(field);
    }
  });
});

// =============================================
// Helpers tests
// =============================================

describe('Helpers', () => {
  describe('createMessage', () => {
    it('should create a message with correct fields', () => {
      const msg = Helpers.createMessage('conv-1', 'user-1', 'alice', 'Hello');

      expect(msg.conversationId).toBe('conv-1');
      expect(msg.user.id).toBe('user-1');
      expect(msg.user.username).toBe('alice');
      expect(msg.content).toBe('Hello');
      expect(msg.platform).toBe('slack');
    });

    it('should not include platformContext by default', () => {
      const msg = Helpers.createMessage('conv-1', 'user-1', 'alice', 'Hello');

      // This is a potential problem: messages created by Helpers don't have platformContext
      expect(msg.platformContext).toBeUndefined();
    });
  });

  describe('createStatusResponse', () => {
    it('should create THINKING status', () => {
      const resp = Helpers.createStatusResponse('conv-1', 'THINKING');

      expect(resp.conversationId).toBe('conv-1');
      expect(resp.status?.status).toBe('THINKING');
      expect(resp.status?.customMessage).toBeUndefined();
    });

    it('should include custom message when provided', () => {
      const resp = Helpers.createStatusResponse('conv-1', 'CUSTOM', 'Searching docs...');

      expect(resp.status?.status).toBe('CUSTOM');
      expect(resp.status?.customMessage).toBe('Searching docs...');
    });

    it('should support all status types', () => {
      const statuses = ['THINKING', 'SEARCHING', 'GENERATING', 'PROCESSING', 'ANALYZING', 'CUSTOM'] as const;
      for (const status of statuses) {
        const resp = Helpers.createStatusResponse('conv-1', status);
        expect(resp.status?.status).toBe(status);
      }
    });
  });

  describe('createContentResponse', () => {
    it('should create END chunk by default (final=true)', () => {
      const resp = Helpers.createContentResponse('conv-1', 'Hello world');

      expect(resp.content?.type).toBe('END');
      expect(resp.content?.content).toBe('Hello world');
    });

    it('should create START chunk when final=false', () => {
      const resp = Helpers.createContentResponse('conv-1', 'Starting...', false);

      expect(resp.content?.type).toBe('START');
    });
  });

  describe('createSuggestedPromptsResponse', () => {
    it('should create prompts with auto-generated IDs', () => {
      const prompts = [
        { title: 'Help', message: 'Can you help me?' },
        { title: 'Example', message: 'Show an example' },
      ];

      const resp = Helpers.createSuggestedPromptsResponse('conv-1', prompts);

      expect(resp.prompts?.prompts).toHaveLength(2);
      expect(resp.prompts?.prompts[0].id).toBe('prompt_0');
      expect(resp.prompts?.prompts[0].title).toBe('Help');
      expect(resp.prompts?.prompts[0].message).toBe('Can you help me?');
      expect(resp.prompts?.prompts[1].id).toBe('prompt_1');
    });

    it('should handle empty prompts array', () => {
      const resp = Helpers.createSuggestedPromptsResponse('conv-1', []);
      expect(resp.prompts?.prompts).toHaveLength(0);
    });
  });

  describe('createErrorResponse', () => {
    it('should create error with code and message', () => {
      const resp = Helpers.createErrorResponse('conv-1', 'RATE_LIMIT', 'Too many requests');

      expect(resp.error?.code).toBe('RATE_LIMIT');
      expect(resp.error?.message).toBe('Too many requests');
    });
  });
});

// =============================================
// ConversationStream tests
// =============================================

describe('ConversationStream', () => {
  let mockGrpc: MockGrpcStream;
  let stream: ConversationStream;

  beforeEach(() => {
    mockGrpc = new MockGrpcStream();
    stream = new ConversationStream(mockGrpc);
  });

  describe('sendMessage', () => {
    it('should wrap message in ConversationRequest', () => {
      const msg = createMessage();
      stream.sendMessage(msg);

      expect(mockGrpc.written).toHaveLength(1);
      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.message).toBeDefined();
      expect(written.message?.content).toBe('Hello');
    });

    it('should preserve platformContext in written message', () => {
      const pc = createPlatformContext({
        messageId: 'plat-msg-001',
        channelId: 'conv-abc',
        threadId: 'thread-xyz',
      });

      const msg = createMessage({ platformContext: pc });
      stream.sendMessage(msg);

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.message?.platformContext).toBeDefined();
      expect(written.message?.platformContext?.messageId).toBe('plat-msg-001');
      expect(written.message?.platformContext?.channelId).toBe('conv-abc');
      expect(written.message?.platformContext?.threadId).toBe('thread-xyz');
    });

    it('should preserve platformContext with empty threadId', () => {
      const pc = createPlatformContext({
        messageId: 'msg-001',
        channelId: 'conv-123',
        threadId: undefined,
      });

      const msg = createMessage({ platformContext: pc });
      stream.sendMessage(msg);

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.message?.platformContext?.channelId).toBe('conv-123');
      expect(written.message?.platformContext?.threadId).toBeUndefined();
    });

    it('should handle message without platformContext', () => {
      const msg = createMessage({ platformContext: undefined });
      stream.sendMessage(msg);

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.message?.platformContext).toBeUndefined();
    });

    it('should preserve all PlatformContext fields', () => {
      const pc: PlatformContext = {
        messageId: 'msg-slack-ts',
        channelId: 'C123456',
        threadId: '1234567890.000001',
        channelName: '#general',
        workspaceId: 'T999',
        platformData: {
          team_id: 'T999',
          bot_id: 'B123',
        },
      };

      const msg = createMessage({ platform: 'slack', platformContext: pc });
      stream.sendMessage(msg);

      const written = mockGrpc.written[0] as ConversationRequest;
      const writtenPC = written.message?.platformContext;
      expect(writtenPC?.messageId).toBe('msg-slack-ts');
      expect(writtenPC?.channelId).toBe('C123456');
      expect(writtenPC?.threadId).toBe('1234567890.000001');
      expect(writtenPC?.channelName).toBe('#general');
      expect(writtenPC?.workspaceId).toBe('T999');
      expect(writtenPC?.platformData).toEqual({ team_id: 'T999', bot_id: 'B123' });
    });

    it('should preserve user fields', () => {
      const msg = createMessage({
        user: {
          id: 'U123',
          username: 'alice',
          email: 'alice@example.com',
          avatarUrl: 'https://example.com/avatar.png',
        },
      });
      stream.sendMessage(msg);

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.message?.user.id).toBe('U123');
      expect(written.message?.user.username).toBe('alice');
      expect(written.message?.user.email).toBe('alice@example.com');
      expect(written.message?.user.avatarUrl).toBe('https://example.com/avatar.png');
    });
  });

  describe('sendFeedback', () => {
    it('should wrap feedback in ConversationRequest', () => {
      const feedback = { conversationId: 'conv-1', type: 'thumbs_up' };
      stream.sendFeedback(feedback);

      expect(mockGrpc.written).toHaveLength(1);
      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.feedback).toBeDefined();
      expect(written.feedback.conversationId).toBe('conv-1');
    });
  });

  describe('sendAgentResponse', () => {
    it('should wrap AgentResponse in ConversationRequest', () => {
      stream.sendAgentResponse({
        conversationId: 'conv-1',
        content: { type: 'DELTA', content: 'Hello' },
      });

      expect(mockGrpc.written).toHaveLength(1);
      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse).toBeDefined();
      expect(written.agentResponse?.conversationId).toBe('conv-1');
    });

    it('should not set message or feedback keys', () => {
      stream.sendAgentResponse({
        conversationId: 'conv-1',
        status: { status: 'THINKING' },
      });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.message).toBeUndefined();
      expect(written.feedback).toBeUndefined();
      expect(written.agentResponse).toBeDefined();
    });
  });

  describe('sendContentChunk', () => {
    it('should send START chunk', () => {
      stream.sendContentChunk('conv-1', { type: 'START', content: '' });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse?.content?.type).toBe('START');
      expect(written.agentResponse?.content?.content).toBe('');
    });

    it('should send DELTA chunk with content', () => {
      stream.sendContentChunk('conv-1', { type: 'DELTA', content: 'Hello ' });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse?.content?.type).toBe('DELTA');
      expect(written.agentResponse?.content?.content).toBe('Hello ');
    });

    it('should send END chunk with full content', () => {
      stream.sendContentChunk('conv-1', { type: 'END', content: 'Hello world' });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse?.content?.type).toBe('END');
      expect(written.agentResponse?.content?.content).toBe('Hello world');
    });

    it('should set conversationId on the AgentResponse', () => {
      stream.sendContentChunk('my-conv', { type: 'DELTA', content: 'x' });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse?.conversationId).toBe('my-conv');
    });
  });

  describe('sendStatusUpdate', () => {
    it('should send THINKING status', () => {
      stream.sendStatusUpdate('conv-1', { status: 'THINKING' });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse?.status?.status).toBe('THINKING');
    });

    it('should send CUSTOM status with message', () => {
      stream.sendStatusUpdate('conv-1', { status: 'CUSTOM', customMessage: 'Searching docs...' });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse?.status?.status).toBe('CUSTOM');
      expect(written.agentResponse?.status?.customMessage).toBe('Searching docs...');
    });

    it('should set conversationId on the AgentResponse', () => {
      stream.sendStatusUpdate('my-conv', { status: 'GENERATING' });

      const written = mockGrpc.written[0] as ConversationRequest;
      expect(written.agentResponse?.conversationId).toBe('my-conv');
    });
  });

  describe('end', () => {
    it('should end the underlying stream', () => {
      stream.end();
      expect(mockGrpc.ended).toBe(true);
    });
  });

  describe('event forwarding', () => {
    it('should emit response events from gRPC data', () => {
      const received: any[] = [];
      stream.on('response', (resp: any) => received.push(resp));

      const mockResponse = {
        conversationId: 'conv-1',
        incomingMessage: {
          platform: 'web',
          content: 'Hello from platform',
          platformContext: {
            messageId: 'msg-1',
            channelId: 'conv-1',
          },
        },
      };

      mockGrpc.emit('data', mockResponse);

      expect(received).toHaveLength(1);
      expect(received[0].conversationId).toBe('conv-1');
      expect(received[0].incomingMessage.platformContext.channelId).toBe('conv-1');
    });

    it('should emit end event', () => {
      let ended = false;
      stream.on('end', () => { ended = true; });

      mockGrpc.emit('end');
      expect(ended).toBe(true);
    });

    it('should emit error event', () => {
      let receivedError: Error | null = null;
      stream.on('error', (err: Error) => { receivedError = err; });

      mockGrpc.emit('error', new Error('stream broken'));
      expect(receivedError?.message).toBe('stream broken');
    });
  });
});

// =============================================
// MessageStream tests
// =============================================

describe('MessageStream', () => {
  it('should forward response events', () => {
    const mockCall = new MockGrpcStream();
    const msgStream = new MessageStream(mockCall);

    const received: any[] = [];
    msgStream.on('response', (resp: any) => received.push(resp));

    mockCall.emit('data', { conversationId: 'conv-1', content: 'test' });
    expect(received).toHaveLength(1);
  });

  it('should forward end events', () => {
    const mockCall = new MockGrpcStream();
    const msgStream = new MessageStream(mockCall);

    let ended = false;
    msgStream.on('end', () => { ended = true; });

    mockCall.emit('end');
    expect(ended).toBe(true);
  });

  it('should forward error events', () => {
    const mockCall = new MockGrpcStream();
    const msgStream = new MessageStream(mockCall);

    let receivedError: Error | null = null;
    msgStream.on('error', (err: Error) => { receivedError = err; });

    mockCall.emit('error', new Error('test error'));
    expect(receivedError?.message).toBe('test error');
  });
});

// =============================================
// MessagingClient tests
// =============================================

describe('MessagingClient', () => {
  it('should throw when creating stream before connect', () => {
    const client = new MessagingClient('localhost:9090');
    expect(() => client.createConversationStream()).toThrow('Client not connected');
  });

  it('should throw when calling processMessage before connect', async () => {
    const client = new MessagingClient('localhost:9090');
    await expect(client.processMessage(createMessage())).rejects.toThrow('Client not connected');
  });

  it('should throw when calling getThreadHistory before connect', async () => {
    const client = new MessagingClient('localhost:9090');
    await expect(client.getThreadHistory('conv-1')).rejects.toThrow('Client not connected');
  });

  it('should throw when calling getConversationMetadata before connect', async () => {
    const client = new MessagingClient('localhost:9090');
    await expect(client.getConversationMetadata('conv-1')).rejects.toThrow('Client not connected');
  });

  it('should throw when calling healthCheck before connect', async () => {
    const client = new MessagingClient('localhost:9090');
    await expect(client.healthCheck()).rejects.toThrow('Client not connected');
  });

  it('should emit disconnected on close', () => {
    const client = new MessagingClient('localhost:9090');
    let disconnected = false;
    client.on('disconnected', () => { disconnected = true; });

    client.close();
    expect(disconnected).toBe(true);
  });
});

// =============================================
// PlatformContext roundtrip simulation tests
// =============================================

describe('PlatformContext roundtrip', () => {
  it('should preserve context when agent echoes back incoming message context', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    // Simulate what astro-messaging sends to the agent (incoming platform message)
    const incomingFromServer = {
      conversationId: 'conv-web-123',
      responseId: 'msg-001',
      incomingMessage: {
        id: 'msg-001',
        platform: 'web',
        conversationId: 'conv-web-123',
        content: 'What is an API?',
        platformContext: {
          messageId: 'msg-001',
          channelId: 'conv-web-123',
          threadId: '',
          channelName: '',
          workspaceId: '',
          platformData: {},
        },
        user: {
          id: 'user-42',
          username: 'webuser',
        },
      },
    };

    // Agent receives the incoming message
    let receivedResponse: any = null;
    stream.on('response', (resp: any) => { receivedResponse = resp; });
    mockGrpc.emit('data', incomingFromServer);

    expect(receivedResponse).not.toBeNull();

    // Agent extracts platformContext (this is what the TS agent does)
    const message = receivedResponse.incomingMessage;
    const platformContext = message.platformContext;

    // Agent sends response back with the same platformContext
    stream.sendMessage({
      conversationId: message.conversationId,
      platform: message.platform,
      platformContext: platformContext,
      content: 'An API is an Application Programming Interface',
      user: { id: 'agent', username: 'Engineering Assistant' },
    });

    // Verify what was written to the gRPC stream
    const written = mockGrpc.written[0] as ConversationRequest;
    expect(written.message).toBeDefined();
    expect(written.message?.platform).toBe('web');
    expect(written.message?.conversationId).toBe('conv-web-123');

    // The critical check: platformContext must be intact
    const sentPC = written.message?.platformContext;
    expect(sentPC).toBeDefined();
    expect(sentPC?.messageId).toBe('msg-001');
    expect(sentPC?.channelId).toBe('conv-web-123');
  });

  it('should preserve Slack threaded message context through roundtrip', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    const incomingFromServer = {
      conversationId: 'C123456-1234567890.000001',
      incomingMessage: {
        id: 'msg-slack-001',
        platform: 'slack',
        conversationId: 'C123456-1234567890.000001',
        content: 'Thread reply',
        platformContext: {
          messageId: 'C123456:1234567890.999999',
          channelId: 'C123456',
          threadId: '1234567890.000001',
          channelName: '#general',
          workspaceId: 'T999',
          platformData: { team_id: 'T999' },
        },
        user: { id: 'U123456', username: 'slackuser' },
      },
    };

    let receivedResponse: any = null;
    stream.on('response', (resp: any) => { receivedResponse = resp; });
    mockGrpc.emit('data', incomingFromServer);

    const message = receivedResponse.incomingMessage;

    // Agent echoes back with same context
    stream.sendMessage({
      conversationId: message.conversationId,
      platform: message.platform,
      platformContext: message.platformContext,
      content: 'Here is my answer',
      user: { id: 'agent', username: 'Agent' },
    });

    const written = mockGrpc.written[0] as ConversationRequest;
    const sentPC = written.message?.platformContext;

    expect(sentPC?.channelId).toBe('C123456');
    expect(sentPC?.threadId).toBe('1234567890.000001');
    expect(sentPC?.messageId).toBe('C123456:1234567890.999999');
    expect(sentPC?.channelName).toBe('#general');
    expect(sentPC?.workspaceId).toBe('T999');
    expect(sentPC?.platformData).toEqual({ team_id: 'T999' });
  });

  it('should handle missing platformContext in incoming message', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    // Incoming message WITHOUT platformContext (edge case / bug scenario)
    const incomingFromServer = {
      conversationId: 'conv-1',
      incomingMessage: {
        id: 'msg-1',
        platform: 'web',
        conversationId: 'conv-1',
        content: 'Hello',
        // platformContext intentionally missing
        user: { id: 'user-1', username: 'test' },
      },
    };

    let receivedResponse: any = null;
    stream.on('response', (resp: any) => { receivedResponse = resp; });
    mockGrpc.emit('data', incomingFromServer);

    const message = receivedResponse.incomingMessage;
    const platformContext = message.platformContext;

    // platformContext is undefined - agent tries to echo it
    stream.sendMessage({
      conversationId: message.conversationId,
      platform: message.platform,
      platformContext: platformContext, // undefined
      content: 'Response',
      user: { id: 'agent', username: 'Agent' },
    });

    const written = mockGrpc.written[0] as ConversationRequest;
    // This is the bug: platformContext is undefined, so Go server will get nil PlatformContext
    expect(written.message?.platformContext).toBeUndefined();
  });

  it('should handle platformContext with all empty string fields', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    // proto3 defaults: empty strings for all fields
    const incomingFromServer = {
      conversationId: 'conv-1',
      incomingMessage: {
        id: 'msg-1',
        platform: 'web',
        conversationId: 'conv-1',
        content: 'Hello',
        platformContext: {
          messageId: '',
          channelId: '',
          threadId: '',
          channelName: '',
          workspaceId: '',
          platformData: {},
        },
        user: { id: 'user-1', username: 'test' },
      },
    };

    let receivedResponse: any = null;
    stream.on('response', (resp: any) => { receivedResponse = resp; });
    mockGrpc.emit('data', incomingFromServer);

    const message = receivedResponse.incomingMessage;

    stream.sendMessage({
      conversationId: message.conversationId,
      platform: message.platform,
      platformContext: message.platformContext,
      content: 'Response',
      user: { id: 'agent', username: 'Agent' },
    });

    const written = mockGrpc.written[0] as ConversationRequest;
    const sentPC = written.message?.platformContext;

    // All fields present but empty - this is what Go server sees as `PlatformContext: `
    expect(sentPC).toBeDefined();
    expect(sentPC?.messageId).toBe('');
    expect(sentPC?.channelId).toBe('');
    expect(sentPC?.threadId).toBe('');
  });
});

// =============================================
// Proto-loader oneofs behavior tests
// =============================================

describe('proto-loader oneofs: true behavior', () => {
  it('should surface oneof fields directly on the object (not nested under payload)', () => {
    // When proto-loader is configured with oneofs: true, oneof fields appear
    // directly on the object. The AgentResponse has:
    //   oneof payload { Message incoming_message = 3; ... }
    // With oneofs: true, proto-loader puts the active field directly:
    //   response.incomingMessage = { ... }  (NOT response.payload.incomingMessage)
    //
    // And a "payload" field indicates which oneof is active:
    //   response.payload = "incomingMessage"

    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    // This is the shape proto-loader actually produces with oneofs: true
    const serverResponse = {
      conversationId: 'conv-1',
      responseId: 'resp-1',
      incomingMessage: {
        id: 'msg-1',
        platform: 'web',
        content: 'Hello',
        platformContext: {
          messageId: 'msg-1',
          channelId: 'conv-1',
        },
        user: { id: 'user-1', username: 'test' },
      },
      payload: 'incomingMessage', // oneofs: true sets this to the active field name
    };

    let received: any = null;
    stream.on('response', (resp: any) => { received = resp; });
    mockGrpc.emit('data', serverResponse);

    // Agent should access the field directly, NOT via .payload
    expect(received.incomingMessage).toBeDefined();
    expect(received.incomingMessage.content).toBe('Hello');
    expect(received.incomingMessage.platformContext.channelId).toBe('conv-1');

    // The "payload" field is a string indicating which oneof is active
    expect(received.payload).toBe('incomingMessage');
  });

  it('should handle content chunk oneof correctly', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    const serverResponse = {
      conversationId: 'conv-1',
      content: {
        type: 'DELTA',
        content: 'Hello world',
      },
      payload: 'content',
    };

    let received: any = null;
    stream.on('response', (resp: any) => { received = resp; });
    mockGrpc.emit('data', serverResponse);

    expect(received.content.type).toBe('DELTA');
    expect(received.content.content).toBe('Hello world');
  });
});

// =============================================
// ConversationRequest serialization tests
// =============================================

describe('ConversationRequest serialization', () => {
  it('should use "message" key for message requests (matching proto oneof)', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    stream.sendMessage(createMessage({ content: 'test' }));

    const written = mockGrpc.written[0];
    // ConversationRequest oneof: { message: Message } or { feedback: PlatformFeedback }
    expect(written).toHaveProperty('message');
    expect(written).not.toHaveProperty('feedback');
  });

  it('should use "feedback" key for feedback requests', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    stream.sendFeedback({ conversationId: 'conv-1' });

    const written = mockGrpc.written[0];
    expect(written).toHaveProperty('feedback');
    expect(written).not.toHaveProperty('message');
  });

  it('should serialize nested platformContext correctly for gRPC wire format', () => {
    const mockGrpc = new MockGrpcStream();
    const stream = new ConversationStream(mockGrpc);

    const msg = createMessage({
      platform: 'slack',
      conversationId: 'C123-thread',
      platformContext: {
        messageId: 'C123:ts',
        channelId: 'C123',
        threadId: '1234567890.000001',
        platformData: { team_id: 'T999' },
      },
    });

    stream.sendMessage(msg);

    const written = mockGrpc.written[0] as ConversationRequest;

    // Verify the full structure matches what proto-loader expects for serialization
    // proto-loader with keepCase:false expects camelCase keys and will serialize
    // them to the correct snake_case wire format
    expect(written).toEqual({
      message: {
        id: 'msg-001',
        platform: 'slack',
        conversationId: 'C123-thread',
        content: 'Hello',
        user: { id: 'user-1', username: 'testuser' },
        platformContext: {
          messageId: 'C123:ts',
          channelId: 'C123',
          threadId: '1234567890.000001',
          platformData: { team_id: 'T999' },
        },
      },
    });
  });
});
