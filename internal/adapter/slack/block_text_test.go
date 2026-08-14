package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// blocksFromJSON round-trips a Block Kit JSON fixture through slack.Blocks
// so tests construct realistic inputs the same way the adapter receives
// them off the wire.
func blocksFromJSON(t *testing.T, raw string) slack.Blocks {
	t.Helper()
	var b slack.Blocks
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("failed to unmarshal blocks fixture: %v", err)
	}
	return b
}

func TestRenderBlocks_NoBlocks(t *testing.T) {
	if got := renderBlocks("hello", slack.Blocks{}); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
	if got := renderBlocks("  hello  ", slack.Blocks{}); got != "hello" {
		t.Errorf("expected trim, got %q", got)
	}
	if got := renderBlocks("", slack.Blocks{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// User-typed rich_text: text is already Slack's rendering of the
// rich_text block. Re-rendering would duplicate, so when text is
// non-empty we drop the rich_text portion entirely.
func TestRenderBlocks_UserRichTextSkippedWhenTextPresent(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_section","elements":[
				{"type":"text","text":"hello world"}
			]}
		]}
	]`)
	if got := renderBlocks("hello world", blocks); got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

// App-posted section: text is a short fallback, section carries the
// real content — both should reach the agent.
func TestRenderBlocks_SectionAppendedToText(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"section","text":{"type":"mrkdwn","text":"All green."}}
	]`)
	want := "Build status\n\nAll green."
	if got := renderBlocks("Build status", blocks); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderBlocks_SectionWithFields(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"section",
		 "text":{"type":"mrkdwn","text":"summary"},
		 "fields":[
			{"type":"mrkdwn","text":"*Service:* api"},
			{"type":"mrkdwn","text":"*Version:* v1.2.3"}
		 ]}
	]`)
	got := renderBlocks("", blocks)
	for _, want := range []string{"summary", "*Service:* api", "*Version:* v1.2.3"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered %q missing %q", got, want)
		}
	}
}

func TestRenderBlocks_HeaderPlusSectionDropsImageDivider(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"header","text":{"type":"plain_text","text":"Deploy Status"}},
		{"type":"divider"},
		{"type":"section","text":{"type":"mrkdwn","text":"All systems nominal."}},
		{"type":"image","image_url":"https://x/y.png","alt_text":"chart"}
	]`)
	got := renderBlocks("", blocks)
	want := "Deploy Status\n\nAll systems nominal."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderBlocks_Context(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"context","elements":[
			{"type":"mrkdwn","text":"by *alice*"},
			{"type":"image","image_url":"https://x/y.png","alt_text":"x"},
			{"type":"plain_text","text":"at 12:00"}
		]}
	]`)
	got := renderBlocks("", blocks)
	want := "by *alice* at 12:00"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// When text is empty, rich_text is included and rendered with wire-format
// mentions so downstream stripMentions sees them.
func TestRenderBlocks_RichTextWhenTextEmpty(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_section","elements":[
				{"type":"text","text":"Hi "},
				{"type":"user","user_id":"U123"},
				{"type":"text","text":" check out "},
				{"type":"link","url":"https://example.com","text":"this"},
				{"type":"text","text":" "},
				{"type":"emoji","name":"tada"}
			]}
		]}
	]`)
	want := "Hi <@U123> check out this :tada:"
	if got := renderBlocks("", blocks); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderBlocks_RichTextList(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_list","style":"bullet","elements":[
				{"type":"rich_text_section","elements":[{"type":"text","text":"first"}]},
				{"type":"rich_text_section","elements":[{"type":"text","text":"second"}]}
			]}
		]}
	]`)
	want := "first\nsecond"
	if got := renderBlocks("", blocks); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderBlocks_RichTextQuoteAndPreformatted(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_quote","elements":[{"type":"text","text":"quoted line"}]},
			{"type":"rich_text_preformatted","elements":[{"type":"text","text":"code()"}]}
		]}
	]`)
	got := renderBlocks("", blocks)
	for _, want := range []string{"quoted line", "code()"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered %q missing %q", got, want)
		}
	}
}

// Mixed: rich_text + section. text is non-empty so rich_text gets
// dropped, but section is still appended.
func TestRenderBlocks_MixedRichTextAndSection(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_section","elements":[
				{"type":"user","user_id":"UBOT"},
				{"type":"text","text":" please summarize"}
			]}
		]},
		{"type":"section","text":{"type":"mrkdwn","text":"Q3 revenue: $5M"}}
	]`)
	got := renderBlocks("<@UBOT> please summarize", blocks)
	want := "<@UBOT> please summarize\n\nQ3 revenue: $5M"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// attachmentsFromJSON round-trips an attachments fixture the same way the
// adapter receives it off the wire.
func attachmentsFromJSON(t *testing.T, raw string) []slack.Attachment {
	t.Helper()
	var atts []slack.Attachment
	if err := json.Unmarshal([]byte(raw), &atts); err != nil {
		t.Fatalf("failed to unmarshal attachments fixture: %v", err)
	}
	return atts
}

func TestRenderMessage_NoAttachmentsMatchesRenderBlocks(t *testing.T) {
	blocks := blocksFromJSON(t, `[
		{"type":"section","text":{"type":"mrkdwn","text":"All green."}}
	]`)
	want := renderBlocks("Build status", blocks)
	if got := renderMessage("Build status", blocks, nil); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A forwarded message: Slack leaves `text` empty and puts the shared body in
// `attachments`, so reading text and blocks alone delivers nothing.
func TestRenderMessage_ForwardedBodyIncluded(t *testing.T) {
	atts := attachmentsFromJSON(t, `[
		{"author_name":"Rodric Rabbah","text":"here are the 3 PRs: astro-spec#5, astro-cli#11, agents#56","fallback":"[Aug 7] Rodric: here are the 3 PRs"}
	]`)
	got := renderMessage("", slack.Blocks{}, atts)
	want := "[slack_attachment] from Rodric Rabbah\nhere are the 3 PRs: astro-spec#5, astro-cli#11, agents#56"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderMessage_ForwardedBodyAppendedToText(t *testing.T) {
	atts := attachmentsFromJSON(t, `[{"text":"the shared body"}]`)
	got := renderMessage("look at this", slack.Blocks{}, atts)
	want := "look at this\n\n[slack_attachment]\nthe shared body"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A link unfurl repeats the text it was derived from; delivering both would
// double the content the agent reads.
func TestRenderMessage_AttachmentDuplicateOfTextSkipped(t *testing.T) {
	atts := attachmentsFromJSON(t, `[{"text":"deploy finished","fallback":"deploy finished"}]`)
	if got := renderMessage("deploy finished", slack.Blocks{}, atts); got != "deploy finished" {
		t.Errorf("got %q, want %q", got, "deploy finished")
	}
}

func TestRenderMessage_AttachmentFallbackUsedWhenTextEmpty(t *testing.T) {
	atts := attachmentsFromJSON(t, `[{"fallback":"only the fallback survived"}]`)
	got := renderMessage("", slack.Blocks{}, atts)
	want := "[slack_attachment]\nonly the fallback survived"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Rich text inside an attachment reaches the agent through the same block
// walk the top-level message uses.
func TestRenderMessage_AttachmentBlocksRendered(t *testing.T) {
	atts := attachmentsFromJSON(t, `[
		{"blocks":[
			{"type":"rich_text","elements":[
				{"type":"rich_text_section","elements":[
					{"type":"text","text":"nested rich text body"}
				]}
			]}
		]}
	]`)
	got := renderMessage("", slack.Blocks{}, atts)
	want := "[slack_attachment]\nnested rich text body"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderMessage_TitleAndFieldsIncluded(t *testing.T) {
	atts := attachmentsFromJSON(t, `[
		{"pretext":"heads up","title":"Build 42 failed","text":"3 tests failed",
		 "fields":[{"title":"Service","value":"api"}]}
	]`)
	got := renderMessage("", slack.Blocks{}, atts)
	want := "[slack_attachment]\nheads up\nBuild 42 failed\n3 tests failed\nService api"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An image-only attachment carries no text, so it must not add a marker with
// nothing under it.
func TestRenderMessage_ImageOnlyAttachmentSkipped(t *testing.T) {
	atts := attachmentsFromJSON(t, `[{"image_url":"https://example.com/a.png","image_width":100}]`)
	if got := renderMessage("look", slack.Blocks{}, atts); got != "look" {
		t.Errorf("got %q, want %q", got, "look")
	}
}
