# Astro Messaging

A production-ready Go-based messaging service that enables AI agents to interact with messaging platforms (Slack, Discord, Teams). The service provides both HTTP (legacy) and **gRPC** interfaces with native support for AI features like status indicators, suggested prompts, and real-time bidirectional streaming.

## ✨ Key Features

- **gRPC Bidirectional Streaming**: Real-time communication between agents and platforms
- **Slack AI Features**: Native support for `assistant.threads.setStatus` and `assistant.threads.setSuggestedPrompts`
- **Thread History Management**: Automatic tracking of message edits and deletions
- **Multi-Platform Support**: Slack (complete), Discord & Teams (framework ready)
- **Rate Limiting**: Adaptive rate limiting per platform (Slack: 3s minimum)
- **Agent SDKs**: Go and TypeScript client libraries for building agents

## 📦 SDK Structure

This package includes SDKs for building agents:

- **Go SDK**: `pkg/client/` - See [examples/simple-agent](examples/simple-agent/)
- **TypeScript SDK**: `sdk/node/` - See [engineering-assistant-ts](../astro-agents/engineering-assistant-ts/)

Both SDKs provide the same functionality:
- gRPC client for messaging service
- Type-safe message handling
- Thread history retrieval
- Helper functions for status updates and prompts

## 🏗️ Architecture

```
┌─────────────┐
│  Slack App  │
└──────┬──────┘
       │ Socket Mode
       ▼
┌─────────────────────┐      gRPC Stream       ┌──────────────┐
│  Slack Adapter      │◄────────────────────────┤  AI Agent    │
│  - Event handling   │      (bidirectional)    │  (Your Code) │
│  - AI APIs          │─────────────────────────┤              │
│  - Rate limiting    │                         └──────────────┘
└─────────────────────┘
       │
       ▼
┌─────────────────────┐
│ Thread History      │
│ Store (In-Memory)   │
│ - Edits tracking    │
│ - Delete handling   │
└─────────────────────┘
```

### Message Flow

**Incoming (Slack → Agent)**:
1. User sends message in Slack
2. Slack Socket Mode → Slack Adapter
3. Adapter translates to proto message → gRPC server
4. gRPC server streams to agent

**Outgoing (Agent → Slack)**:
1. Agent sends proto response via gRPC stream
2. gRPC server → Slack Adapter
3. Adapter calls Slack AI APIs:
   - `assistant.threads.setStatus` (status updates)
   - `assistant.threads.setSuggestedPrompts` (quick replies)
   - `chat.postMessage` (final content)

## 🚀 Quick Start

This package provides a standalone container for the messaging service. For a complete working setup with docker-compose orchestration, see the `@astro/agents` package which includes scripts to run the full application stack (agent + messaging + Redis).

### Using gRPC (Recommended)

```bash
# Start the messaging service with gRPC enabled
export SLACK_BOT_TOKEN="xoxb-your-token"
export SLACK_APP_TOKEN="xapp-your-token"
export GRPC_ENABLED=true
export GRPC_LISTEN_ADDR=":9090"

go run cmd/server/main.go
```

The service will start:
- gRPC server on `:9090` (for agents)
- HTTP API on `:8081` (legacy)
- Slack adapter in Socket Mode

See [examples/simple-agent](examples/simple-agent/README.md) for a complete agent implementation.

### Building and Publishing with Makefile

The easiest way to build and publish the container is using the included Makefile:

```bash
# See all available commands
make help

# Check environment configuration
make check-env

# Build the container locally
make build

# Publish to GitHub Container Registry (builds, logs in, and pushes)
make publish

# Run the container locally
make run

# Clean up local images
make clean
```

**Prerequisites:**
- Docker installed and running
- GitHub Container Registry credentials in root `.env` file:
  ```bash
  GITHUB_USERNAME=your-username
  GITHUB_TOKEN=your-token
  GITHUB_REGISTRY=ghcr.io
  ```

### Manual Docker Build

You can also build manually:
```bash
# Build the container
docker build -t astro-messaging -f deployments/Dockerfile .

# Run the container with environment variables
docker run -p 8081:8081 \
  -e SLACK_BOT_TOKEN=xoxb-your-token \
  -e SLACK_APP_TOKEN=xapp-your-token \
  -e AGENT_URL=http://localhost:8080 \
  -e STORAGE_TYPE=memory \
  astro-messaging
```

### Running the Full Application Stack

See `packages/astro-agents` for docker-compose setup and orchestration scripts.

## Architecture

```
┌─────────────────────────────────────────────────┐
│              Slack Platform                     │
│         (WebSocket/Socket Mode)                 │
└─────────────────┬───────────────────────────────┘
                  │ Events
                  ▼
┌─────────────────────────────────────────────────┐
│     Go Messaging Service (:8081)                │
│  ┌───────────────────────────────────────────┐  │
│  │  SlackAdapter                             │  │
│  │  - Socket Mode Connection                 │  │
│  │  - Event Handling (message, app_mention) │  │
│  │  - Message Translation                    │  │
│  │  - Rate Limiting                          │  │
│  └───────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────┐  │
│  │  HTTP Server                              │  │
│  │  - POST /message (messaging → agent)        │  │
│  │  - POST /send-message (agent → messaging)   │  │
│  │  - PUT /message/:id (streaming updates)   │  │
│  │  - GET /health                            │  │
│  └───────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────┐  │
│  │  Conversation Store (Redis/In-Memory)     │  │
│  └───────────────────────────────────────────┘  │
└─────────────────┬───────────────────────────────┘
                  │ HTTP (localhost)
                  ▼
┌─────────────────────────────────────────────────┐
│     Agent Container (:8080)                     │
│  ┌───────────────────────────────────────────┐  │
│  │  HTTP Server (Node.js/Bun)                │  │
│  │  - POST /message endpoint                 │  │
│  └───────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────┐  │
│  │  AstroAgent                               │  │
│  │  - agent.stream() with callbacks          │  │
│  │  - Tool execution                         │  │
│  │  - Response generation                    │  │
│  └───────────────────────────────────────────┘  │
└─────────────────────────────────────────────────┘
```

## Features

- **Slack Socket Mode**: No webhooks required, easier local development
- **Streaming Responses**: Real-time message updates as agent generates responses
- **Conversation Context**: Redis-backed persistent conversation state
- **Rate Limiting**: Token bucket rate limiter respecting Slack's API limits
- **Health Checks**: Monitor messaging and adapter health
- **Docker Support**: Ready for container deployment
- **Graceful Shutdown**: Clean connection teardown

## Quick Start

### Prerequisites

- Go 1.22+
- Redis (for conversation storage)
- Slack app with Socket Mode enabled

### Local Development

1. **Clone and navigate to the messaging directory:**
   ```bash
   cd apps/astro-messaging
   ```

2. **Create environment configuration:**
   ```bash
   cp .env.example .env
   # Edit .env with your Slack tokens
   ```

3. **Start Redis:**
   ```bash
   docker run -d -p 6379:6379 redis:7-alpine
   ```

4. **Run the messaging:**
   ```bash
   go run cmd/messaging/main.go
   ```

5. **In another terminal, start the agent container:**
   ```bash
   cd ../../packages/astro-agents
   bun run slack-server
   ```

6. **Verify health:**
   ```bash
   curl http://localhost:8081/health
   curl http://localhost:8080/health
   ```

### With Full Application Stack

For docker-compose orchestration with agent and Redis, see the `@astro/agents` package:

```bash
cd packages/astro-agents
./start.sh  # Runs full stack via docker-compose
```

## Configuration

All configuration is done via environment variables in the `.env` file.

### Setup Configuration

```bash
# Copy the template
cp .env.docker .env

# Edit with your credentials
nano .env
```

### Required Variables

```bash
# Slack credentials (from https://api.slack.com/apps)
SLACK_BOT_TOKEN=xoxb-your-bot-token-here
SLACK_APP_TOKEN=xapp-your-app-token-here

# Anthropic API key (from https://console.anthropic.com/)
ANTHROPIC_API_KEY=sk-ant-your-anthropic-key-here
```

### Optional Variables

```bash
# Storage type: "redis" (default) or "memory"
STORAGE_TYPE=redis

# gRPC Server (default: enabled)
GRPC_ENABLED=true
GRPC_LISTEN_ADDR=:9090
GRPC_MAX_STREAMS=100

# Thread History Store
THREAD_HISTORY_MAX_SIZE=1000        # Max threads in memory
THREAD_HISTORY_MAX_MESSAGES=50     # Max messages per thread
THREAD_HISTORY_TTL_HOURS=24        # Time-to-live in hours

# Slack Configuration
SLACK_RATE_LIMIT_RPS=0.33          # Requests per second (3s minimum)
SLACK_RATE_LIMIT_BURST=10          # Burst size

# Logging level: debug, info, warn, error
LOG_LEVEL=info
```

See `.env.docker` for the complete template with documentation.

### Slack App Setup

1. **Create a Slack App** at https://api.slack.com/apps
2. **Enable Socket Mode:**
   - Go to Socket Mode and toggle on
   - Generate an app-level token with `connections:write` scope
3. **Add Bot Token Scopes:**
   - `chat:write` - Send messages
   - `channels:history` - Read channel history
   - `groups:history` - Read private channel history
   - `im:history` - Read DM history
   - `app_mentions:read` - Receive @mentions
4. **Subscribe to Bot Events:**
   - `message.channels`
   - `message.groups`
   - `message.im`
   - `app_mention`
5. **Install to Workspace** and copy the bot token

## API Reference

### Messaging Service Endpoints

#### Health Check
```
GET /health
```
Returns health status of messaging and adapters.

#### Send Message
```
POST /send-message
Content-Type: application/json

{
  "platform": "slack",
  "channel_id": "C123456",
  "thread_id": "1234567890.123456",
  "content": "Hello from the agent!",
  "ephemeral": false
}
```

#### Update Message (Streaming)
```
PUT /message/:message_id
Content-Type: application/json

{
  "platform": "slack",
  "content": "Updated message content..."
}
```

#### Get Conversation
```
GET /conversation/:conversation_id
```
Returns conversation context and metadata.

### Agent Endpoints

The agent container must implement:

#### Receive Message
```
POST /message
Content-Type: application/json

{
  "id": "msg-123",
  "platform": "slack",
  "content": "User message",
  "user_id": "U123456",
  "channel_id": "C123456",
  "thread_id": "1234567890.123456",
  "conversation_id": "C123456-1234567890.123456",
  "timestamp": "2026-01-15T10:30:00Z"
}
```

Expected response:
```json
{
  "content": "Agent response",
  "create_thread": true,
  "ephemeral": false
}
```

## Building Agents (gRPC Client)

The easiest way to build agents is using the provided gRPC client library.

### Installation

```bash
go get github.com/astromode-ai/astro-messaging/pkg/client
go get github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1
```

### Quick Example

```go
package main

import (
    "context"
    "log"

    "github.com/astromode-ai/astro-messaging/pkg/client"
    pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
)

func main() {
    // Connect to messaging service
    client, err := client.NewClient("localhost:9090")
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    // Create bidirectional stream
    stream, err := client.ProcessConversation(context.Background())
    if err != nil {
        log.Fatal(err)
    }

    // Receive messages and send responses
    stream.ReceiveAll(func(resp *pb.AgentResponse) error {
        // Process message and send response
        log.Printf("Received: %v", resp)
        return nil
    })
}
```

### Full Example

See [examples/simple-agent](examples/simple-agent/README.md) for a complete agent implementation with:
- Message processing
- Status updates ("Thinking...", "Generating...")
- Suggested prompts (quick replies)
- Thread history retrieval
- Error handling

### Client Features

- **Bidirectional Streaming**: Real-time message flow
- **Thread History**: `GetThreadHistory(conversationID, maxMessages)`
- **Status Updates**: Show "Thinking...", "Searching...", etc.
- **Suggested Prompts**: Send quick reply buttons
- **Content Streaming**: Stream responses token-by-token (platform dependent)
- **Error Handling**: Send error messages to users

## Development

### Project Structure

```
apps/astro-messaging/
├── cmd/
│   └── messaging/
│       └── main.go                  # Entry point
├── internal/
│   ├── adapter/
│   │   ├── adapter.go               # Base adapter interface
│   │   └── slack/
│   │       ├── adapter.go           # Slack implementation
│   │       ├── translator.go        # Message translation
│   │       └── rate_limiter.go      # Rate limiting
│   ├── api/
│   │   ├── server.go                # HTTP server
│   │   └── handlers.go              # Request handlers
│   ├── store/
│   │   ├── store.go                 # Storage interface
│   │   ├── redis.go                 # Redis implementation
│   │   └── memory.go                # In-memory implementation
│   └── agent/
│       └── client.go                # Agent HTTP client
├── pkg/
│   └── types/
│       └── message.go               # Shared types
├── config/
│   ├── config.go                    # Configuration
│   └── config.yaml                  # Default config
└── deployments/
    ├── Dockerfile                   # Container build
    └── docker-compose.yaml          # Local dev setup
```

### Running Tests

```bash
go test ./...
```

### Building

```bash
go build -o messaging ./cmd/messaging
```

## Version Management

The package version is tracked in the `VERSION` file at the root of this package. Version information is automatically embedded in the binary at build time.

### View Current Version

```bash
make version
```

### Bump Version

Use semantic versioning targets:

```bash
# Patch bump: 0.1.0 -> 0.1.1 (bug fixes)
make version-bump-patch

# Minor bump: 0.1.0 -> 0.2.0 (new features, backward compatible)
make version-bump-minor

# Major bump: 0.1.0 -> 1.0.0 (breaking changes)
make version-bump-major
```

### Versioning Strategy

- **VERSION file**: Single source of truth for the version number
- **Docker tags**: Each build creates three tags:
  - `<version>` - Semantic version (e.g., `0.1.0`)
  - `<commit>` - Git commit SHA (e.g., `ba443cf`)
  - `latest` - Always points to the most recent build
- **Binary embedding**: Version, commit SHA, and build date are embedded in the binary via ldflags
- **Git tags**: After publishing a release, tag the commit:
  ```bash
  git tag v$(cat VERSION)
  git push origin v$(cat VERSION)
  ```

### Release Process

1. Bump the version: `make version-bump-patch` (or minor/major)
2. Commit the VERSION file: `git commit -m "Bump version to $(cat VERSION)"`
3. Build and publish: `make publish`
4. Tag the release: `git tag v$(cat VERSION) && git push origin v$(cat VERSION)`

## Deployment

### Kubernetes

Example deployment manifest:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: astro-agent
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: agent
          image: your-agent:latest
          ports:
            - containerPort: 8080
        - name: messaging
          image: astro-messaging:latest
          ports:
            - containerPort: 8081
          env:
            - name: AGENT_URL
              value: "http://localhost:8080"
            - name: SLACK_BOT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: slack-credentials
                  key: bot-token
```

## Troubleshooting

### Messaging service won't connect to Slack
- Verify `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` are correct
- Check Socket Mode is enabled in Slack app settings
- Ensure app has required scopes and event subscriptions

### Messages not reaching agent
- Verify agent is running on port 8080
- Check `AGENT_URL` is correct (use `http://localhost:8080` for local dev)
- Check agent logs for errors in `/message` endpoint

### Streaming not working
- Verify agent is calling messaging's `PUT /message/:id` endpoint
- Check rate limiting isn't throttling updates too much
- Ensure Slack message IDs are correctly formatted

### Redis connection issues
- Verify Redis is running and accessible
- Check `REDIS_URL` format: `redis://host:port`
- For local dev, use in-memory storage: `STORAGE_TYPE=memory`

## Contributing

1. Create a feature branch
2. Make your changes
3. Add tests for new functionality
4. Run `go test ./...` and ensure all tests pass
5. Submit a pull request

## License

See root LICENSE file.

## Support

For questions and support, open an issue in the main Astro repository.
