# Contributing to messaging

The Astro messaging bridge: a Go/gRPC service that connects agents to chat
surfaces (Slack, web) plus client SDKs for Go, TypeScript, and Python. The wire
contract is protobuf under `proto/astro/messaging/v1/`.

You only need this repo to build, test, and contribute.

## Layout

- `cmd/server/` — service entrypoint
- `internal/` — `adapter/` (`slack/`, `web/` HTTP+SSE API), `grpc/`, `store/`
  (Redis + in-memory), `authz/`, `langfuse/`, `metrics/`
- `pkg/` — `client/` (Go SDK), `gen/…/v1/` (generated protobuf), `types/`
- `proto/astro/messaging/v1/` — `.proto` source (the source of truth)
- `sdk/node/` — TypeScript SDK (`@astropods/messaging`)
- `sdk/python/` — Python SDK (`astropods-messaging`)

## Prerequisites

- Go 1.25+
- `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH` (proto codegen)
- Bun — only for the TypeScript SDK
- Python 3.10+ — only for the Python SDK
- Optional: Docker; Redis (`STORAGE_TYPE=redis`)
- To run the server: a Slack app in Socket Mode (`SLACK_BOT_TOKEN`,
  `SLACK_APP_TOKEN`). Configuration is entirely env-var driven (see `README.md`).

## Build & run

```sh
go build ./...
go run cmd/server/main.go     # gRPC :9090, HTTP/SSE :8080, metrics :9091
docker build -t astro-messaging .
```

## Protobuf codegen

Edit the `.proto` files, then regenerate and commit the output:

```sh
./scripts/generate-proto.sh              # Go stubs -> pkg/gen
bash sdk/python/scripts/gen_proto.sh     # Python stubs (needs grpcio-tools)
```

CI (`gen-python-stubs.yml`) fails the PR if the Python stubs are out of sync, run
the script and commit the result. The TypeScript SDK loads proto at runtime, so
it has no separate generation step.

## Tests & checks

```sh
go build ./...
go vet ./...
go test ./...                                       # CI adds -race on main
golangci-lint run                                   # config: .golangci.yaml
cd sdk/node && bun install && bun test              # TypeScript SDK
cd sdk/python && pip install -e ".[dev]" && pytest  # Python SDK
```

Cross-language serialization check (also run in CI):

```sh
go run tools/test-serialization/main.go serialize
cd sdk/node && bun test
go run tools/test-serialization/main.go deserialize
```

## Commits & pull requests

- **Conventional Commits** with scopes: `feat(slack): …`, `fix(chat): …`,
  `feat(proto): …`, `ci(pypi): …`, etc.
- Branch off `main`; PRs are squash-merged (PR number in the subject).
- Add a changelog at `docs/changelog/YYYY-MM-DD-<slug>.md` with **Summary /
  Design / Migration** sections for non-trivial changes.
- CI (`.github/workflows/ci.yml`) runs on PRs and pushes to `main`.
