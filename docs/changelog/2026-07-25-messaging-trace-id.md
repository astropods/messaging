# Summary

Ticket: [astropods/astro#1182](https://github.com/astropods/astro/issues/1182)

Messaging logs now include the trace ID supplied by an agent response or platform feedback event, allowing operators to correlate sidecar activity with the originating agent trace.

# Design

Request-scoped logging extracts the trace ID from a valid W3C `traceparent` value and adds it as the structured `trace_id` attribute. The enriched context flows through gRPC response routing into the Slack and Web adapters, so their logs retain the same correlation value without exposing the complete propagation header.

Invalid, missing, or all-zero trace identifiers are ignored and preserve the existing logger behavior.

# Migration

No migration is required. Agents that already attach `TraceContext` to responses receive correlated messaging logs automatically.
