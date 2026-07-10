# Summary

Adds vendor-neutral trace context to the messaging protocol so agents can attach
the trace for an assistant response and receive that same context back on later
platform feedback. This changelog covers the SDK and proto surface only; adapter
behavior and messaging-container feedback forwarding are separate changes.

# Design

A new `trace.proto` defines `TraceContext` with W3C `traceparent` and optional
`tracestate` values. The message is shared by both response and feedback paths:

- `AgentResponse.trace_context` carries trace context for an assistant response.
- `PlatformFeedback.trace_context` carries the context back when platform
  feedback references that response.
Content chunks remain payload-only; streaming responses attach trace context to
the enclosing `AgentResponse`.

The Go and Python protobuf stubs are regenerated, and the TypeScript SDK types
now expose `TraceContext` on `AgentResponse` and `PlatformFeedback`.

# Migration

None. The proto changes are additive. Older SDKs and adapters continue to
consume feedback as before, but they will not see or emit `trace_context` until
they upgrade to the new messaging SDK.
