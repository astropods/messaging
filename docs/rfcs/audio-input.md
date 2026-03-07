# RFC: Audio Input Support

**Status:** Draft
**Author:** saswat
**Date:** 2026-03-07
**Branch:** `audio`

## Summary

Add raw audio streaming to the messaging system so that any frontend (browser, phone call, mobile app, SIP client) can send audio that is delivered as a `ReadableStream` to a Mastra agent's `voice.listen()` method. The messaging system is a pass-through — it does not perform speech-to-text or any transcoding. Mastra handles STT via its voice provider abstraction.

## Motivation

The messaging system currently supports text input via Slack and HTTP/SSE (web). Many use cases require voice input:

- Browser-based voice chat (customer support widgets, accessibility)
- Phone calls via Twilio / Vonage / Telnyx
- Mobile apps with native mic recording
- SIP / VoIP integrations
- Voice-enabled kiosks or hardware devices

All of these produce raw audio bytes. Rather than building STT into the messaging layer, we pass audio through to the agent, which delegates to Mastra's `voice.listen()`. This keeps the messaging system simple and lets Mastra handle provider choice (Whisper, Deepgram, Google STT, etc.).

## Design Principles

1. **Audio is a modality, not a platform.** It is not a new adapter — it extends the existing Web adapter with a WebSocket endpoint and adds thin telephony handlers.
2. **Pass-through, not processing.** No transcoding, no VAD, no STT server-side. Raw bytes in, raw bytes forwarded.
3. **Same response path.** Agent responses come back as the existing `AgentResponse` (text `ContentChunk`s). No TTS.
4. **Format-agnostic.** Accept whatever the frontend sends. Tag it with encoding metadata so the agent/Mastra knows how to decode it.

## Frontend Compatibility

| Frontend | Audio format | Protocol | Notes |
|----------|-------------|----------|-------|
| Browser | WebM/Opus (MediaRecorder default) | WebSocket | Most common case |
| Twilio | mulaw 8kHz mono (base64-encoded) | WebSocket (Twilio connects to us) | Twilio Media Streams protocol |
| Vonage/Nexmo | PCM 16-bit 16kHz | WebSocket (Vonage connects to us) | |
| React Native / mobile | PCM or AAC | WebSocket | Platform-dependent |
| SIP / FreeSWITCH | PCM / G.711 | WebSocket or RTP bridge | Requires a WebSocket shim for RTP |
| Alexa | Opus | HTTPS POST (batch) | Single request, not streaming |
| Desktop (Electron) | WebM/Opus (same as browser) | WebSocket | Uses browser APIs |

## Protocol

### Wire Format (WebSocket)

The client opens a WebSocket connection and sends:

1. **First message** (JSON text frame): session configuration
2. **Subsequent messages** (binary frames): raw audio bytes
3. **Final message** (JSON text frame): end-of-stream signal

Server sends back responses as JSON text frames (same event format as SSE).

#### Session Config (first message)

```json
{
  "type": "audio.config",
  "encoding": "webm_opus",
  "sample_rate": 48000,
  "channels": 1,
  "language": "en-US",
  "source": "browser"
}
```

#### Audio Data (subsequent messages)

Binary WebSocket frames containing raw audio bytes. No wrapping, no JSON — just the bytes from `MediaRecorder.ondataavailable`, a Twilio media payload (after base64 decode), or a native mic buffer.

#### End of Audio Segment

```json
{
  "type": "audio.end"
}
```

Transport-level signal meaning "process what you have." This is not utterance detection — the messaging system does no VAD or silence detection. The **client** decides when to send this based on its own UX:

- **Push-to-talk**: user releases the button
- **Client-side VAD**: e.g. `@ricky0123/vad-web` detects silence
- **Twilio**: `stop` event or call hangup
- **File upload**: sent implicitly after the entire file

After `audio.end`, the server forwards the accumulated audio to the agent. The client can then send more audio — either binary frames directly (reuses previous config) or a new `audio.config` if parameters changed (e.g., switching languages). One WebSocket connection = one conversation session with any number of segments.

#### Server Responses

Same JSON format as SSE events (chunk, status, finish, error). Sent as text frames.

```json
{"event": "status", "data": {"type": "status", "status": "THINKING"}}
{"event": "chunk",  "data": {"type": "chunk", "content": "Hello", "chunk_type": "start"}}
{"event": "chunk",  "data": {"type": "chunk", "content": " world", "chunk_type": "delta"}}
{"event": "finish", "data": {"type": "finish", "response_id": "abc-123"}}
```

## Protobuf Changes

### New file: `proto/astro/messaging/v1/audio.proto`

```protobuf
syntax = "proto3";
package astro.messaging.v1;

option go_package = "github.com/postman/astro/messaging/v1;messagingv1";

// Audio encoding format — covers browser, telephony, and mobile sources
enum AudioEncoding {
  AUDIO_ENCODING_UNSPECIFIED = 0;
  LINEAR16 = 1;          // PCM signed 16-bit little-endian (universal baseline)
  MULAW = 2;             // G.711 mu-law (Twilio, telephony)
  OPUS = 3;              // Raw Opus frames
  MP3 = 4;               // MP3 (batch uploads)
  WEBM_OPUS = 5;         // WebM container with Opus (browser MediaRecorder default)
  OGG_OPUS = 6;          // OGG container with Opus
  FLAC = 7;              // FLAC lossless
  AAC = 8;               // AAC (iOS native recording)
}

// Sent once at the start of an audio stream to configure the session
message AudioStreamConfig {
  AudioEncoding encoding = 1;
  int32 sample_rate = 2;        // Hz: 8000, 16000, 24000, 44100, 48000
  int32 channels = 3;           // 1 = mono (speech default), 2 = stereo
  string language = 4;          // BCP-47 hint, e.g. "en-US" (optional)
  string conversation_id = 5;   // Links audio to an existing conversation

  // Source metadata — helps the agent pick the right STT config
  string source = 6;            // "browser", "twilio", "vonage", "mobile", "sip"
}

// A chunk of raw audio bytes
message AudioChunk {
  bytes data = 1;               // Raw audio bytes
  int64 sequence = 2;           // Monotonic sequence number for ordering
  bool done = 3;       // Final chunk — close the audio stream
}
```

### Changes to `proto/astro/messaging/v1/service.proto`

```protobuf
import "astro/messaging/v1/audio.proto";

service AgentMessaging {
  // ... existing RPCs ...

  // Audio: client streams raw audio, server responds with text
  // First message MUST be AudioStreamConfig, rest are AudioChunks
  rpc ProcessAudioStream(stream AudioStreamRequest)
      returns (stream AgentResponse);
}

// Wrapper for the audio stream
message AudioStreamRequest {
  oneof request {
    AudioStreamConfig config = 1;    // First message: session setup
    AudioChunk audio = 2;            // Subsequent: audio data
  }
}
```

### Changes to `ConversationRequest` (for bidi streams)

For agents using `ProcessConversation`, audio can be sent inline alongside text:

```protobuf
message ConversationRequest {
  oneof request {
    Message message = 1;
    PlatformFeedback feedback = 2;
    AgentConfig agent_config = 3;
    AgentResponse agent_response = 4;
    AudioStreamConfig audio_config = 5;  // Start audio within conversation
    AudioChunk audio = 6;                // Audio data within conversation
  }
}
```

## Go Changes

### Capabilities

```go
// capabilities.go
type AdapterCapabilities struct {
    // ... existing fields ...

    SupportsAudioInput bool     // Can receive audio streams
    AudioEncodings     []string // Accepted encodings
}

func WebCapabilities() AdapterCapabilities {
    return AdapterCapabilities{
        // ... existing ...
        SupportsAudioInput: true,
        AudioEncodings:     []string{"webm_opus", "opus", "linear16"},
    }
}
```

### Web Adapter: WebSocket Audio Route

New route on the existing web adapter HTTP server:

```
WS /api/conversations/{id}/audio
```

The handler:

1. Upgrades to WebSocket (gorilla/websocket or nhooyr.io/websocket)
2. Reads first text message as `AudioStreamConfig` JSON
3. Opens a `ProcessAudioStream` gRPC client stream
4. Sends `AudioStreamConfig` as first protobuf message
5. Reads binary frames in a loop, wraps as `AudioChunk`, sends to gRPC
6. Reads gRPC responses in a parallel goroutine, sends as JSON text frames back to the WebSocket
7. On `audio.end` text message or WebSocket close, sends `AudioChunk{done: true}` and closes gRPC stream

New files:
- `internal/adapter/web/audio.go` — WebSocket audio handler
- `internal/adapter/web/audio_test.go`

### Telephony Handlers

Thin translators that normalize provider-specific WebSocket protocols into the same `AudioStreamConfig` + `AudioChunk` pipeline.

#### Twilio Media Streams

Twilio connects to us via WebSocket. Its protocol sends JSON messages:

```json
{"event": "connected", "protocol": "Call", "version": "1.0.0"}
{"event": "start", "streamSid": "...", "start": {"mediaFormat": {"encoding": "audio/x-mulaw", "sampleRate": 8000, "channels": 1}}}
{"event": "media", "media": {"payload": "<base64 audio>"}}
{"event": "stop"}
```

The handler translates:
- `start` -> `AudioStreamConfig{encoding: MULAW, sample_rate: 8000, channels: 1, source: "twilio"}`
- `media` -> base64 decode payload -> `AudioChunk{data: decoded}`
- `stop` -> `AudioChunk{done: true}`

Route: `WS /api/telephony/twilio/stream`

New files:
- `internal/adapter/telephony/twilio.go`
- `internal/adapter/telephony/twilio_test.go`

#### Vonage

Similar pattern. Vonage sends raw PCM as binary frames after an initial HTTP request configures the stream.

Route: `WS /api/telephony/vonage/stream`

New files:
- `internal/adapter/telephony/vonage.go`

## Node SDK Changes

### ConversationStream

New events emitted when audio messages arrive on the bidi stream:

```typescript
// New events
interface ConversationStreamEvents {
  audioConfig: (config: AudioStreamConfig) => void;
  audioChunk: (chunk: AudioChunk) => void;
}
```

### Helper: `audioAsReadable()`

Collects `audioChunk` events into a Node.js `ReadableStream` suitable for passing to Mastra:

```typescript
class ConversationStream extends EventEmitter {
  // ... existing ...

  /**
   * Returns a ReadableStream of audio bytes from the remote client.
   * The stream closes when an AudioChunk with done=true arrives.
   */
  audioAsReadable(): ReadableStream<Uint8Array> {
    return new ReadableStream({
      start: (controller) => {
        this.on('audioChunk', (chunk: AudioChunk) => {
          if (chunk.endOfStream) {
            controller.close();
          } else {
            controller.enqueue(new Uint8Array(chunk.data));
          }
        });

        this.on('end', () => {
          try { controller.close(); } catch {}
        });

        this.on('error', (err) => {
          try { controller.error(err); } catch {}
        });
      },
    });
  }
}
```

### Dedicated `ProcessAudioStream` wrapper

For agents that only do audio (no bidirectional text):

```typescript
class AudioStream extends EventEmitter {
  private stream: grpc.ClientReadableStream<AgentResponse>;

  constructor(streamFactory: () => grpc.ClientDuplexStream<AudioStreamRequest, AgentResponse>) {
    // Same reconnect pattern as ConversationStream
  }

  /** Send session config (must be called first) */
  configure(config: AudioStreamConfig): void;

  /** Send an audio chunk */
  sendAudio(data: Buffer | Uint8Array, sequence?: number): void;

  /** Signal end of audio */
  endAudio(): void;

  /** Get a ReadableStream for Mastra */
  // (not needed here — this is the sending side)
}
```

### Agent-side usage

```typescript
import { ConversationStream } from '@astro/messaging';
import { Agent } from '@mastra/core/agent';

const agent = new Agent({ voice: openaiVoice, /* ... */ });

conversation.on('audioConfig', async (config) => {
  const audioStream = conversation.audioAsReadable();

  // Mastra handles STT
  const transcript = await agent.voice.listen(audioStream, {
    filetype: encodingToMastraFiletype(config.encoding),
  });

  // Process as normal text
  const response = await agent.generate(transcript);

  // Send text response back through existing ContentChunk flow
  conversation.write({
    agentResponse: {
      conversationId: config.conversationId,
      content: { type: 'START', content: response.text },
    },
  });
});
```

## Encoding-to-Filetype Mapping

For passing to Mastra's `voice.listen()`:

| AudioEncoding | Mastra filetype |
|---------------|-----------------|
| LINEAR16 | `"wav"` |
| MULAW | `"wav"` |
| OPUS | `"opus"` |
| WEBM_OPUS | `"webm"` |
| OGG_OPUS | `"ogg"` |
| MP3 | `"mp3"` |
| FLAC | `"flac"` |
| AAC | `"m4a"` |

## What This Does NOT Include

- **No TTS / audio output.** Agent responses are text only. If TTS is needed later, it would be a separate RFC.
- **No server-side transcoding.** Audio bytes pass through as-is.
- **No server-side VAD.** The client decides when speech starts/ends (push-to-talk, client-side VAD like `@ricky0123/vad-web`, or telephony provider silence detection).
- **No new adapter.** Audio extends the existing Web adapter and adds telephony handlers.

## Implementation Order

1. `proto/astro/messaging/v1/audio.proto` — new protobuf definitions
2. `service.proto` — add `ProcessAudioStream` RPC, extend `ConversationRequest`
3. `capabilities.go` — add audio fields
4. `internal/adapter/web/audio.go` — WebSocket audio handler
5. `internal/adapter/telephony/twilio.go` — Twilio Media Streams handler
6. `sdk/node/src/audio-stream.ts` — Node SDK audio support
7. `sdk/node/src/conversation-stream.ts` — add `audioAsReadable()` helper
8. `internal/adapter/telephony/vonage.go` — Vonage handler (stretch)

## Decisions

1. **Max audio duration?** Optional. Configurable server-side idle timeout (default: none). Deployments can set one if needed — not enforced by the protocol.
2. **Batch uploads?** Yes. `POST /api/conversations/{id}/audio` accepts a single audio file (multipart) for non-streaming use cases (Alexa, file uploads, pre-recorded messages). The handler reads the file, wraps it as a single `AudioChunk` with `done: true`, and sends it through the same gRPC pipeline.
3. **Multiple utterances per stream?** Yes. A single WebSocket connection supports a continuous conversation. The client sends `audio.end` to mark the end of an utterance, the server processes it and sends back a response, then the client can send a new `audio.config` (or just more binary frames if the config hasn't changed) for the next utterance. This is the natural model for phone calls, voice assistants, and any "keep talking" interface. One connection = one conversation session.
