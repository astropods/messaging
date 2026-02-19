# astro-messaging

Go messaging service that connects AI agents to messaging platforms via gRPC bidirectional streaming. Ships with Go and TypeScript client SDKs.

## Features

- **gRPC bidirectional streaming** — real-time message flow between agents and platforms
- **Slack adapter** — Socket Mode, AI status indicators, suggested prompts, rate limiting
- **Web adapter** — HTTP/SSE for browser-based clients
- **Thread history** — tracks edits and deletions in memory
- **Storage** — Redis or in-memory conversation store
- **Multi-arch Docker image** — `linux/amd64` and `linux/arm64`

## Project Structure

```
astro-messaging/
├── cmd/server/                  # Server entrypoint
├── config/                      # Configuration (config.go + config.yaml)
├── internal/
│   ├── adapter/                 # Platform adapter interface
│   │   ├── slack/               # Slack Socket Mode adapter
│   │   └── web/                 # HTTP/SSE adapter
│   ├── grpc/                    # gRPC server
│   ├── store/                   # Redis + in-memory stores
│   └── version/                 # Build-time version info
├── pkg/
│   ├── client/                  # Go client SDK
│   ├── gen/astro/messaging/v1/  # Generated protobuf types
│   └── types/                   # Shared Go types
├── proto/                       # Protobuf source definitions
├── sdk/node/                    # TypeScript SDK (published to npm)
│   └── src/
├── tools/test-serialization/    # Cross-language serialization test tool
├── Dockerfile
├── go.mod
└── VERSION                      # Single version source of truth
```

## Quick Start

### Prerequisites

- Go 1.24+
- Slack app with Socket Mode enabled (bot token + app token)

### Run locally

```bash
export SLACK_BOT_TOKEN="xoxb-your-token"
export SLACK_APP_TOKEN="xapp-your-token"

go run cmd/server/main.go
```

The server starts:
- gRPC on `:9090` (agents connect here)
- HTTP/SSE on `:8080` (web adapter)

### Run with Docker

```bash
docker build -t astro-messaging .

docker run \
  -e SLACK_BOT_TOKEN=xoxb-your-token \
  -e SLACK_APP_TOKEN=xapp-your-token \
  -p 9090:9090 \
  astro-messaging
```

## Configuration

All config via environment variables (see `config/config.yaml` for defaults):

```bash
# Slack
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
SLACK_RATE_LIMIT_RPS=0.33     # 3s minimum between messages
SLACK_RATE_LIMIT_BURST=10

# gRPC
GRPC_ENABLED=true
GRPC_LISTEN_ADDR=:9090
GRPC_MAX_STREAMS=100

# Storage: "memory" (default) or "redis"
STORAGE_TYPE=memory
REDIS_URL=redis://localhost:6379

# Thread history
THREAD_HISTORY_MAX_SIZE=1000
THREAD_HISTORY_MAX_MESSAGES=50
THREAD_HISTORY_TTL_HOURS=24

# Logging: debug, info, warn, error
LOG_LEVEL=info
```

## Agent SDKs

### Go

```bash
go get github.com/astromode-ai/astro-messaging/pkg/client
```

```go
import (
    "github.com/astromode-ai/astro-messaging/pkg/client"
    pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
)

c, err := client.NewClient("localhost:9090")
defer c.Close()

stream, err := c.ProcessConversation(ctx)
stream.ReceiveAll(func(resp *pb.AgentResponse) error {
    // handle incoming message, send response
    return nil
})
```

### TypeScript

```bash
bun add @astromode-ai/astro-messaging
# or
npm install @astromode-ai/astro-messaging
```

SDK source lives in `sdk/node/`. See its `src/messaging-client.ts` for the full API.

## Development

### Go tests

```bash
go test ./...
```

### TypeScript tests

```bash
cd sdk/node
bun install
bun test
```

### Cross-language serialization tests

```bash
# 1. Generate Go test data
go run tools/test-serialization/main.go serialize

# 2. Run TS tests (reads Go data, writes TS data)
cd sdk/node && bun test

# 3. Verify Go can read TS data
go run tools/test-serialization/main.go deserialize
```

### Regenerate protobuf

```bash
./scripts/generate-proto.sh
```

## Versioning

`VERSION` is the single source of truth for both the Go binary and the npm package. CI reads it automatically at build/publish time.

To release a new version:

1. Update `VERSION`
2. Commit and push
3. Create a GitHub release — this triggers the npm publish workflow
4. The Docker build workflow embeds the version in the binary via ldflags

## Slack App Setup

1. Create an app at https://api.slack.com/apps
2. Enable **Socket Mode** and generate an app-level token (`connections:write` scope)
3. Add bot token scopes: `chat:write`, `channels:history`, `groups:history`, `im:history`, `app_mentions:read`
4. Subscribe to events: `message.channels`, `message.groups`, `message.im`, `app_mention`
5. Install to workspace
