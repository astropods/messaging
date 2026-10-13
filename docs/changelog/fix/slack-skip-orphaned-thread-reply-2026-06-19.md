# Summary

When an agent replied to a Slack thread whose parent message had since been
deleted, the reply did not stay in the thread. Slack does not reject a
`chat.postMessage` whose `thread_ts` points to a deleted parent — it silently
promotes the reply to a top-level channel message (returns `ok: true` with a
fresh `ts`). The result was agent replies leaking into the channel, out of
context, after the user removed the message they were answering.

# Design

The Slack adapter sets `thread_ts` in several places across two layers (the
custom AI HTTP client for the main reply and rich cards, and the slack-go
client for edits and error/not-enabled notices). A single guard now gates all
of them.

`canPostToThread(channelID, threadTS)` probes the parent before posting:

- Empty `threadTS` (DMs, observed/top-level posts) is always allowed — there is
  no thread to orphan.
- Otherwise it calls `conversations.replies` (the same read `fetchReactionMessage`
  uses). A live message — thread parent or not — comes back as a one-element
  result and the post proceeds. A deleted parent returns `thread_not_found` /
  `message_not_found`, which is treated as definitive and the post is skipped.
- Other (transient) errors fall through to posting anyway, so a temporary API
  blip never drops a legitimate reply.

The missing-parent policy lives in one place: when the parent is gone the
adapter skips the post and logs a warning rather than letting Slack promote it
to the channel. All five threaded send sites (main reply, edit/replace, error,
not-enabled notice, and — via the main reply guard — rich card attachments)
route through this check.

Trade-off: one extra `conversations.replies` read per threaded reply.

# Migration

None. Behavioral fix internal to the Slack adapter; no API or interface changes.
