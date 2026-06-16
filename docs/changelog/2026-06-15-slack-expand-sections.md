## Summary

Long Slack assistant replies now render as multiple threaded replies instead of one dense message with repeated "Show more" affordances.

## Design

Astro still buffers streamed assistant output until the response completes, but the Slack formatter now slices long content at Slack's section text limit and sends each slice as its own reply in the same thread.

Each reply contains one content section. That keeps Slack's native expansion behavior scoped to the reply itself instead of stacking several collapsed sections inside a single message. The final reply carries the agent footer and feedback controls so users can still rate or comment on the complete answer without seeing duplicate controls on every chunk.

## Migration

No agent changes are required. Deployed Slack agents pick up the behavior when the messaging service is updated.
