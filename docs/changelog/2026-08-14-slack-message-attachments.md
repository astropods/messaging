# Summary

An agent could not read a forwarded or shared Slack message. Asked about one, it
answered that the content "wasn't included in your message", and the user had to
copy and paste the text by hand.

Slack keeps the body of a shared message, and of a link unfurl, in
`attachments` rather than in `text` or `blocks`. Every inbound path read text
and Block Kit only, so those messages arrived empty:

- `handleAppMention` and `handleMessage` rendered the event.
- `fetchReactionMessage` rendered the reacted message.
- `threadTranscript` rendered each message in the thread summary.

The thread summary made the loss total rather than partial. It skips any message
whose rendered text is empty, so a forwarded message in a thread produced no
line at all: the agent had no way to know the message existed.

# Design

`renderMessage(text, blocks, attachments)` now wraps `renderBlocks` and appends
the attachment bodies. Mentions, reactions, thread summaries, and chat history
call it, so every path that delivers Slack content to an agent shares one
rendering.

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
`text` rendered through the same block walk (an attachment can carry its own
Block Kit), and falls back to `fallback` when there is no text. Two cases
produce nothing, on purpose: an image-only attachment, which would otherwise add
a label with nothing under it, and a body already contained in the message text,
which is the shape of a link unfurl derived from that text.

## Message events read the raw envelope

`slackevents.MessageEvent` has no attachments field, so DMs, thread replies, and
observed messages cannot take them from the parsed event. `eventAttachments`
reads them from the events envelope that socket mode already delivers alongside
the parsed event, and `handleInnerEvent` passes them to `handleMessage`.

Reading the envelope rather than re-fetching the message keeps the common shape
working at no cost: "look at this" plus a forward carries the comment in `text`
and the substance in `attachments`, and a fetch-only-when-empty rule would miss
it, because the rendering is not empty. It also costs no extra Slack call, and
avoids racing Slack: an unfurl is attached to the stored message a moment after
the event arrives, so a fetch at event time can still come back without it.

# Compatibility

The change is additive for every agent already running against this container.

- With no attachments, `renderMessage` returns exactly what `renderBlocks`
  returned, which a test pins.
- Content only grows, and only for messages that carry attachments. Nothing is
  removed or reordered, and the existing markers (`[slack_meta]`,
  `[reaction …]`, `[slack_thread_summary]`) stay where they were: the attachment
  block is appended to the message body.
- No proto, SDK, config, or scope change. `handleMessage`'s new parameter is
  internal to the adapter package.
- Observer agents that ingest bot posts will start seeing attachment bodies from
  those bots, which were previously invisible. That is more input to the same
  field, not a different shape. Where a bot repeats `text` in `fallback`, the
  duplicate is dropped rather than delivered twice.

# Migration

None.
