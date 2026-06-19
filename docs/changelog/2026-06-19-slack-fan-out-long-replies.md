# Summary

Long agent replies were silently truncated on Slack. The adapter split a reply
into 3000-char `section` blocks (Slack's per-section text limit) but packed them
all into a single `chat.postMessage`, which Slack caps at 50 blocks. The
overflow handling kept the first 48 blocks plus the two trailing feedback
widgets and dropped everything in between — so the middle of a big answer just
vanished, with no indication to the user.

# Design

The reply now fans out across multiple messages in the same thread instead of
being truncated.

- `feedbackTrailingBlocks` builds the footer plus the two feedback widgets that
  must stay together and close out a reply.
- `batchBlocks` splits the content section blocks into groups of at most 50,
  gluing the trailing widgets onto the final group when they fit, or emitting
  them as their own final message when they don't. The feedback controls always
  land on the last message, so a reply ends with a single set of controls.
- `PostMessageWithFeedback` posts each group as a `chat.postMessage` in the same
  thread. The first message carries the full reply text as its notification
  fallback; continuations use `"(continued)"`. It returns the first message's
  timestamp, as before.

No content is dropped regardless of length, and every message stays within
Slack's 50-block limit.

# Migration

None. Behavioral change internal to the Slack adapter; no API or interface
changes.
