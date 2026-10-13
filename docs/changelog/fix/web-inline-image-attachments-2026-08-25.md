# Summary

An image uploaded in the chat composer never reached the model. The web adapter
forwarded every attachment as `Attachment_FILE` with an empty `url`, and
adapter-core only turns an attachment into model-visible content when it is
typed `IMAGE` and carries the bytes in a data URI. The agent therefore received
a path on its files volume and nothing the model could see, so a vision agent
answered as though no image had been sent.

The Slack adapter already inlines images this way. This brings the web path to
the same contract.

# Design

- **Images ride the message twice.** The `FILE` attachment is unchanged, so
  agents that read bytes from the shared volume keep working. Alongside it the
  adapter adds an `IMAGE` attachment whose `url` is a base64 data URI. The two
  resolve through different halves of adapter-core (`resolveAttachments` reads
  FILE, `resolveImages` reads IMAGE), so an agent sees the file as a path and
  the image as visual content in the same turn.
- **The media type is sniffed, not declared.** `Content-Type` arrives from the
  browser, and a data URI whose label disagrees with its bytes is rejected by
  the model. The adapter labels the URI with `http.DetectContentType` and skips
  inlining when the bytes are not an image, which also stops a mislabelled
  upload from reaching the model as a broken image.
- **The inline budget spans the message, not the file.** A message may carry up
  to `maxAttachmentsPerMessage` files. A per-image cap alone would let sixteen
  images overrun the 4 MiB gRPC frame and fail the send outright, so a single
  2 MiB budget is spent across the message. Base64 inflates by about a third,
  which puts a full budget near 2.7 MiB.
- **Every skip degrades to the file.** Oversized, unreadable, and non-image
  attachments still forward as `FILE`. Inlining is additive, so a failure costs
  the model its view of the image and nothing else.

# Migration

None. Agents that already read `StreamOptions.images` (the Slack path) start
receiving web uploads with no change. Agents that only read `attachments` are
unaffected.
