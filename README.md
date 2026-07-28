# Astropods Messaging

Go messaging service that connects AI agents to messaging platforms via gRPC bidirectional streaming. Ships with Go and TypeScript client SDKs.

## Features

- **gRPC bidirectional streaming** — real-time message flow between agents and platforms
- **Slack adapter** — Socket Mode, AI status indicators, suggested prompts, rate limiting, observe-channel mode
- **Web adapter** — HTTP/SSE for browser-based clients
- **Thread history** — tracks edits and deletions in memory
- **Storage** — Redis or in-memory conversation store
- **Chat persistence** — deployment web-chat history in a deployment-local SQLite store, durable on the agent's shared disk (see `CHAT_DB_PATH`)
- **Multi-arch Docker image** — `linux/amd64` and `linux/arm64`

## Project Structure

```
messaging/
├── cmd/server/                  # Server entrypoint
├── config/                      # Configuration (env-var driven, see config.go)
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

- Go 1.25+
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
docker build -t astro-messaging .

docker run \
  -e SLACK_BOT_TOKEN=xoxb-your-token \
  -e SLACK_APP_TOKEN=xapp-your-token \
  -p 9090:9090 \
  astro-messaging
```

The web adapter serves the HTTP/SSE API only; the chat UI is provided by the
astro-client SPA, which talks to this API over the same-origin `/chat/*` proxy.

## Configuration

All config via environment variables:

```bash
# Slack
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
SLACK_RATE_LIMIT_RPS=0.33     # 3s minimum between messages
SLACK_RATE_LIMIT_BURST=10

# Slack behavioural settings (JSON). All keys are optional.
SLACK_CONFIG='{
  "socket_mode": true,
  "auto_thread": true,
  "actionable_reactions": ["ticket"],
  "allowed_channel_ids": [],
  "allowed_user_ids": [],
  "observe_channel_ids": []
}'

# gRPC
GRPC_ENABLED=true
GRPC_LISTEN_ADDR=:9090
GRPC_MAX_STREAMS=100

# Web adapter
WEB_ENABLED=false
WEB_LISTEN_ADDR=:8080
WEB_ALLOWED_ORIGINS=*

# Storage: "memory" (default) or "redis"
STORAGE_TYPE=memory
REDIS_URL=redis://localhost:6379

# Thread history
THREAD_HISTORY_MAX_SIZE=1000
THREAD_HISTORY_MAX_MESSAGES=50
THREAD_HISTORY_TTL_HOURS=24

# Chat persistence (deployment web chat)
# SQLite file backing the chat-page API (conversations + messages). In deployed
# sidecars astro-server sets this to a path on the agent's shared persistent
# disk, so history survives pod reschedules. Unset (default) disables
# persistence — used for local dev.
CHAT_DB_PATH=

# Logging: debug (default), info, warn, error
LOG_LEVEL=debug
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
| `messaging_messages_dropped_total` | Counter | `platform`, `reason` | Messages dropped before reaching an agent (`no_agent`, `allowlist`, `bot_filtered`, `app_mention_dedup`) |
| `messaging_slack_events_total` | Counter | `event_type` | Slack events by type: `dm`, `thread_reply`, `mention`, `reaction`, `observed_top` |
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

### Python

```bash
pip install astropods-messaging
```

Requires Python 3.10+. This is a low-level package — if you're building an agent, use a higher-level adapter (e.g. `astropods-adapter-langchain`) which depends on this automatically.

Use it directly when implementing a custom adapter:

```python
import grpc
from astropods_messaging import (
    AgentMessagingStub,
    AgentResponse,
    ContentChunk,
)

channel = grpc.insecure_channel("localhost:9090")
stub = AgentMessagingStub(channel)

def process(requests):
    for request in requests:
        yield AgentResponse(
            conversation_id=request.conversation_id,
            content=ContentChunk(content="hello", type=ContentChunk.END),
        )

stub.ProcessConversation(process(stub.ProcessConversation(...)))
```

SDK source lives in `sdk/python/`. Published to PyPI as `astropods-messaging`.

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

## Build & Publish

### Docker (`astropods/messaging`)

The Docker image is built and published to Docker Hub via `.github/workflows/build.yml`. It uses a 2-stage build: Go builds the static binary, and a slim Debian image ships it.

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

### PyPI (`astropods-messaging`)

The Python SDK is published to PyPI via `.github/workflows/publish-pypi.yml`.

Triggered automatically when a GitHub release is published, or manually via **Actions → Publish PyPI package → Run workflow**.

Uses PyPA trusted publishing — no token storage required.

## Slack App Setup

1. Create an app at https://api.slack.com/apps
2. Enable **Socket Mode** and generate an app-level token (`connections:write` scope)
3. Add bot token scopes: `chat:write`, `channels:history`, `groups:history`, `im:history`, `app_mentions:read`
4. Subscribe to events: `message.channels`, `message.groups`, `message.im`, `app_mention`
5. Install to workspace

### Observe channels

By default the Slack adapter only forwards messages where the user addressed the bot (DMs, `@`-mentions, thread replies, button clicks, reactions). Set `observe_channel_ids` in `SLACK_CONFIG` to also forward every top-level message in specific channels — useful for watcher / classifier agents.

```json
{ "observe_channel_ids": ["C0123ABCDEF"] }
```

Behaviour in an observe channel:

- Top-level messages are forwarded with `PlatformContext.trigger = TRIGGER_OBSERVED`.
- Posts that `@`-mention the bot still flow through `app_mention` only (no double-delivery).
- Slack retries / rapid duplicates are suppressed by an in-memory dedup (2-minute window).
- Per-user authz is bypassed — the user did not address the bot, so operator-configured grants don't apply. Tighten `allowed_channel_ids` if you need a channel-level gate.

Every outbound `pb.Message` from the Slack adapter also carries `PlatformContext.bot_user_id` (resolved once at init from `auth.test`). The adapter strips `<@bot>` from `app_mention` content; this field lets the agent still detect "I was mentioned" — including on observed traffic.
