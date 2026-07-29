# Agent response trace IDs

## Summary

Slack agent replies now expose the trace ID already carried in response trace context, making a specific turn easy to locate during debugging.

Related issue: [astropods/astro#1541](https://github.com/astropods/astro/issues/1541).

## Design

The sidecar validates the W3C version and trace ID at the response boundary while tolerating extension fields from future traceparent versions. Slack preserves response trace context across streamed chunks, derives the trace ID when the response completes, and renders it above the existing agent ID footer. Content and trace buffers share one synchronized lifecycle, so completion or a terminal agent error evicts both together. Keeping this capability Slack-only is an intentional product decision: the in-app chat experience does not expose trace IDs or add trace-copy controls.

## Migration

No action is required.
