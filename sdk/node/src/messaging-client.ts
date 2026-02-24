import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { join } from 'path';
import { EventEmitter } from 'events';

// Import types (will be generated from proto)
export interface Message {
  id?: string;
  timestamp?: any;
  platform: string;
  platformContext?: PlatformContext;
  user: User;
  content: string;
  attachments?: Attachment[];
  conversationId: string;
}

export interface User {
  id: string;
  username?: string;
  email?: string;
  avatarUrl?: string;
  userData?: { [key: string]: string };
}

export interface PlatformContext {
  messageId: string;
  channelId: string;
  threadId?: string;
  channelName?: string;
  workspaceId?: string;
  platformData?: { [key: string]: string };
}

export interface Attachment {
  type: string;
  url?: string;
  filename?: string;
  mimeType?: string;
  title?: string;
}

// AgentResponse uses proto-loader's oneof flattening (oneofs: true).
// The active oneof field goes directly on the object, not nested under "payload".
export interface AgentResponse {
  conversationId: string;
  responseId?: string;
  // oneof payload — only one of these should be set:
  incomingMessage?: Message;
  status?: StatusUpdate;
  content?: ContentChunk;
  prompts?: SuggestedPrompts;
  threadMetadata?: ThreadMetadata;
  error?: ErrorResponse;
  contextRequest?: ThreadHistoryRequest;
}

export interface StatusUpdate {
  status: 'THINKING' | 'SEARCHING' | 'GENERATING' | 'PROCESSING' | 'ANALYZING' | 'CUSTOM';
  customMessage?: string;
  emoji?: string;
}

export interface ContentChunk {
  type: 'START' | 'DELTA' | 'END' | 'REPLACE';
  content: string;
  attachments?: any[];
  platformMessageId?: string;
  options?: any;
}

export interface SuggestedPrompts {
  prompts: Array<{
    id: string;
    title: string;
    message: string;
    description?: string;
  }>;
}

export interface ThreadMetadata {
  threadId?: string;
  title?: string;
  createNew?: boolean;
}

export interface ErrorResponse {
  code: string;
  message: string;
  details?: string;
  retryable?: boolean;
}

export interface ThreadHistoryRequest {
  conversationId: string;
  maxMessages?: number;
  includeEdited?: boolean;
  includeDeleted?: boolean;
}

export interface ThreadHistoryResponse {
  conversationId: string;
  messages: ThreadMessage[];
  isComplete: boolean;
  fetchedAt?: any;
}

export interface ThreadMessage {
  messageId: string;
  user: User;
  content: string;
  attachments?: Attachment[];
  timestamp: any;
  wasEdited?: boolean;
  isDeleted?: boolean;
  originalContent?: string;
  editedAt?: any;
  deletedAt?: any;
  platformData?: { [key: string]: string };
}

export interface AgentToolGraphNode {
  id: string;
  name: string;
  type: string;
}

export interface AgentToolGraphEdge {
  id: string;
  source: string;
  target: string;
}

export interface AgentToolGraph {
  nodes: AgentToolGraphNode[];
  edges: AgentToolGraphEdge[];
}

export interface AgentToolConfig {
  name: string;
  title: string;
  description: string;
  type: string;
  graph?: AgentToolGraph;
  /** Optional. JSON Schema for MCP tool input (must have "type":"object"). If omitted, MCP uses {"type":"object"}. */
  inputSchemaJson?: string;
}

export interface AgentConfig {
  systemPrompt: string;
  tools: AgentToolConfig[];
  /** MCP tool definitions (what to expose over MCP). Separate from agent tools. */
  mcpDefinition?: AgentToolConfig[];
}

export interface ConversationRequest {
  message?: Message;
  feedback?: any;
  agentConfig?: AgentConfig;
  agentResponse?: AgentResponse;
}

/**
 * MessagingClient provides a TypeScript interface to the Astro Messaging gRPC service
 */
export class MessagingClient extends EventEmitter {
  private client: any;
  private conversationStream: any;
  private isConnected: boolean = false;

  constructor(private serverAddress: string) {
    super();
  }

  /**
   * Connect to the gRPC server
   */
  async connect(): Promise<void> {
    const protoPath = 'astro/messaging/v1/service.proto';

    const packageDefinition = protoLoader.loadSync(protoPath, {
      keepCase: false,
      longs: String,
      enums: String,
      defaults: true,
      oneofs: true,
      includeDirs: [join(__dirname, 'proto')],
    });

    const protoDescriptor = grpc.loadPackageDefinition(packageDefinition) as any;
    const AgentMessaging = protoDescriptor.astro.messaging.v1.AgentMessaging;

    this.client = new AgentMessaging(
      this.serverAddress,
      grpc.credentials.createInsecure()
    );

    this.isConnected = true;
    this.emit('connected');
  }

  /**
   * Create a bidirectional conversation stream
   */
  createConversationStream(): ConversationStream {
    if (!this.isConnected) {
      throw new Error('Client not connected. Call connect() first.');
    }

    this.conversationStream = this.client.ProcessConversation();
    return new ConversationStream(this.conversationStream);
  }

  /**
   * Process a single message (server-side streaming)
   */
  async processMessage(message: Message): Promise<MessageStream> {
    if (!this.isConnected) {
      throw new Error('Client not connected. Call connect() first.');
    }

    return new Promise((resolve, reject) => {
      const call = this.client.ProcessMessage(message);
      resolve(new MessageStream(call));
    });
  }

  /**
   * Get thread history for a conversation
   */
  async getThreadHistory(
    conversationId: string,
    maxMessages: number = 50
  ): Promise<ThreadHistoryResponse> {
    if (!this.isConnected) {
      throw new Error('Client not connected. Call connect() first.');
    }

    const request: ThreadHistoryRequest = {
      conversationId,
      maxMessages,
      includeEdited: true,
      includeDeleted: false,
    };

    return new Promise((resolve, reject) => {
      this.client.GetThreadHistory(request, (error: any, response: ThreadHistoryResponse) => {
        if (error) {
          reject(error);
        } else {
          resolve(response);
        }
      });
    });
  }

  /**
   * Get conversation metadata
   */
  async getConversationMetadata(conversationId: string): Promise<any> {
    if (!this.isConnected) {
      throw new Error('Client not connected. Call connect() first.');
    }

    const request = {
      identifier: {
        conversationId,
      },
    };

    return new Promise((resolve, reject) => {
      this.client.GetConversationMetadata(request, (error: any, response: any) => {
        if (error) {
          reject(error);
        } else {
          resolve(response);
        }
      });
    });
  }

  /**
   * Check service health
   */
  async healthCheck(): Promise<{ status: string }> {
    if (!this.isConnected) {
      throw new Error('Client not connected. Call connect() first.');
    }

    return new Promise((resolve, reject) => {
      this.client.HealthCheck({}, (error: any, response: any) => {
        if (error) {
          reject(error);
        } else {
          resolve(response);
        }
      });
    });
  }

  /**
   * Close the client connection
   */
  close(): void {
    if (this.conversationStream) {
      this.conversationStream.end();
    }
    if (this.client) {
      this.client.close();
    }
    this.isConnected = false;
    this.emit('disconnected');
  }
}

/**
 * ConversationStream wraps a bidirectional gRPC stream
 */
export class ConversationStream extends EventEmitter {
  constructor(private stream: any) {
    super();

    this.stream.on('data', (response: AgentResponse) => {
      this.emit('response', response);
    });

    this.stream.on('end', () => {
      this.emit('end');
    });

    this.stream.on('error', (error: Error) => {
      this.emit('error', error);
    });
  }

  /**
   * Send a message through the stream
   */
  sendMessage(message: Message): void {
    const request: ConversationRequest = {
      message,
    };
    this.stream.write(request);
  }

  /**
   * Send platform feedback through the stream
   */
  sendFeedback(feedback: any): void {
    const request: ConversationRequest = {
      feedback,
    };
    this.stream.write(request);
  }

  /**
   * Send agent configuration through the stream
   */
  sendAgentConfig(config: AgentConfig): void {
    const request: ConversationRequest = {
      agentConfig: config,
    };
    this.stream.write(request);
  }

  /**
   * Send a typed AgentResponse through the stream
   */
  sendAgentResponse(response: AgentResponse): void {
    const request: ConversationRequest = {
      agentResponse: response,
    };
    this.stream.write(request);
  }

  /**
   * Send a content chunk (START/DELTA/END) for a conversation
   */
  sendContentChunk(conversationId: string, chunk: ContentChunk): void {
    this.sendAgentResponse({
      conversationId,
      content: chunk,
    });
  }

  /**
   * Send a status update for a conversation
   */
  sendStatusUpdate(conversationId: string, status: StatusUpdate): void {
    this.sendAgentResponse({
      conversationId,
      status,
    });
  }

  /**
   * End the stream
   */
  end(): void {
    this.stream.end();
  }
}

/**
 * MessageStream wraps a server-side streaming response
 */
export class MessageStream extends EventEmitter {
  constructor(private call: any) {
    super();

    this.call.on('data', (response: AgentResponse) => {
      this.emit('response', response);
    });

    this.call.on('end', () => {
      this.emit('end');
    });

    this.call.on('error', (error: Error) => {
      this.emit('error', error);
    });
  }
}

/**
 * Helper functions for creating common message types
 */
export const Helpers = {
  createMessage(
    conversationId: string,
    userId: string,
    username: string,
    content: string
  ): Message {
    return {
      conversationId,
      user: {
        id: userId,
        username,
      },
      content,
      platform: 'slack',
    };
  },

  createStatusResponse(
    conversationId: string,
    status: StatusUpdate['status'],
    message?: string
  ): AgentResponse {
    return {
      conversationId,
      status: {
        status,
        customMessage: message,
      },
    };
  },

  createContentResponse(conversationId: string, content: string, final: boolean = true): AgentResponse {
    return {
      conversationId,
      content: {
        type: final ? 'END' : 'START',
        content,
      },
    };
  },

  createSuggestedPromptsResponse(
    conversationId: string,
    prompts: Array<{ title: string; message: string }>
  ): AgentResponse {
    return {
      conversationId,
      prompts: {
        prompts: prompts.map((p, i) => ({
          id: `prompt_${i}`,
          title: p.title,
          message: p.message,
        })),
      },
    };
  },

  createErrorResponse(
    conversationId: string,
    code: string,
    message: string
  ): AgentResponse {
    return {
      conversationId,
      error: {
        code,
        message,
      },
    };
  },
};
