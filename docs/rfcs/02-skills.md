# RFC: Agent-Advertised Skills (Slash Commands)

**Status:** Draft
**Author:** rabbah
**Date:** 2026-05-22
**Branch:** `feat/slash`

## Summary

Let agents register a named, user-invocable "skill" with the messaging server. The playground surfaces registered skills in a `/`-triggered popover; picking one sends a typed `SkillInvocation` to the agent over its existing gRPC stream — distinct from a free-text `Message`. Agents register/deregister dynamically; no static config, no manifest, no restart required.

The messaging layer stores the skill catalog and routes invocations. It does not execute skills, validate arguments, or impose a schema beyond `name`/`description`/`longDescription`.

## Motivation

Current chat surface only carries free-text user messages. Agents that expose well-defined commands (`/help`, `/agent-card`, `/clear`, domain actions) have to:

1. Parse free text on every turn, hoping the user typed the magic prefix consistently.
2. Document commands out-of-band — the playground has no way to discover or display them.

That's brittle and undiscoverable. We want:

- **Discovery**: user types `/` and sees what's available.
- **Typed dispatch**: the agent receives a `SkillInvocation{name, args}` it can match on, not a string it has to parse.
- **Dynamic catalog**: agents add/remove skills at runtime (e.g. after auth, after loading config) without redeploying.

## Design Principles

1. **Agent-owned namespace.** Skills are agent state pushed to the server, not server config. The server is a passthrough catalog.
2. **No server-side execution.** The server forwards `SkillInvocation` to the agent and stops. Agents implement whatever the skill does.
3. **In-memory, per-process.** Skills live for the lifetime of the messaging-server process. Restart = empty catalog; agents re-register on stream reconnect. No persistence, no DB.
4. **Same stream, new oneof.** Reuses the existing bidirectional `ProcessConversation` stream — no new RPCs, no new connections.
5. **Web-only surface for now.** Slack/Discord adapters are unaffected. Only the web adapter exposes `/api/skills` and accepts invocations.

## Protocol

### Proto additions (`proto/astro/messaging/v1/skill.proto` — new file)

Three messages:

- **`Skill`** — `name` (slug, unique key), `description` (short label), `long_description` (optional help text).
- **`AddSkill { Skill skill }`** — agent → server. Re-sending with the same `name` replaces the prior entry.
- **`RemoveSkill { string name }`** — agent → server. Unknown names are a no-op.
- **`SkillInvocation { string skill_name; string args }`** — server → agent. `args` is free-form text after the command (e.g. `/echo hello world` → `args = "hello world"`).

### Existing protos extended

- `ConversationRequest.request` oneof gains `AddSkill add_skill = 7`, `RemoveSkill remove_skill = 8`, and `SkillInvocation skill_invocation = 9` (server→agent forwarding). All additive, no field-number conflicts.
- `AgentResponse.payload` oneof gains `SkillInvocation skill_invocation = 13` — the wrapper the gRPC server uses when forwarding an invocation to the agent on its existing stream.

### Name & description rules

The proto types are intentionally string-shaped, but the server enforces a tight contract before anything reaches the store. Validation lives in `SkillsStore.Add`:

| Field | Rule | Rationale |
|---|---|---|
| `Skill.name` | matches `^[a-z][a-z0-9-]*$` (kebab-case: lowercase letters, digits, hyphens; must start with a letter) | URL-safe, popover-friendly, unambiguous as a slash-command identifier |
| `Skill.name` | length ≤ 64 characters | Long enough for composite names (`summarize-thread-and-post-back`), short enough to render in the popover |
| `Skill.description` | length ≤ 1024 characters | Short label; ~1 KB of UTF-8 covers a tooltip plus a paragraph |
| `Skill.long_description` | length ≤ 1024 characters | Same cap; a chatty agent can't bloat the catalog |

Implicit consequences of the kebab-case regex: no spaces, no capital letters, no XML angle brackets — they can never appear in a stored name.

**Case-insensitive collision and lookup.** Names are unique case-insensitively, but registration *rejects* mixed-case rather than silently normalizing it — agents see immediate feedback at the source (`AddSkill rejected: name must be kebab-case…`). The case-insensitive behavior is for *client lookups*: `Get` and `Remove` lowercase + trim the query, so a playground user typing `/Agent-Card` or `/AGENT-CARD` in the popover resolves to the stored `agent-card`.

**Failure mode.** `Add` returns an error wrapping `store.ErrInvalidSkillName`. The gRPC handler logs at warn level and drops the registration without terminating the stream — one bad `AddSkill` should not kill the conversation. Other request types on the stream continue to flow.

## Architecture

The agent and the web client are independent inflows — they never see each other directly. The messaging server is the hub: it owns the `SkillsStore`, the HTTP surface, and the gRPC stream registry. Arrows show one-way flow.

```
       agent                                   web client
         │                                          │
         │ add_skill / remove_skill        user picks /foo
         │ (ConversationRequest oneof)     (POST /invocations)
         ▼                                          ▼
  ┌──────────────┐                          ┌──────────────┐
  │ SkillsStore  │◄────── List ─────────────│ Web handlers │
  │  (in-mem,    │                          │              │
  │   sync.RWMu) │                          │ /api/skills  │
  └──────────────┘                          └──────────────┘
                                                    │
                                       HandleSkillInvocation
                                       (forward to agent via
                                        gRPC stream registry)
                                                    │
                                                    ▼
                                           AgentResponse{
                                             skill_invocation: {...}
                                           } on the agent's stream
```

`HandleSkillInvocation` does **not** mutate the store — it looks up the skill name to reject unknown invocations, then forwards the invocation to the agent's stream via the gRPC server's stream registry.

### Components

- **`internal/store/skills.go` (`SkillsStore`)** — `map[name]*Skill` behind a `sync.RWMutex`. Methods: `Add`, `Remove`, `Get`, `List` (sorted by name for stable UI ordering). `Add` validates name format/length and description size (see *Name & description rules* below) and returns an error; invalid entries are never stored. `Get` and `Remove` are case-insensitive so playground popover queries with any casing still resolve.
- **`internal/grpc/server.go`** — `ProcessConversation` loop gains two new oneof cases (`AddSkill`, `RemoveSkill`) that mutate the store. A new `HandleSkillInvocation(ctx, conversationID, *SkillInvocation)` method mirrors `HandleIncomingMessage`: finds the agent's stream by conversation ID, wraps the invocation in `AgentResponse{skill_invocation: …}`, and `Send`s it.
- **`internal/adapter/SkillInvoker` (interface)** — `HandleSkillInvocation(ctx, conv, *SkillInvocation) error`. Implemented by the gRPC server, consumed by the web adapter. Keeps the web → gRPC dependency one-way and mockable.
- **`internal/adapter/web/handlers.go`** — two new endpoints (below). Holds a `*SkillsStore` and a `SkillInvoker`, both nil-safe so tests can opt out.

### HTTP surface (web adapter)

| Method | Path | Auth | Behavior |
|--------|------|------|----------|
| `GET`  | `/api/skills` | session | Returns `{ skills: [{ name, description, longDescription? }, …] }`. Empty array if none registered (not 404). |
| `POST` | `/api/conversations/{id}/invocations` | session | Body `{ skill, args? }`. 202 on accepted forward, 404 if `skill` is unknown, 503 if no agent stream is attached. |

`POST /invocations` deliberately validates the skill name against the store before forwarding — a misbehaving client can't make an agent see arbitrary slash commands. Skipped only when the store isn't wired (test injection).

### Server wiring (`cmd/server/main.go`)

One additional store constructed in `main`, threaded through `grpc.NewServer(…, skillsStore)` and `initializeAdapters(…, skillsStore, …)`. The web adapter wires the gRPC server as its `SkillInvoker` after both are constructed (same dependency-injection pattern already used for `AudioForwarder`).

## SDK Ergonomics

Each SDK gets the minimum needed to make the common case ergonomic.

- **`pkg/client` (Go)** — `ConversationStream.AddSkill(*pb.Skill)` and `RemoveSkill(name string)` — wraps the oneof construction.
- **`sdk/node`** — same on `ConversationStream`. Also emits a `skillInvocation` event when the server forwards one, so agents can `stream.on('skillInvocation', …)`.
- **`sdk/python`** — `add_skill(skill)` / `remove_skill(name)` helpers in a new `skills` module; return a `ConversationRequest` ready to send. Mirrors the existing Python style (build-the-message, caller sends).

No SDK has a higher-level "skill registry" abstraction. The agent owns the policy of when to add/remove; the SDK only saves it from remembering the oneof layout.

## Implementation Plan

Implemented as a chain of single-purpose commits on `feat/slash`, each green on its own. Order matters: every step compiles and tests against the prior step's surface, so a reviewer can read them top-down without needing the rest of the stack in their head.

| # | Commit | Layer | What it adds | Gated by |
|---|--------|-------|--------------|----------|
| 1 | `chore(proto): add Skill types and extend conversation/response oneofs` | proto | `proto/astro/messaging/v1/skill.proto` with `Skill` / `AddSkill` / `RemoveSkill` / `SkillInvocation`; new field numbers on `ConversationRequest` (7,8,9) and `AgentResponse` (13). Regenerated `pkg/gen/**`, Node TS types, Python `_pb2` modules. | — |
| 2 | `feat(store): in-memory SkillsStore for agent-advertised slash commands` | store | `internal/store/skills.go` + tests. `Add` / `Remove` / `Get` / `List` (sorted) behind a `sync.RWMutex`. Drops nil / empty-name `Add` to keep the catalog addressable. | (1) for `*pb.Skill` |
| 3 | `feat(grpc): wire SkillsStore + skill-invocation forwarding` | gRPC server | `ProcessConversation` loop routes `AddSkill` / `RemoveSkill` into the store. New `HandleSkillInvocation(ctx, conv, *SkillInvocation)` wraps the invocation in `AgentResponse{skill_invocation}` and sends on the agent's stream. `NewServer` gains a `*SkillsStore` arg; `cmd/server/main.go` constructs and threads it through. | (1), (2) |
| 4 | `feat(web): /api/skills list and /invocations endpoints` | web adapter | `GET /api/skills` returns the catalog; `POST /api/conversations/{id}/invocations` validates the skill against the store and forwards via the new `adapter.SkillInvoker` interface. `WebAdapter` gains `SetSkillsStore` and `SetSkillInvoker`. Wired in `cmd/server/main.go` after the gRPC server is built. | (3) for `SkillInvoker` impl |
| 5 | `feat(sdk/go): AddSkill / RemoveSkill on ConversationStream` | Go SDK | `pkg/client/messaging_client.go` adds two methods that wrap the oneof construction. | (1) |
| 6 | `feat(sdk/node): addSkill / removeSkill + skillInvocation event` | Node SDK | `sdk/node/src/messaging-client.ts` adds `Skill` / `SkillInvocation` interfaces, `addSkill` / `removeSkill` on `ConversationStream`, and a `'skillInvocation'` event emitted when the agent receives one. | (1) |
| 7 | `feat(sdk/python): add_skill / remove_skill helpers` | Python SDK | `sdk/python/src/astropods_messaging/skills.py` with the two helpers; re-exported from `__init__.py` alongside the regenerated `_pb2` types. | (1) |
| 8 | `chore: bump playground git hash` | playground pin | Submodule pointer bump so the consumer pulls in the playground commits that render the `/` popover and POST to `/api/skills` + `/invocations`. | (4) for the HTTP surface |
| 9 | `feat(store): validate skill name + description; case-insensitive lookup` | store + gRPC | Adds the kebab-case regex, 64-char name cap, 1024-char description cap, and the case-insensitive `Get`/`Remove` paths in `internal/store/skills.go`. `SkillsStore.Add` now returns `error`; the gRPC handler logs a warning and drops the registration on rejection rather than killing the stream. New test files cover each rule, the at-the-limit lengths, and the lookup-vs-collision behavior. | (2), (3) |

### Why this ordering

- Proto first (1): every subsequent layer imports a generated type. Splitting proto into its own commit also keeps the noisy regen diff (`pkg/gen/**`, Node TS, Python `_pb2`) out of the feature commits.
- Store before gRPC (2 → 3): the gRPC handler mutates the store, so the store has to exist and be tested in isolation first. Avoids "wait, what does `List` return if `Add` failed?" coming up during gRPC review.
- gRPC before web (3 → 4): the web adapter depends on the `SkillInvoker` interface that the gRPC server implements. Putting them in one commit hid the interface inside the implementation; separating made the contract reviewable on its own.
- Web before SDKs (4 → 5,6,7): SDKs are leaves — nothing in the messaging server depends on them. Landing them last means a broken SDK never blocks a server reviewer.
- SDKs can ship in parallel (5, 6, 7): independent files, no cross-dependency. Sequenced in the history only for tidy review.
- Playground pin last (8): the submodule bump is the only commit that turns the feature on for users. Holding it to the end means CI is green for every prior commit even with the playground at its old hash.

### Test coverage added

- `internal/store/skills_test.go` — `Add` / `Remove` / `Get` / `List` semantics, including nil-safety, sort order, every name-validation rule (length, kebab-case, capitals, underscores, XML brackets, leading digit/hyphen), description size caps, off-by-one at the length limit, and case-insensitive lookup.
- `internal/grpc/server_test.go` — extended for the `AddSkill` / `RemoveSkill` routing and `HandleSkillInvocation` forwarding paths.
- `internal/adapter/web/adapter_test.go` — covers `GET /api/skills` (empty + populated), `POST /invocations` happy path, unknown-skill 404, missing-invoker 503.
- `sdk/python/tests/test_stubs.py` — round-trip parse of the new proto messages so the generated Python stubs stay in sync.

## Compatibility

- **Existing adapters (Slack, Discord)**: untouched. They never see `AddSkill`/`RemoveSkill`/`SkillInvocation` and never get asked for skills.
- **Existing agents**: opt-in. An agent that ignores the new oneof cases continues to work — `Skill*` requests it doesn't send won't arrive, and `SkillInvocation` responses are only forwarded if the user picks one from the playground, which they can't do until the agent first calls `addSkill`.
- **Proto wire compatibility**: purely additive — new field numbers, new messages, no renames or type changes. Old generated code on either side keeps deserializing valid messages (unknown fields are skipped per proto3 semantics).

## Out of Scope (intentional)

- **Argument schemas / autocomplete.** `args` is a single free-form string. Structured arg parsing is the agent's job. We can revisit if a real use case wants per-arg typing.
- **Persistence.** Skills die with the process. If an agent restarts, it re-registers. The playground catalog auto-recovers because the stream re-attaches.
- **Permissions / per-user filtering.** Every authenticated session sees the same list. Per-user gating is the agent's job (refuse the invocation in `SkillInvocation` handling).
- **Slack/Discord parity.** Slash commands on those platforms have their own native semantics (Slack: app commands manifest; Discord: registered application commands). Mapping `Skill` onto those is a separate proposal.
- **Skill discovery across multiple agents on the same conversation.** Current model assumes one agent per conversation stream, which the messaging server already enforces.

## Open Questions

- **Should `SkillInvocation` carry the invoking user's identity?** Today it doesn't — agents have to correlate via `conversationId`/the prior `Message.user`. Adding a `User` field is cheap if needed; deferring until an agent asks.
- **Should we rate-limit invocations?** A user spamming `/foo` could flood the agent stream. Probably wants the same backpressure model as text messages — out-of-scope for this RFC.
- **Should the catalog be queryable by tag/category?** Not yet. `description` is the only label. If catalogs grow past ~20 entries this becomes a UX problem.

## Migration

None for existing deployments. Enabling the feature requires:

1. Regenerate protos (`moon run messaging:proto-gen`).
2. Rebuild server + SDK consumers.
3. Update the agent to call `addSkill` after the stream connects and handle the `skillInvocation` event/oneof.

Playground users see the `/` popover automatically once any skill is registered; absent a registered skill, `/` is a normal character.
