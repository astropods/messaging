# astropods-messaging

Generated gRPC stubs for the Astropods messaging service. Used internally by adapter packages to communicate with the Astro runtime.

## Installation

```bash
pip install astropods-messaging
```

Requires Python 3.10+.

## Usage

This package is a low-level dependency. If you're building an agent, use a higher-level adapter instead (e.g. `astropods-adapter-langchain`).

Use this package directly if you're implementing a custom adapter:

```python
from astropods_messaging import AgentMessagingStub, ConversationRequest, AgentResponse, ContentChunk
```

## Exported symbols

### Service

| Symbol | Description |
|--------|-------------|
| `AgentMessagingStub` | gRPC client stub for the `AgentMessaging` service |
| `ConversationRequest` | Incoming message from the messaging service |
| `HealthCheckRequest` / `HealthCheckResponse` | Health probe messages |

### Responses

| Symbol | Description |
|--------|-------------|
| `AgentResponse` | Top-level response wrapper sent back to the messaging service |
| `ContentChunk` | Streamed text fragment from the agent |
| `StatusUpdate` | Agent status change (e.g. `THINKING`, `PROCESSING`) |
| `Transcript` | Transcribed text from an audio input |
| `ErrorResponse` | Error to surface to the user |
| `SuggestedPrompts` | Follow-up prompt suggestions |
| `ThreadMetadata` | Metadata to attach to the conversation thread |

### Messages

| Symbol | Description |
|--------|-------------|
| `Message` | A message in the conversation |
| `User` | User identity attached to a request |
| `Attachment` | File or media attached to a message |

### Audio

| Symbol | Description |
|--------|-------------|
| `AudioStreamConfig` | Audio format configuration |
| `AudioChunk` | A chunk of raw audio data |
| `AudioEncoding` | Enum of supported audio encodings |

### Config

| Symbol | Description |
|--------|-------------|
| `AgentConfig` | Agent configuration reported to the platform |
| `AgentToolConfig` | A single tool entry within `AgentConfig` |

### Feedback

| Symbol | Description |
|--------|-------------|
| `PlatformFeedback` | User feedback event (e.g. thumbs up/down) |
