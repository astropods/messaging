# slackreplytest

A standalone CLI for eyeballing the Slack adapter's long-reply behavior in a
live workspace. It calls the real `SlackAIClient.PostMessageWithFeedback`, so it
exercises the exact chunking + fan-out code path the adapter uses in production:

- replies **fan out** across multiple threaded messages once they exceed Slack's
  50-block-per-message cap, and
- each block is small enough that Slack renders it inline instead of collapsing
  it behind a **"See more"** fold.

## Prerequisites

- A Slack **bot token** in `SLACK_BOT_TOKEN`.
- The token must belong to an app configured as a **Slack AI assistant**.
  `PostMessageWithFeedback` always attaches the native feedback widgets, which a
  plain bot rejects with `invalid_blocks`. This is the same requirement the
  adapter has in production.
- The bot must be a member of the target channel.

## Run

All commands below run from this directory (`cmd/slackreplytest`):

```bash
cd cmd/slackreplytest
export SLACK_BOT_TOKEN=xoxb-...

# Post the built-in sample (~40 paragraphs: long paras + table + code + blockquote)
go run . -channel C0123456789

# Force a bigger reply to see it fan out across multiple messages
go run . -channel C0123456789 -sample 60

# Send the saved fixture
go run . -channel C0123456789 -input testdata/reply.md

# Send your own markdown, or pipe via stdin
go run . -channel C0123456789 -input path/to/reply.md
cat testdata/reply.md | go run . -channel C0123456789 -input -

# Reply into an existing thread (e.g. the ts a previous run printed)
go run . -channel C0123456789 -thread 1718000000.000100

# Dev-mode footer / agent id in the footer
go run . -channel C0123456789 -dev -agent-id my-agent

# Compare: post the raw content as one native Slack markdown block (Slack renders it)
go run . -channel C0123456789 -input testdata/reply.md                  # custom pipeline
go run . -channel C0123456789 -input testdata/reply.md -markdown-block   # native markdown block
```

## Comparing the custom pipeline vs Slack's native markdown block

`slack-go` exposes a native `markdown` block (`slack.NewMarkdownBlock`) built for
LLM output: you hand Slack raw Markdown and it does the rendering and any
server-side block splitting. `-markdown-block` posts the content that way (no
feedback widgets — the point is to eyeball content rendering) so you can compare
it against the adapter's hand-rolled conversion. Look at whether the native block:

- renders Markdown **tables** (early versions did not),
- avoids the per-block **"See more"** fold on long content and code, and
- stays within limits.

A markdown block is capped at **12,000 characters** (over it Slack returns
`msg_too_long`), so the tool splits long content into multiple markdown blocks
on line boundaries. The native block therefore doesn't remove chunking — but it
could still replace the mrkdwn conversion, table building, and See-more
splitting with simple ≤12k chunking, *if* the rendering checks above pass.

If it renders cleanly, most of the custom pipeline in `slack_ai_api.go` could be
replaced by a single markdown block plus a thin length guard.

(From the module root, substitute `go run ./cmd/slackreplytest` and prefix the
`-input` paths with `cmd/slackreplytest/`.)

The first message's `ts` is printed to **stdout** — re-run with `-thread <ts>`
to test follow-up replies in the same thread. Each posted part is logged to
**stderr** (`part=1 parts=2 …`) so you can watch the fan-out happen.

## Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `-channel` | — | Channel ID to post to (**required**) |
| `-thread` | "" | `thread_ts` to reply into; omit to start a new thread |
| `-input` | "" | Markdown file to send, or `-` for stdin |
| `-sample` | 0 | Generate N sample paragraphs instead of `-input` |
| `-dev` | false | Enable the dev-mode footer |
| `-agent-id` | "" | Agent ID rendered in the footer |
| `-v` | true | Log each posted message part to stderr |
| `-markdown-block` | false | Post raw content as one native Slack markdown block instead of the custom pipeline |

When neither `-input` nor `-sample` is given, a built-in 40-paragraph sample is
used.

## Fixtures

`testdata/reply.md` is a realistic long agent reply (headings, a table, a code
block, a blockquote run, links, and long paragraphs) for exercising the split
without hand-crafting input each time.
