package slack

import (
	"encoding/json"
	"testing"

	"github.com/slack-go/slack"
)

func TestStripMentions(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"<@U123456> Hello!", "Hello!"},                          // Trims leading/trailing spaces
		{"Hey <@U123456> how are you?", "Hey  how are you?"},     // Doesn't trim internal spaces
		{"No mentions here", "No mentions here"},
		{"<@U123> <@U456> Multiple mentions", "Multiple mentions"}, // Trims leading/trailing spaces
	}

	for _, test := range tests {
		result := stripMentions(test.input)
		if result != test.expected {
			t.Errorf("For input '%s', expected '%s', got '%s'", test.input, test.expected, result)
		}
	}
}

func TestFormatMessageID(t *testing.T) {
	channelID := "C123456"
	timestamp := "1234567890.123456"

	messageID := FormatMessageID(channelID, timestamp)
	expected := "C123456:1234567890.123456"

	if messageID != expected {
		t.Errorf("Expected message ID '%s', got '%s'", expected, messageID)
	}
}

func TestParseMessageID(t *testing.T) {
	messageID := "C123456:1234567890.123456"

	channelID, timestamp := ParseMessageID(messageID)

	if channelID != "C123456" {
		t.Errorf("Expected channel ID 'C123456', got '%s'", channelID)
	}

	if timestamp != "1234567890.123456" {
		t.Errorf("Expected timestamp '1234567890.123456', got '%s'", timestamp)
	}
}

func TestParseMessageID_Invalid(t *testing.T) {
	messageID := "invalid-format"

	channelID, timestamp := ParseMessageID(messageID)

	// New behavior: returns empty strings for invalid format
	if channelID != "" {
		t.Errorf("Expected empty channel ID, got '%s'", channelID)
	}

	if timestamp != "" {
		t.Errorf("Expected empty timestamp, got '%s'", timestamp)
	}
}

// unmarshalBlocks is a tiny helper that round-trips a JSON block-kit
// payload through slack.Blocks so tests can construct realistic block
// fixtures the same way the adapter receives them off the wire.
func unmarshalBlocks(t *testing.T, raw string) slack.Blocks {
	t.Helper()
	var b slack.Blocks
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("failed to unmarshal blocks fixture: %v", err)
	}
	return b
}

func TestExtractBlockText_Empty(t *testing.T) {
	if got := extractBlockText(slack.Blocks{}); got != "" {
		t.Errorf("expected empty string for no blocks, got %q", got)
	}
}

func TestExtractBlockText_Section(t *testing.T) {
	blocks := unmarshalBlocks(t, `[
		{"type":"section","text":{"type":"mrkdwn","text":"Hello *world*"}}
	]`)
	if got := extractBlockText(blocks); got != "Hello *world*" {
		t.Errorf("section text: got %q, want %q", got, "Hello *world*")
	}
}

func TestExtractBlockText_SectionWithFields(t *testing.T) {
	blocks := unmarshalBlocks(t, `[
		{"type":"section",
		 "text":{"type":"mrkdwn","text":"summary"},
		 "fields":[
			{"type":"mrkdwn","text":"*Name:* Jane"},
			{"type":"mrkdwn","text":"*Age:* 42"}
		 ]}
	]`)
	want := "summary\n*Name:* Jane\n*Age:* 42"
	if got := extractBlockText(blocks); got != want {
		t.Errorf("section+fields: got %q, want %q", got, want)
	}
}

func TestExtractBlockText_Header(t *testing.T) {
	blocks := unmarshalBlocks(t, `[
		{"type":"header","text":{"type":"plain_text","text":"Big Title"}}
	]`)
	if got := extractBlockText(blocks); got != "Big Title" {
		t.Errorf("header: got %q, want %q", got, "Big Title")
	}
}

func TestExtractBlockText_Context(t *testing.T) {
	blocks := unmarshalBlocks(t, `[
		{"type":"context","elements":[
			{"type":"mrkdwn","text":"by Alice"},
			{"type":"plain_text","text":"2 hours ago"}
		]}
	]`)
	if got := extractBlockText(blocks); got != "by Alice 2 hours ago" {
		t.Errorf("context: got %q, want %q", got, "by Alice 2 hours ago")
	}
}

func TestExtractBlockText_RichText(t *testing.T) {
	blocks := unmarshalBlocks(t, `[
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
	if got := extractBlockText(blocks); got != want {
		t.Errorf("rich_text: got %q, want %q", got, want)
	}
}

func TestExtractBlockText_RichTextUnknownFallback(t *testing.T) {
	// rich_text_list isn't modeled in slack-go v0.12.3 so it lands in
	// RichTextUnknown; the generic walker should still pull text out.
	blocks := unmarshalBlocks(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_list","style":"bullet","elements":[
				{"type":"rich_text_section","elements":[{"type":"text","text":"first"}]},
				{"type":"rich_text_section","elements":[{"type":"text","text":"second"}]}
			]}
		]}
	]`)
	got := extractBlockText(blocks)
	if got == "" {
		t.Errorf("rich_text_list fallback: expected non-empty extraction, got empty")
	}
	for _, want := range []string{"first", "second"} {
		if !contains(got, want) {
			t.Errorf("rich_text_list fallback: extracted %q missing substring %q", got, want)
		}
	}
}

func TestExtractBlockText_MixedBlocks(t *testing.T) {
	blocks := unmarshalBlocks(t, `[
		{"type":"header","text":{"type":"plain_text","text":"Status"}},
		{"type":"divider"},
		{"type":"section","text":{"type":"mrkdwn","text":"All systems nominal."}},
		{"type":"image","image_url":"https://x/y.png","alt_text":"chart"}
	]`)
	want := "Status\nAll systems nominal."
	if got := extractBlockText(blocks); got != want {
		t.Errorf("mixed blocks: got %q, want %q", got, want)
	}
}

func TestExtractBlockTextFromInnerEvent(t *testing.T) {
	raw := []byte(`{
		"type":"message",
		"text":"fallback",
		"blocks":[
			{"type":"section","text":{"type":"mrkdwn","text":"from blocks"}}
		]
	}`)
	if got := extractBlockTextFromInnerEvent(raw); got != "from blocks" {
		t.Errorf("inner-event extract: got %q, want %q", got, "from blocks")
	}
}

func TestExtractBlockTextFromInnerEvent_NoBlocks(t *testing.T) {
	if got := extractBlockTextFromInnerEvent(nil); got != "" {
		t.Errorf("nil payload: got %q, want empty", got)
	}
	if got := extractBlockTextFromInnerEvent([]byte(`{"text":"only"}`)); got != "" {
		t.Errorf("payload without blocks: got %q, want empty", got)
	}
	if got := extractBlockTextFromInnerEvent([]byte(`not json`)); got != "" {
		t.Errorf("invalid json: got %q, want empty", got)
	}
}

func TestInnerEventJSON(t *testing.T) {
	payload := []byte(`{"team_id":"T1","event":{"type":"message","text":"hi"},"type":"event_callback"}`)
	inner := innerEventJSON(payload)
	if inner == nil {
		t.Fatal("expected non-nil inner event")
	}
	var got struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(inner, &got); err != nil {
		t.Fatalf("inner event not valid JSON: %v", err)
	}
	if got.Type != "message" || got.Text != "hi" {
		t.Errorf("inner event fields: got %+v", got)
	}
}

func TestInnerEventJSON_Empty(t *testing.T) {
	if got := innerEventJSON(nil); got != nil {
		t.Errorf("nil payload: got %q, want nil", got)
	}
	if got := innerEventJSON([]byte(`not json`)); got != nil {
		t.Errorf("invalid json: got %q, want nil", got)
	}
}

func TestMergeBlockText(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		blocks string
		want   string
	}{
		{"both empty", "", "", ""},
		{"only text", "hello", "", "hello"},
		{"only blocks", "", "hello", "hello"},
		{"identical", "hello", "hello", "hello"},
		{"text contains blocks", "hello world", "hello", "hello world"},
		{"blocks contain text", "hello", "hello world", "hello world"},
		{"distinct", "fallback", "rich content", "fallback\n\nrich content"},
		{"trims whitespace", "  hello  ", "  hello  ", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeBlockText(tt.text, tt.blocks); got != tt.want {
				t.Errorf("merge(%q, %q): got %q, want %q", tt.text, tt.blocks, got, tt.want)
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
