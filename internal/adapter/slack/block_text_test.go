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
