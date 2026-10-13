# Slack inline images + mention thread summary

## Summary

Two gaps in Slack ingress. (1) An @-mention delivered only the mention line, so
an agent summoned mid-thread ("make a ticket for this thread") never saw the
earlier replies and had to ask the user to restate them. (2) Images a user
attached in Slack never reached the agent, so it could not see screenshots.
Both are now forwarded.

## Design

**Thread transcript on in-thread mentions.** When an app-mention is a reply
inside an existing thread (`ThreadTimeStamp` set), the adapter fetches the
thread with `conversations.replies` and prepends a `[slack_thread_summary]`
block (capped at 50 messages / 12k runes, bot status posts dropped) to the
message content. Top-level mentions are unchanged. Consumers already recognize
the `[slack_thread_summary]` marker.

**Inline images.** Image files on app-mention and reacted-message events are
downloaded with the bot token (`slack-go` `GetFile`) and attached to the
outbound `Message` as `Attachment{IMAGE}` whose `url` is a
`data:<mime>;base64,…` URI, so the bytes ride the wire inline and the agent
needs no Slack credentials of its own. Non-image files, images over 2 MiB (kept
under the gRPC frame after base64 inflation), and download failures are skipped
so one bad file never blocks the message. No proto change: reuses the existing
`Attachment.url` field.

## Migration

None; both behaviors are automatic. Images larger than 2 MiB are skipped (and
logged) rather than inlined. If larger images become common, route them through
the files store (storage_key) instead of inlining.
