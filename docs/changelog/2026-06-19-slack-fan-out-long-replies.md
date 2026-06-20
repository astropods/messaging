# Summary

Long agent replies on Slack were mangled: the adapter converted Markdown to
Slack mrkdwn and packed it into one `chat.postMessage` capped at 50 blocks,
truncating the middle of big answers, and individual blocks were folded behind a
"See more". Tables rendered as monospace text and code blocks lost their
language.

# Design

Replies are now handed to Slack as Markdown via the native **`markdown`** block
(`{"type":"markdown","text":...}`); Slack does the rendering — headings, bold,
links, lists, tables, and syntax-highlighted code — so the adapter no longer
converts Markdown or builds section/table blocks itself.

What remains is sizing and delivery:

- **Chunking.** A `markdown` block is capped at ~12000 chars, and a whole
  message's blocks have their own size limit. `chunkMarkdown` splits the reply
  into ≤10000-char pieces on line boundaries, treating each fenced code block as
  an indivisible atom so a ```` ``` ```` block is never cut in two (which would
  drop its closing fence and re-interpret the rest as Markdown). An atom larger
  than the budget is hard-split as a last resort.
- **Fan-out.** Each chunk is posted as its own `markdown` block in a separate
  message in the same thread, so every message stays under Slack's per-block and
  per-message size limits. The footer + feedback widgets ride on the last
  message. The first message carries the full reply text as its notification
  fallback; continuations use `"(continued)"`.

This removes the bespoke mrkdwn conversion, section/sentence/word chunking, and
native table-builder in favor of Slack's own renderer. Plain (non-Markdown) text
is unaffected — it is valid Markdown and renders as plain paragraphs.

# Migration

None. Behavioral change internal to the Slack adapter; no API or interface
changes.
