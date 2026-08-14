# Summary

Slack keeps the body of a shared or forwarded message, and of a link unfurl, in
`attachments` rather than in `text` or `blocks`. The adapter renders text and
Block Kit, so a message whose content lives in attachments reaches an agent
empty. In a thread summary it disappears completely, because the summary skips
messages whose rendered text is empty.

Attachment bodies now render on every path that delivers Slack content to an
agent.

# Design

`renderMessage(text, blocks, attachments)` wraps `renderBlocks` and appends the
attachment bodies. Mentions, reactions, thread summaries, and chat history all
call it, so one rendering serves every path.

Each attachment contributes a block labelled `[slack_attachment]`, with the
author name when Slack supplies one:

```
what do these close?

[slack_attachment] from Rodric Rabbah
astro-spec#5, astro-cli#11, agents#56
```

The label matters because the quoted body is not what the user typed. An agent
that treats the two as one string attributes the quoted words to the requester.

Per attachment the renderer takes pretext, title, body, and fields. The body is
`text` rendered through the same block walk, because an attachment can carry its
own Block Kit, and falls back to `fallback` when there is no text. Two cases
render to nothing, on purpose: an image-only attachment, which would otherwise
add a label with nothing under it, and a body already contained in the message
text, which is the shape of a link unfurl derived from that text.

## Message events read the raw envelope

`slackevents.MessageEvent` has no attachments field, so DMs, thread replies, and
observed messages cannot take them from the parsed event. `eventAttachments`
reads them from the events envelope that socket mode delivers alongside the
parsed event, and `handleInnerEvent` passes them to `handleMessage`.

The envelope is the right source rather than a re-fetch of the stored message,
for two reasons. A comment paired with a forward ("look at this" plus the shared
message) keeps the comment in `text` and the substance in `attachments`, so a
rule that fetches only when the rendering is empty never covers it. And Slack
attaches an unfurl to the stored message a moment after dispatching the event, so
a fetch at event time can return a message without it.

# Compatibility

The change is additive for agents already running against this container.

- A message with no attachments renders exactly as `renderBlocks` renders it,
  which a test pins.
- Content grows only for messages that carry attachments. Ordering is unchanged,
  and the existing markers (`[slack_meta]`, `[reaction …]`,
  `[slack_thread_summary]`) keep their position ahead of the body.
- No proto, SDK, config, or scope change. The `attachments` parameter on
  `handleMessage` is internal to the adapter package.
- Observer agents that ingest bot posts receive the attachment bodies of those
  posts, which is more input to the same field. Where a bot repeats `text` in
  `fallback`, the duplicate renders to nothing.

# Migration

None.
