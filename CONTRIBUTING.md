# Contributing to messaging

The Astro messaging bridge: a Go/gRPC service that connects agents to chat
surfaces (Slack, web) plus client SDKs for Go, TypeScript, and Python. The wire
contract is protobuf under `proto/astro/messaging/v1/`.

## Layout

- `cmd/server/` — service entrypoint
- `internal/` — `adapter/` (`slack/`, `web/` with the embedded playground),
  `grpc/`, `store/` (Redis + in-memory), `authz/`, `langfuse/`, `metrics/`
- `pkg/` — `client/` (Go SDK), `gen/…/v1/` (generated protobuf), `types/`
- `proto/astro/messaging/v1/` — `.proto` source (the source of truth)
- `sdk/node/` — TypeScript SDK (`@astropods/messaging`)
- `sdk/python/` — Python SDK (`astropods-messaging`)
- `playground/` — UI, a git submodule embedded into the server binary

You only need this repo to build, test, and contribute. (The `playground` UI is a
separate submodule repo, needed only for the embedded web UI / Docker image, see
below.)

## Prerequisites

- Go 1.25+
- `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH` (proto codegen)
- Bun — only for the TypeScript SDK and the playground build
- Python 3.10+ — only for the Python SDK
- Optional: SSH access to the `playground` submodule (embedded web UI / Docker),
  Docker, Redis (`STORAGE_TYPE=redis`), Verdaccio for local SDK publish
- To run the server: a Slack app in Socket Mode (`SLACK_BOT_TOKEN`,
  `SLACK_APP_TOKEN`). Configuration is entirely env-var driven (see `README.md`).

## Build & run

`go build` and the tests work with just this repo. The playground steps are only
needed for the embedded web UI (and the Docker image does them for you):

```sh
go build -v ./...
go run cmd/server/main.go

# optional: embed the playground UI (needs playground submodule access)
git submodule update --init
cd playground && bun install && bun run build && cd ..
cp -r playground/dist internal/adapter/web/dist

docker build -t astro-messaging .        # multi-stage: builds playground + Go
```

## Protobuf codegen

Edit `.proto` files, then regenerate and commit the output:

```sh
./scripts/generate-proto.sh              # Go stubs -> pkg/gen
bash sdk/python/scripts/gen_proto.sh     # Python stubs (grpcio-tools)
```

CI (`gen-python-stubs.yml`) fails the PR if the Python stubs are out of sync, run
the script and commit. The TS SDK loads proto at runtime, no separate gen step.

## Tests & checks

```sh
go test ./...                                       # Go (CI adds -race on main)
go vet ./...
golangci-lint run                                   # config: .golangci.yaml (v2)
cd sdk/node && bun install && bun test              # TypeScript SDK
cd sdk/python && pip install -e ".[dev]" && pytest  # Python SDK
```

Cross-language serialization check:

```sh
go run tools/test-serialization/main.go serialize
cd sdk/node && bun test
go run tools/test-serialization/main.go deserialize
```

## Commits & pull requests

- **Conventional Commits** with scopes: `feat(slack): …`, `fix(chat): …`,
  `feat(proto): …`, `ci(pypi): …`, etc.
- Branch off `main`; PRs are **squash-merged** (PR number in the subject).
- Add a changelog at `docs/changelog/YYYY-MM-DD-<slug>.md` with **Summary /
  Design / Migration** sections for non-trivial changes.
- CI (`.github/workflows/ci.yml`) runs on PRs and push to `main`.

## Versioning

There is no `VERSION` file (the coupling was removed in #51). The release
workflow stamps the build version from the short commit SHA; SDK versions are set
in `sdk/node/package.json` and `sdk/python/pyproject.toml`. (The `README.md`
still describes an older `VERSION`-file scheme, treat this section as current.)
