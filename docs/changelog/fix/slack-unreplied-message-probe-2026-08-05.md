# Summary

Slack agents went silent on two of their three entry points: an @-mention on a
top-level channel message, and an actionable reaction (`:ticket:`, `:bug:`,
`:mag:`) on a message with no thread. @-mentions inside an existing thread kept
working. Nothing was logged above Debug, so the failure looked like the agent
ignoring the request.

Both broken paths share one property: the message they act on has no replies
yet. The reply target for a top-level mention is the mentioned message itself,
and the reacted message is likewise standalone. Both were resolved with
`conversations.replies`, and the adapter treated an ok-but-empty answer from
that endpoint as proof the message was gone:

- `canPostToThread` skipped the reply to avoid orphaning it (added to keep
  replies out of the channel when a thread parent is deleted).
- `fetchReactionMessage` returned not-ok, and the reaction was never forwarded
  to the agent at all.

An in-thread mention targets a real thread root, which `conversations.replies`
does return, which is why that path was unaffected.

# Design

Message existence is now resolved in one place, `lookupMessage`, which both
call sites share:

1. `conversations.replies` first. It is the only call that reaches a message
   posted inside a thread, which is the common reply target.
2. If that comes back ok-but-empty (or definitively `thread_not_found` /
   `message_not_found`), confirm with `conversations.history` scoped to the
   exact ts (`latest=oldest=ts`, `inclusive=true`). A live standalone message is
   returned there; a deleted one is not.

The result distinguishes "Slack says it is gone" from "we could not tell". Only
the former makes `canPostToThread` skip a post; an API blip, rate limit, or
missing scope still fails open and posts, as before. The deleted-parent guard is
unchanged in behavior — it now needs agreement from both endpoints before
dropping a reply.

Silent drops are also now counted, not just logged: `messages_dropped` gains the
reasons `thread_parent_gone` and `reaction_message_unavailable`, so a recurrence
is visible without reading Debug logs.

## Reactions on an image with no caption

A second silent drop on the same path: `handleReactionAdded` skipped any reacted
message whose rendered text was empty. Slack leaves `text` empty on an
uncaptioned upload and `renderBlocks` deliberately drops image blocks, so a
`:ticket:` on a screenshot resolved fine and was then thrown away — the one shape
where the whole request lives in the attachment. The @-mention path never had
this guard, which is why the same screenshot worked when the bot was @-mentioned.

The guard now runs against resolved attachments as well as text, and the images
are resolved once and reused for the outbound message. Resolved attachments
rather than raw files is deliberate: a non-image upload or a failed download
leaves nothing to act on, and the agent would receive a bare reaction preamble.
Genuinely contentless reactions are still dropped, now counted as
`reaction_message_empty`.

# Migration

None. The adapter already needs the history scope it uses here
(`channels:history` / `groups:history` / `im:history` / `mpim:history`), which
is the same scope `conversations.replies` requires.
