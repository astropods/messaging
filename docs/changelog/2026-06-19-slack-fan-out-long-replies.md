# Summary

Long agent replies on Slack had two problems. They were silently truncated —
the adapter split a reply into 3000-char `section` blocks (Slack's per-section
text limit) but packed them into a single `chat.postMessage`, which Slack caps
at 50 blocks; the overflow handling kept the first 48 blocks plus the trailing
feedback widgets and dropped everything in between. And the surviving blocks,
being up to 3000 chars each, were collapsed by Slack behind a "See more" fold.

# Design

Replies now render in full and inline.

- `splitForSectionBlocks` breaks the reply into ~250-char section blocks (on
  line and sentence boundaries) so Slack renders each block without a "See more"
  fold, the approach the Yoda agent uses. A single over-long clause with no
  sentence break (commas, "e.g.", an em-dash) is word-wrapped so it can't remain
  one oversized block. Fenced code blocks and runs of blockquote lines are kept
  intact; `splitIntoChunks` remains a safety net for any chunk still over the
  hard 3000-char section limit.
- The reply fans out across multiple messages in the same thread instead of
  being truncated. `batchBlocks` splits the section blocks into groups of at
  most 50, gluing the footer + feedback widgets (`feedbackTrailingBlocks`) onto
  the final group when they fit, or emitting them as their own final message
  when they don't. The feedback controls always land on the last message, so a
  reply ends with a single set of controls.
- `PostMessageWithFeedback` posts each group as a `chat.postMessage` in the same
  thread. The first message carries the full reply text as its notification
  fallback; continuations use `"(continued)"`. It returns the first message's
  timestamp, as before.

No content is dropped regardless of length, every message stays within Slack's
50-block limit, and no block is large enough to trigger a "See more".

# Migration

None. Behavioral change internal to the Slack adapter; no API or interface
changes.
