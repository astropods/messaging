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
  transcript?: Transcript;
  audioConfig?: AudioStreamConfig;
  audioChunk?: AudioChunk;
  action?: ActionRequest;
}

export interface ActionRequest {
  actionName: string;
  payloadJson: string;
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

export interface Transcript {
  text: string;
  messageId?: string;
  language?: string;
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
}

export interface AgentConfig {
  systemPrompt: string;
  tools: AgentToolConfig[];
}

// --- Audio types ---
//
// These types support raw audio streaming between the messaging server and agent.
// Audio flows: Client mic → WebSocket → Server → gRPC → Agent SDK (here)
//
// The agent receives audio as events on ConversationStream:
//   stream.on('audioConfig', config => { ... })  // encoding, sample rate, etc.
//   stream.on('audioChunk', chunk => { ... })     // raw audio bytes
//
// The agent can convert these into a ReadableStream for Mastra's STT:
//   const audioStream = conversation.audioAsReadable();
//   const transcript = await agent.voice.listen(audioStream, { filetype: 'webm' });

/**
 * Supported audio encoding formats. Matches the AudioEncoding protobuf enum.
 *
 * Common sources:
 * - LINEAR16: Universal PCM baseline (any platform)
 * - MULAW: Twilio / telephony (G.711 mu-law, 8kHz)
 * - WEBM_OPUS: Browser MediaRecorder default
 * - AAC: iOS native recording
 */
export type AudioEncoding =
  | 'LINEAR16'
  | 'MULAW'
  | 'OPUS'
  | 'MP3'
  | 'WEBM_OPUS'
  | 'OGG_OPUS'
  | 'FLAC'
  | 'AAC';

/**
 * Configuration sent at the start of an audio segment to describe the format.
 * Maps to the AudioStreamConfig protobuf message.
 */
export interface AudioStreamConfig {
  encoding: AudioEncoding;    // Audio codec (e.g. 'WEBM_OPUS')
  sampleRate: number;         // Hz (e.g. 48000 for browser, 8000 for Twilio)
  channels: number;           // 1 = mono (speech), 2 = stereo
  language?: string;          // BCP-47 hint for STT (e.g. 'en-US')
  conversationId: string;     // Links audio to the conversation
  source?: string;            // Origin: 'browser', 'twilio', 'mobile', etc.
  userId?: string;            // Identity of the user sending audio
}

/**
 * A chunk of raw audio bytes. Maps to the AudioChunk protobuf message.
 *
 * Chunks arrive sequentially during a segment. When done=true, the segment
 * is complete and the agent should run STT on the accumulated audio.
 */
export interface AudioChunk {
  data: Buffer | Uint8Array;  // Raw audio bytes (empty when done=true)
  sequence?: number;          // Monotonic ordering counter
  done?: boolean;             // true = end of segment, run STT now
}

/**
 * Maps an AudioEncoding to the filetype string expected by Mastra's voice.listen().
 *
 * Usage:
 *   const filetype = audioEncodingToFiletype(config.encoding);
 *   const transcript = await agent.voice.listen(audioStream, { filetype });
 */
export function audioEncodingToFiletype(encoding: AudioEncoding): string {
  const map: Record<AudioEncoding, string> = {
    LINEAR16: 'wav',
    MULAW: 'wav',
    OPUS: 'opus',
    MP3: 'mp3',
    WEBM_OPUS: 'webm',
    OGG_OPUS: 'ogg',
    FLAC: 'flac',
    AAC: 'm4a',
  };
  return map[encoding] ?? 'wav';
}

export interface ConversationRequest {
  message?: Message;
  feedback?: any;
  agentConfig?: AgentConfig;
  agentResponse?: AgentResponse;
  audioConfig?: AudioStreamConfig;
  audio?: AudioChunk;
}

export interface ReconnectOptions {
  /** Maximum number of reconnect attempts. Default: Infinity */
  maxRetries?: number;
  /** Initial delay before first retry in ms. Default: 500 */
  initialDelayMs?: number;
  /** Maximum delay between retries in ms. Default: 30_000 */
  maxDelayMs?: number;
  /** Apply full jitter to backoff delay. Default: true */
  jitter?: boolean;
  /** Maximum number of writes to queue during reconnect. Default: 1000 */
  maxBufferSize?: number;
  /** gRPC status codes that trigger a reconnect attempt. Default: UNAVAILABLE, DEADLINE_EXCEEDED, INTERNAL, RESOURCE_EXHAUSTED */
  retryableStatusCodes?: number[];
}

// gRPC status codes: DEADLINE_EXCEEDED=4, INTERNAL=13, UNAVAILABLE=14, RESOURCE_EXHAUSTED=8
const DEFAULT_RETRYABLE_STATUS_CODES = [4, 8, 13, 14];

function resolveReconnectOptions(options: ReconnectOptions): Required<ReconnectOptions> {
  return {
    maxRetries: options.maxRetries ?? Infinity,
    initialDelayMs: options.initialDelayMs ?? 500,
    maxDelayMs: options.maxDelayMs ?? 30_000,
    jitter: options.jitter ?? true,
    maxBufferSize: options.maxBufferSize ?? 1000,
    retryableStatusCodes: options.retryableStatusCodes ?? DEFAULT_RETRYABLE_STATUS_CODES,
  };
}

/**
 * MessagingClient provides a TypeScript interface to the Astro Messaging gRPC service
 */
export class MessagingClient extends EventEmitter {
  private client: any;
  private conversationStream: ConversationStream | null = null;
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
   * Connect with automatic retry on failure (exponential backoff).
   * Emits 'reconnecting' before each retry and 'reconnected' on success after failures.
   */
  async connectWithRetry(options: ReconnectOptions = {}): Promise<void> {
    const opts = resolveReconnectOptions(options);
    let retryCount = 0;

    while (true) {
      try {
        await this.connect();
        if (retryCount > 0) {
          this.emit('reconnected', { attempt: retryCount });
        }
        return;
      } catch (err: any) {
        if (retryCount >= opts.maxRetries) {
          throw err;
        }
        const base = Math.min(opts.initialDelayMs * Math.pow(2, retryCount), opts.maxDelayMs);
        const delayMs = opts.jitter ? base * (0.5 + Math.random() * 0.5) : base;
        this.emit('reconnecting', { attempt: retryCount + 1, reason: err, delayMs });
        await new Promise(resolve => setTimeout(resolve, delayMs));
        retryCount++;
      }
    }
  }

  /**
   * Create a bidirectional conversation stream with optional reconnect support
   */
  createConversationStream(options?: ReconnectOptions): ConversationStream {
    if (!this.isConnected) {
      throw new Error('Client not connected. Call connect() first.');
    }

    const factory = () => this.client.ProcessConversation();
    this.conversationStream = new ConversationStream(factory, options);
    return this.conversationStream;
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
 * ConversationStream wraps a bidirectional gRPC stream with automatic reconnection.
 *
 * Events:
 *   - 'response'     — AgentResponse received from server
 *   - 'reconnecting' — { attempt, reason, delayMs } — before each retry delay
 *   - 'reconnected'  — { attempt } — after a successful stream recreation
 *   - 'error'        — non-retryable error OR max retries exceeded
 *   - 'end'          — only on intentional close(), not on unexpected stream drop
 */
export class ConversationStream extends EventEmitter {
  private stream: any;
  private writeBuffer: ConversationRequest[] = [];
  private reconnecting = false;
  private closed = false;
  private retryCount = 0;
  private readonly opts: Required<ReconnectOptions>;

  constructor(private streamFactory: () => any, options: ReconnectOptions = {}) {
    super();
    this.opts = resolveReconnectOptions(options);
    this.stream = this.streamFactory();
    this.attachHandlers(this.stream);
  }

  private attachHandlers(stream: any): void {
    stream.on('data', (response: any) => {
      this.retryCount = 0;

      // Emit audio-specific events if present.
      // The server sends audio data through the bidi stream as AgentResponse
      // messages with audioConfig or audioChunk payloads. We emit dedicated
      // events for these so the agent can handle audio separately from text,
      // while still emitting the generic 'response' event for observability.
      if (response.audioConfig) {
        this.emit('audioConfig', response.audioConfig as AudioStreamConfig);
      } else if (response.audioChunk) {
        this.emit('audioChunk', response.audioChunk as AudioChunk);
      }

      this.emit('response', response as AgentResponse);
    });

    stream.on('error', (error: any) => {
      if (!this.closed && this.isRetryable(error)) {
        this.scheduleReconnect(error);
      } else {
        this.emit('error', error);
      }
    });

    stream.on('end', () => {
      if (!this.closed) {
        this.scheduleReconnect(new Error('Stream ended unexpectedly'));
      }
      // If closed, 'end' was already emitted by end() — do nothing
    });
  }

  private isRetryable(error: any): boolean {
    return this.opts.retryableStatusCodes.includes(error.code);
  }

  private calculateDelay(): number {
    const base = Math.min(this.opts.initialDelayMs * Math.pow(2, this.retryCount), this.opts.maxDelayMs);
    return this.opts.jitter ? base * (0.5 + Math.random() * 0.5) : base;
  }

  private scheduleReconnect(reason: Error): void {
    if (this.reconnecting || this.closed) return;
    if (this.retryCount >= this.opts.maxRetries) {
      this.emit('error', new Error(`Max reconnection attempts (${this.opts.maxRetries}) exceeded`));
      return;
    }
    const delayMs = this.calculateDelay();
    this.reconnecting = true;
    this.emit('reconnecting', { attempt: this.retryCount + 1, reason, delayMs });
    setTimeout(() => this.doReconnect(), delayMs);
  }

  private doReconnect(): void {
    if (this.closed) return;
    this.retryCount++;
    try {
      this.stream = this.streamFactory();
      this.attachHandlers(this.stream);
      this.reconnecting = false;
      this.emit('reconnected', { attempt: this.retryCount });
      this.flushBuffer();
    } catch (err: any) {
      this.reconnecting = false;
      this.scheduleReconnect(err);
    }
  }

  private flushBuffer(): void {
    const toFlush = this.writeBuffer.splice(0);
    for (const request of toFlush) {
      this.stream.write(request);
    }
  }

  private write(request: ConversationRequest): void {
    if (this.reconnecting || this.closed) {
      if (this.writeBuffer.length >= this.opts.maxBufferSize) {
        this.writeBuffer.shift(); // drop oldest
      }
      this.writeBuffer.push(request);
    } else {
      this.stream.write(request);
    }
  }

  /**
   * Send a message through the stream
   */
  sendMessage(message: Message): void {
    this.write({ message });
  }

  /**
   * Send platform feedback through the stream
   */
  sendFeedback(feedback: any): void {
    this.write({ feedback });
  }

  /**
   * Send agent configuration through the stream
   */
  sendAgentConfig(config: AgentConfig): void {
    this.write({ agentConfig: config });
  }

  /**
   * Send a typed AgentResponse through the stream
   */
  sendAgentResponse(response: AgentResponse): void {
    this.write({ agentResponse: response });
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
   * Send a transcript of the user's audio input back to the platform.
   *
   * After the agent runs STT on the audio, it calls this to send the transcribed
   * text back to the platform (web adapter). The platform uses it to replace the
   * "[audio]" placeholder message with the actual spoken text in the chat UI.
   *
   * @param conversationId - The conversation this transcript belongs to
   * @param text - The transcribed text from STT
   * @param messageId - Optional: the original "[audio]" message ID to update
   * @param language - Optional: BCP-47 language detected by STT (e.g. "en-US")
   */
  sendTranscript(conversationId: string, text: string, messageId?: string, language?: string): void {
    this.sendAgentResponse({
      conversationId,
      transcript: { text, messageId, language },
    });
  }

  // --- Audio support ---
  //
  // These methods handle sending audio data through the gRPC bidi stream.
  // Two directions:
  //   - Agent → Server (sendAudioConfig/sendAudioChunk/endAudio): used when the
  //     agent needs to forward audio upstream (less common)
  //   - Server → Agent (audioConfig/audioChunk events + audioAsReadable): the main
  //     path where the server forwards client mic audio to the agent for STT

  /**
   * Send an audio stream config through the bidi stream.
   * Must be called before sendAudioChunk() so the receiver knows the encoding.
   */
  sendAudioConfig(config: AudioStreamConfig): void {
    this.write({ audioConfig: config });
  }

  /**
   * Send a raw audio chunk through the bidi stream.
   * The chunk's sequence number should increase monotonically.
   */
  sendAudioChunk(chunk: AudioChunk): void {
    this.write({ audio: chunk });
  }

  /**
   * Signal end of the current audio segment by sending an empty chunk with done=true.
   * The receiver should process all accumulated audio (e.g. run STT).
   * After this, more audio can follow — either new config or more chunks.
   */
  endAudio(): void {
    this.write({ audio: { data: Buffer.alloc(0), done: true } });
  }

  /**
   * Converts incoming audioChunk events into a Web Streams API ReadableStream.
   *
   * This is the primary integration point with Mastra's voice system. The agent
   * listens for the 'audioConfig' event to know the format, then calls this
   * method to get a stream it can pass directly to voice.listen():
   *
   * ```typescript
   * conversation.on('audioConfig', async (config) => {
   *   const audioStream = conversation.audioAsReadable();
   *   const filetype = audioEncodingToFiletype(config.encoding);
   *   const transcript = await agent.voice.listen(audioStream, { filetype });
   *   // ... process transcript
   * });
   * ```
   *
   * The ReadableStream:
   * - Yields Uint8Array chunks as audioChunk events arrive
   * - Closes when an AudioChunk with done=true arrives (end of segment)
   * - Closes when the ConversationStream emits 'end' (intentional close)
   * - Errors when the ConversationStream emits 'error'
   * - Properly cleans up all event listeners on close, error, or cancel
   *
   * @returns A ReadableStream<Uint8Array> suitable for Mastra voice.listen()
   */
  audioAsReadable(): ReadableStream<Uint8Array> {
    // Centralized cleanup to prevent listener leaks. Called on:
    // - done=true chunk (normal completion)
    // - stream 'end' event (intentional close)
    // - stream 'error' event
    // - ReadableStream cancel() (consumer gave up, e.g. reader.cancel())
    const cleanup = () => {
      this.removeListener('audioChunk', onChunk);
      this.removeListener('end', onEnd);
      this.removeListener('error', onError);
    };

    const onChunk = (chunk: AudioChunk) => {
      if (chunk.done) {
        cleanup();
        try { controller.close(); } catch {}
      } else {
        controller.enqueue(new Uint8Array(chunk.data));
      }
    };

    const onEnd = () => {
      cleanup();
      try { controller.close(); } catch {}
    };

    const onError = (err: Error) => {
      cleanup();
      try { controller.error(err); } catch {}
    };

    let controller: ReadableStreamDefaultController<Uint8Array>;

    return new ReadableStream({
      start: (ctrl) => {
        controller = ctrl;
        this.on('audioChunk', onChunk);
        this.once('end', onEnd);
        this.once('error', onError);
      },
      cancel: () => {
        // Consumer cancelled (e.g. reader.cancel()) — remove all listeners
        // to prevent memory leaks
        cleanup();
      },
    });
  }

  /**
   * End the stream intentionally. Emits 'end' and prevents any further reconnects.
   */
  end(): void {
    this.closed = true;
    this.writeBuffer = [];
    this.stream.end();
    this.emit('end');
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
