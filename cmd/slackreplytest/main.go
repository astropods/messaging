// Command slackreplytest posts a message to Slack through the real adapter
// code path (SlackAIClient.PostMessageWithFeedback) so you can eyeball the
// long-reply behavior in a live workspace:
//
//   - replies fan out across multiple threaded messages past 50 blocks, and
//   - no block is large enough to collapse behind a "See more" fold.
//
// Usage:
//
//	export SLACK_BOT_TOKEN=xoxb-...
//	go run ./cmd/slackreplytest -channel C0123456789
//	go run ./cmd/slackreplytest -channel C0123456789 -thread 1718000000.000100
//	go run ./cmd/slackreplytest -channel C0123456789 -sample 60
//	go run ./cmd/slackreplytest -channel C0123456789 -input cmd/slackreplytest/testdata/reply.md
//	cat reply.md | go run ./cmd/slackreplytest -channel C0123456789 -input -
//
// Pass -markdown-block to post the raw content as one native Slack markdown
// block (letting Slack render it) instead of the custom pipeline, to compare
// the two renderings side by side:
//
//	go run ./cmd/slackreplytest -channel C0123456789 -input cmd/slackreplytest/testdata/reply.md
//	go run ./cmd/slackreplytest -channel C0123456789 -input cmd/slackreplytest/testdata/reply.md -markdown-block
//
// The bot token must belong to an app configured as a Slack AI assistant —
// PostMessageWithFeedback attaches the native feedback widgets, which a plain
// bot will reject with invalid_blocks. That is the same requirement the adapter
// has in production.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/astropods/messaging/internal/adapter/slack"
	slackapi "github.com/slack-go/slack"
)

func main() {
	var (
		channel = flag.String("channel", "", "Slack channel ID to post to (required)")
		thread  = flag.String("thread", "", "thread_ts to reply into (optional; omit to start a new thread)")
		input   = flag.String("input", "", "path to a markdown file to send, or - for stdin")
		sample  = flag.Int("sample", 0, "generate N sample paragraphs instead of reading -input (good for testing fan-out)")
		dev     = flag.Bool("dev", false, "enable dev-mode footer")
		agentID = flag.String("agent-id", "", "agent ID rendered in the footer")
		verbose = flag.Bool("v", true, "log each posted message part to stderr")
		mdBlock = flag.Bool("markdown-block", false, "post the raw content as one native Slack markdown block (lets Slack render it) instead of the custom pipeline")
	)
	flag.Parse()

	token := os.Getenv("SLACK_BOT_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: SLACK_BOT_TOKEN is not set")
		os.Exit(2)
	}
	if *channel == "" {
		fmt.Fprintln(os.Stderr, "error: -channel is required")
		flag.Usage()
		os.Exit(2)
	}

	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	content, err := resolveContent(*input, *sample)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "posting %d chars to %s (thread=%q, markdown-block=%v)\n", len(content), *channel, *thread, *mdBlock)

	var ts string
	if *mdBlock {
		ts, err = postMarkdownBlock(token, *channel, *thread, content)
	} else {
		client := slack.NewSlackAIClient(token, *dev, *agentID)
		ts, err = client.PostMessageWithFeedback(context.Background(), *channel, content, *thread)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "post failed: %v\n", err)
		os.Exit(1)
	}

	// If we started a new thread, replies fan out under this ts; print it so you
	// can re-run with -thread <ts> to test follow-up behavior.
	fmt.Println(ts)
}

// postMarkdownBlock posts the raw content as a single native Slack markdown
// block and lets Slack do the rendering (and any server-side block splitting).
// This is the comparison path for evaluating whether the markdown block can
// replace the adapter's custom conversion pipeline. No feedback widgets are
// attached — the point is to eyeball content rendering (tables, code, "See
// more"). Returns the posted message timestamp.
func postMarkdownBlock(token, channel, thread, content string) (string, error) {
	api := slackapi.New(token)

	// A Slack markdown block is capped at 12000 chars (over it → msg_too_long),
	// so split long content into multiple markdown blocks on line boundaries.
	const maxMarkdownBlockChars = 11900
	var blocks []slackapi.Block
	for _, chunk := range chunkOnNewlines(content, maxMarkdownBlockChars) {
		blocks = append(blocks, slackapi.NewMarkdownBlock("", chunk))
	}
	fmt.Fprintf(os.Stderr, "markdown-block: %d block(s)\n", len(blocks))

	opts := []slackapi.MsgOption{
		slackapi.MsgOptionText("markdown-block render test", false), // notification fallback only
		slackapi.MsgOptionBlocks(blocks...),
	}
	if thread != "" {
		opts = append(opts, slackapi.MsgOptionTS(thread))
	}
	_, ts, err := api.PostMessageContext(context.Background(), channel, opts...)
	return ts, err
}

// chunkOnNewlines splits s into pieces of at most max characters, breaking on
// the last newline within the limit so Markdown structures aren't cut mid-line.
func chunkOnNewlines(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut := strings.LastIndex(s[:max], "\n")
		if cut < 1 {
			cut = max
		}
		out = append(out, s[:cut])
		s = strings.TrimPrefix(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// resolveContent returns the message body: a generated sample when -sample > 0,
// the file (or stdin for "-") named by -input, or a built-in default sample.
func resolveContent(input string, sample int) (string, error) {
	if sample > 0 {
		return sampleMarkdown(sample), nil
	}
	switch input {
	case "":
		return sampleMarkdown(40), nil
	case "-":
		b, err := os.ReadFile("/dev/stdin")
		return string(b), err
	default:
		b, err := os.ReadFile(input) //nolint:gosec // path is an explicit CLI arg from a trusted operator
		return string(b), err
	}
}

// sampleMarkdown builds a reply with enough length and variety to exercise both
// behaviors: long paragraphs (which would otherwise fold behind "See more") and
// total volume past the 50-block cap (which forces a fan-out). It also includes
// a heading, bold, a link, a blockquote, a code block, and a table so you can
// confirm those still render across the split.
func sampleMarkdown(paragraphs int) string {
	var b strings.Builder
	b.WriteString("## Long reply test\n\n")
	b.WriteString("This message exercises **fan-out** and the [\"See more\" fix](https://api.slack.com/methods/chat.postMessage).\n\n")
	b.WriteString("> A blockquote that spans\n> several lines should render\n> as one continuous bar.\n\n")
	b.WriteString("```\nfunc main() {\n    fmt.Println(\"this code block must stay intact\")\n}\n```\n\n")
	b.WriteString("| Name | Role | Notes |\n|------|------|-------|\n")
	for i := range 5 {
		fmt.Fprintf(&b, "| item-%d | worker | keeps alignment in a code fence |\n", i)
	}
	b.WriteString("\n")

	sentence := "This is a deliberately long sentence with enough words that the paragraph it belongs to comfortably exceeds the per-block target and would normally be folded behind a \"See more\" link in Slack. "
	for p := range paragraphs {
		fmt.Fprintf(&b, "Paragraph %02d. ", p)
		// ~3 sentences per paragraph → well over the 250-char target.
		for range 3 {
			b.WriteString(sentence)
		}
		b.WriteString("\n\n")
	}
	return b.String()
}
