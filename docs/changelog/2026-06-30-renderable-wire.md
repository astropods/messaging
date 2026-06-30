# Summary

Adds the wire foundation for interactive rendering and elicitation: a new
`Renderable` primitive an agent emits to ask the user for structured input
mid-conversation (forms, confirmations, tool-call approvals), plus the
`RenderableResponse` the user sends back. This is the transport layer only. No
adapter emits or renders a Renderable yet, so behavior is unchanged.

Spec: see the astro repo, `docs/01-spec/interactive-rendering-elicitation-spec.md`.

# Design

A new `renderable.proto` defines `Renderable` and `RenderableResponse` plus the
`RenderKind` and `RenderableAction` enums. The data schema and proposed values
travel as JSON strings (`data_schema_json`, `value_json`, `content_json`) so the
wire stays out of JSON Schema's business and forward compatible with MCP schema
evolution.

The two messages ride existing oneofs additively, so older peers ignore them:

- `AgentResponse.payload` gains `Renderable renderable = 14` (agent → platform).
- `PlatformFeedback.feedback` gains `RenderableResponse renderable_response = 11`
  (platform → agent, alongside the existing button and text feedback).

`RenderableAction` is a shared top-level enum used by both messages, so its
values are prefixed (`RENDERABLE_ACTION_*`) to avoid package-scope collisions on
generic verbs like `SUBMIT` and `CANCEL`. The other enums in this package are
message-nested and stay unprefixed.

Go, Python, and TypeScript stubs are regenerated. The TS SDK loads proto at
runtime, so the new types need no generated code there. Binary roundtrip tests
cover both new oneof members, including the nested feedback → renderableResponse
path the oneof-flattening guard exists for.

# Migration

None. All proto changes are additive. SDK versions bump (node 0.1.0, python
0.1.0) for the new protocol surface, but no existing message or behavior changes.
