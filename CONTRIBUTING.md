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

## Contributions of source code

All such contributions should be in the form of pull requests.

By opening a pull request

- You agree that your contributions will be licensed under the [Apache 2.0 License](LICENSE).
- When you open a pull request with your contributions, **you are certifying that you wrote the code** in the corresponding patch pursuant to the [Developer Certificate of Origin](#developer-certificate-of-origin) included below for your reference.

## Developer Certificate of Origin

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
1 Letterman Drive
Suite D4700
San Francisco, CA, 94129

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```
