# Summary

An agent that acts on a reaction is acting on what somebody said. The adapter
forwards the reacted message's text and the reactor, but not who wrote it, so
the words arrive unattributed.

Reactions now carry the author.

# Design

`User` carries the reactor, as on every other event kind, so the author travels
in `PlatformContext.PlatformData`:

| Key | Value | Needs a scope? |
| --- | --- | --- |
| `author_id` | Whoever wrote the reacted message. | no |
| `author_name` | That person's display name. | `users:read` |

`fetchReactionMessage` already loads the message to render its text, so the
author id comes back on a call the adapter was making anyway. The name goes
through the existing `slackDirectory`, which caches hits and misses, so an app
without `users:read` costs one lookup per id rather than one per reaction.

Both keys are absent when Slack supplies no author, which is the shape of a
webhook post. An unknown author stays unknown rather than falling back to the
reactor: defaulting there would attribute someone else's words to whoever
happened to notice them.

# Migration

None. The keys are additive.
