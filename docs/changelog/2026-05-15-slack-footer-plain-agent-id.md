# Summary

The Slack message footer rendered the Agent ID inside a backtick code span (`` `agent-xyz` ``), which drew the eye and competed with the message body. The footer is meant to be a quiet provenance hint, not a callout.

# Design

`buildFooterText` now emits the Agent ID as plain mrkdwn text rather than wrapping it in backticks. The Slack context block the footer is attached to already styles its contents as small gray text, so plain text reads as intentionally subtle without any further formatting.

- Dev mode: `:test_tube: Sent from dev environment — Agent ID: agent-xyz-123`
- Non-dev: `Agent ID: agent-xyz-123`
- When `agentID` is empty, behavior is unchanged (no footer in non-dev; bare dev banner in dev mode).

# Migration

None. Rendering-only change in the Slack adapter; no API or interface changes.
