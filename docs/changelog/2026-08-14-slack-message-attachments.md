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
call it directly, so those four paths share one rendering. Message events reach
it through the lookup described below.

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

## Message events

`slackevents.MessageEvent` carries no attachments at all, so DMs, thread
replies, and observed messages cannot read them from the event. When such an
event renders to empty content, the adapter now resolves the stored message with
the existing `lookupMessage` helper and renders that copy instead. The extra API
call happens only where the alternative is delivering nothing.

A DM that combines typed text with a forward still loses the forwarded body,
because the rendering is non-empty and no lookup runs. Closing that gap means
one lookup per message event, which is not worth the request volume.

# Migration

None. The paths that read attachments already hold the scopes they need
(`channels:history` / `groups:history` / `im:history` / `mpim:history`).
