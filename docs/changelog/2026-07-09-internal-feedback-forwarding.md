# Summary

Adds internal feedback forwarding for Slack thumbs up/down reactions and free-form comments
when the feedback can be correlated to an assistant response trace.

# Design

Slack stores trace context in message metadata on the feedback-bearing response
message. Feedback button clicks and comment submissions read that metadata back,
attach it to `PlatformFeedback`, and continue through two paths:

- the existing agent callback path
- a new `internal/internalfeedback` path that forwards trace-correlated feedback to
  Astro Server

The internal feedback client is vendor-neutral from the messaging image's
perspective: it translates platform feedback into `thumbs_up`, `thumbs_down`, or
`comment` and skips events without trace context.

# Migration

None. Existing feedback callbacks continue to work. Internal feedback forwarding only occurs
when an adapter emits trace context and the Slack message carries it in metadata.
