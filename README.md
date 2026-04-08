# Astropods Messaging

Go messaging service that connects AI agents to messaging platforms via gRPC bidirectional streaming. Ships with Go and TypeScript client SDKs.

## Features

- **gRPC bidirectional streaming** — real-time message flow between agents and platforms
- **Slack adapter** — Socket Mode, AI status indicators, suggested prompts, rate limiting
- **Web adapter** — HTTP/SSE for browser-based clients
- **Playground UI** — bundled browser-based chat interface, served from `/` when enabled
- **Thread history** — tracks edits and deletions in memory
- **Storage** — Redis or in-memory conversation store
- **Multi-arch Docker image** — `linux/amd64` and `linux/arm64`

## Project Structure

```
messaging/
├── cmd/server/                  # Server entrypoint
├── config/                      # Configuration (env-var driven, see config.go)
├── internal/
│   ├── adapter/                 # Platform adapter interface
│   │   ├── slack/               # Slack Socket Mode adapter
│   │   └── web/                 # HTTP/SSE adapter + embedded playground UI
│   ├── grpc/                    # gRPC server
│   ├── store/                   # Redis + in-memory stores
│   └── version/                 # Build-time version info
├── pkg/
│   ├── client/                  # Go client SDK
│   ├── gen/astro/messaging/v1/  # Generated protobuf types
│   └── types/                   # Shared Go types
├── playground/                  # Playground UI submodule (astropods/playground)
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
- Prometheus metrics on `:9091`

### Run with Docker

```bash
# Initialise the playground submodule, then build
git submodule update --init
docker build -t astro-messaging .

docker run \
  -e SLACK_BOT_TOKEN=xoxb-your-token \
  -e SLACK_APP_TOKEN=xapp-your-token \
  -p 9090:9090 \
  astro-messaging
```

### Run with the playground UI

The playground is bundled into the Docker image. Enable it at runtime — no separate container needed:

```bash
docker run \
  -e WEB_ENABLED=true \
  -e WEB_SERVE_PLAYGROUND=true \
  -e STORAGE_TYPE=memory \
  -p 8080:8080 -p 9090:9090 \
  astro-messaging
```

Open `http://localhost:8080` to access the playground. The UI and API share the same origin so no CORS or proxy configuration is required.

## Configuration

All config via environment variables:

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

# Web adapter
WEB_ENABLED=false
WEB_LISTEN_ADDR=:8080
WEB_ALLOWED_ORIGINS=*
WEB_SERVE_PLAYGROUND=false    # serve the bundled playground UI from /

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

## Metrics

Prometheus metrics are exposed on a dedicated port (default `:9091`):

```bash
METRICS_LISTEN_ADDR=:9091  # default, override if needed

# To disable:
METRICS_ENABLED=false
```

Scrape endpoint: `http://<host>:9091/metrics`

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `messaging_messages_received_total` | Counter | `platform` | Messages that passed adapter filtering and reached the gRPC layer |
| `messaging_messages_forwarded_total` | Counter | `platform` | Messages successfully sent to an agent stream |
| `messaging_messages_dropped_total` | Counter | `platform`, `reason` | Messages dropped before reaching an agent (`no_agent`, `allowlist`, `bot_filtered`) |
| `messaging_slack_events_total` | Counter | `event_type` | Slack events by type: `dm`, `thread_reply`, `mention`, `reaction` |
| `messaging_agent_responses_total` | Counter | `type` | Agent responses routed back to platform adapters |
| `messaging_routing_errors_total` | Counter | `adapter` | Errors delivering agent responses to adapters |
| `messaging_active_streams` | Gauge | — | Currently open bidirectional gRPC agent streams |
| `messaging_web_active_connections` | Gauge | — | Currently open SSE client connections |
| `messaging_message_latency_seconds` | Histogram | `platform` | Time from message receipt to successful agent forwarding |

### Grafana Alloy

Add a scrape job to your Alloy config (`config.alloy`):

```hcl
prometheus.scrape "astro_messaging" {
  targets = [{
    __address__ = "localhost:9091",
  }]
  forward_to = [prometheus.remote_write.default.receiver]
}
```

If the container and Alloy are on the same Docker network, use the container name instead of `localhost`:

```hcl
targets = [{
  __address__ = "messaging:9091",
}]
```

To override the scrape interval or attach labels:

```hcl
prometheus.scrape "astro_messaging" {
  targets = [{
    __address__ = "messaging:9091",
    service     = "astro-messaging",
  }]
  scrape_interval = "30s"
  forward_to      = [prometheus.remote_write.default.receiver]
}
```

### Useful queries

```promql
# Messages dropped because no agent is connected
rate(messaging_messages_dropped_total{reason="no_agent"}[5m])

# Forwarded message throughput by platform
rate(messaging_messages_forwarded_total[5m])

# p95 end-to-end latency
histogram_quantile(0.95, rate(messaging_message_latency_seconds_bucket[5m]))

# Slack event breakdown
rate(messaging_slack_events_total[5m])
```

## Agent SDKs

### Go

```bash
go get github.com/astropods/messaging/pkg/client
```

```go
import (
    "github.com/astropods/messaging/pkg/client"
    pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
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
bun add @astropods/messaging
# or
npm install @astropods/messaging
```

SDK source lives in `sdk/node/`. See its `src/messaging-client.ts` for the full API.

## Development

### Playground submodule

The playground UI lives in a separate repo and is referenced as a git submodule at `playground/`. Initialise it before building:

```bash
git submodule update --init
```

To build the playground assets locally and embed them in a Go binary (for testing without Docker):

```bash
cd playground && bun install && bun run build && cd ..
# dist/ is now at internal/adapter/web/dist/ — go build picks it up
cp -r playground/dist internal/adapter/web/dist
WEB_ENABLED=true WEB_SERVE_PLAYGROUND=true go run cmd/server/main.go
```

To update to the latest playground:

```bash
git submodule update --remote playground
git commit -m "chore: bump playground submodule"
```

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

## Build & Publish

### Docker (`astropods/messaging`)

The Docker image is built and published to Docker Hub via `.github/workflows/build.yml`. It uses a 3-stage build: Bun compiles the playground UI, Go embeds the output and builds the binary, and a slim Debian image ships the final binary. CI checks out submodules recursively so the playground source is available during the build.

Images are built for `linux/amd64` and `linux/arm64` in parallel and merged into a single manifest.

Triggered manually via **Actions → Build & Push → Run workflow**.

Requires two GitHub secrets:
- `DOCKERHUB_USERNAME`
- `DOCKERHUB_TOKEN`

### npm (`@astropods/messaging`)

The TypeScript SDK is published to npm via `.github/workflows/publish-npm.yml`.

Triggered automatically when a GitHub release is published, or manually via **Actions → Publish npm package → Run workflow**.

The workflow:
1. Reads the version from `VERSION` and syncs it into `sdk/node/package.json`
2. Builds and tests the SDK
3. Publishes with provenance (`npm publish --provenance --access public`)
4. Commits the version bump and tags the release

> **Note:** A brand new package must be published manually once before the GitHub Action can take over.

## Slack App Setup

1. Create an app at https://api.slack.com/apps
2. Enable **Socket Mode** and generate an app-level token (`connections:write` scope)
3. Add bot token scopes: `chat:write`, `channels:history`, `groups:history`, `im:history`, `app_mentions:read`
4. Subscribe to events: `message.channels`, `message.groups`, `message.im`, `app_mention`
5. Install to workspace
