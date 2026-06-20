# Summary

Long agent replies on Slack had two problems. They were silently truncated —
the adapter split a reply into 3000-char `section` blocks (Slack's per-section
text limit) but packed them into a single `chat.postMessage`, which Slack caps
at 50 blocks; the overflow handling kept the first 48 blocks plus the trailing
feedback widgets and dropped everything in between. And the surviving blocks,
being up to 3000 chars each, were collapsed by Slack behind a "See more" fold.

# Design

Replies now render in full and inline.

- `buildContentBlocks` lifts Markdown tables out of the reply and renders them as
  native Slack `table` blocks (rich_text cells preserve inline bold and links,
  split at the 100-row limit with the header repeated), the approach the Yoda
  agent uses. Tables no longer render as monospace code fences.
- `splitForSectionBlocks` breaks the prose into ~250-char section blocks (on
  line and sentence boundaries) so Slack renders each block without a "See more"
  fold. A single over-long clause with no sentence break (commas, "e.g.", an
  em-dash) is word-wrapped so it can't remain one oversized block. Fenced code
  blocks and runs of blockquote lines are kept intact; `splitIntoChunks` remains
  a safety net for any chunk still over the hard 3000-char section limit.
- Code-fence language hints (```python) are stripped, since Slack mrkdwn has no
  syntax highlighting and would otherwise render the hint as a literal first
  line. A long code block stays one block (and may carry a "See more"), since
  splitting it would break the fence — that is an intentional trade-off.
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
