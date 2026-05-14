package slack

import (
	"encoding/json"
	"strings"

	"github.com/slack-go/slack"
)

// extractBlockText walks a Slack Block Kit payload and returns its
// human-readable text representation. It handles the block types that
// commonly carry text (section, header, context, rich_text). Non-textual
// blocks (image, divider, actions, file, input) contribute nothing.
//
// Output is one entry per block, joined by single newlines, so an agent
// receives a readable rendering of the message's structured content.
func extractBlockText(blocks slack.Blocks) string {
	if len(blocks.BlockSet) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks.BlockSet))
	for _, b := range blocks.BlockSet {
		if t := blockText(b); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// blockText renders a single Block Kit block to plain text.
func blockText(block slack.Block) string {
	switch b := block.(type) {
	case *slack.SectionBlock:
		var parts []string
		if b.Text != nil && b.Text.Text != "" {
			parts = append(parts, b.Text.Text)
		}
		for _, f := range b.Fields {
			if f != nil && f.Text != "" {
				parts = append(parts, f.Text)
			}
		}
		return strings.Join(parts, "\n")
	case *slack.HeaderBlock:
		if b.Text != nil {
			return b.Text.Text
		}
	case *slack.ContextBlock:
		var parts []string
		for _, e := range b.ContextElements.Elements {
			if t, ok := e.(*slack.TextBlockObject); ok && t.Text != "" {
				parts = append(parts, t.Text)
			}
		}
		return strings.Join(parts, " ")
	case *slack.RichTextBlock:
		var parts []string
		for _, e := range b.Elements {
			if t := richTextElementText(e); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// richTextElementText renders a top-level rich_text child element. slack-go
// v0.12.3 only models rich_text_section explicitly; every other variant
// (rich_text_list, rich_text_quote, rich_text_preformatted, ...) lands in
// RichTextUnknown with the raw JSON preserved, which we walk generically.
func richTextElementText(elem slack.RichTextElement) string {
	switch e := elem.(type) {
	case *slack.RichTextSection:
		var sb strings.Builder
		for _, se := range e.Elements {
			sb.WriteString(richTextSectionElementText(se))
		}
		return sb.String()
	case *slack.RichTextUnknown:
		return genericTextFromRaw(e.Raw)
	}
	return ""
}

// richTextSectionElementText renders a single rich_text_section child.
// Mentions/channels/usergroups are rendered using Slack's wire format
// (<@U…>, <#C…>, <!subteam^S…>) so downstream consumers can parse them
// the same way they parse the Slack `text` field.
func richTextSectionElementText(se slack.RichTextSectionElement) string {
	switch e := se.(type) {
	case *slack.RichTextSectionTextElement:
		return e.Text
	case *slack.RichTextSectionLinkElement:
		if e.Text != "" {
			return e.Text
		}
		return e.URL
	case *slack.RichTextSectionUserElement:
		return "<@" + e.UserID + ">"
	case *slack.RichTextSectionChannelElement:
		return "<#" + e.ChannelID + ">"
	case *slack.RichTextSectionUserGroupElement:
		return "<!subteam^" + e.UsergroupID + ">"
	case *slack.RichTextSectionEmojiElement:
		return ":" + e.Name + ":"
	case *slack.RichTextSectionBroadcastElement:
		return "<!" + e.Range + ">"
	case *slack.RichTextSectionColorElement:
		return e.Value
	case *slack.RichTextSectionUnknownElement:
		return genericTextFromRaw(e.Raw)
	}
	return ""
}

// genericTextFromRaw best-effort walks a raw JSON value and concatenates
// any "text" string fields it finds. Used as a fallback for rich_text
// sub-blocks that slack-go v0.12.3 doesn't model (rich_text_list, _quote,
// _preformatted) so we still surface their content to the agent.
func genericTextFromRaw(raw string) string {
	if raw == "" {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return ""
	}
	var sb strings.Builder
	walkForText(v, &sb)
	return strings.TrimSpace(sb.String())
}

func walkForText(v interface{}, sb *strings.Builder) {
	switch x := v.(type) {
	case map[string]interface{}:
		if t, ok := x["text"].(string); ok && t != "" {
			if sb.Len() > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(t)
		}
		for k, vv := range x {
			if k == "text" {
				continue
			}
			walkForText(vv, sb)
		}
	case []interface{}:
		for _, item := range x {
			walkForText(item, sb)
		}
	}
}

// extractBlockTextFromInnerEvent re-decodes the raw inner event JSON to
// pull out the `blocks` field, which slack-go v0.12.3's typed
// MessageEvent / AppMentionEvent structs do not expose. Returns "" on
// any decode error or when no blocks are present.
func extractBlockTextFromInnerEvent(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var wrapper struct {
		Blocks slack.Blocks `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return ""
	}
	return extractBlockText(wrapper.Blocks)
}

// mergeBlockText combines a message's plain text and its block-extracted
// text without duplicating content. When one fully contains the other we
// keep the superset; otherwise we concatenate with a blank line so the
// agent sees both representations.
func mergeBlockText(text, blockText string) string {
	text = strings.TrimSpace(text)
	blockText = strings.TrimSpace(blockText)
	if blockText == "" {
		return text
	}
	if text == "" {
		return blockText
	}
	if text == blockText {
		return text
	}
	if strings.Contains(text, blockText) {
		return text
	}
	if strings.Contains(blockText, text) {
		return blockText
	}
	return text + "\n\n" + blockText
}

// innerEventJSON pulls the `event` sub-object out of a Socket Mode
// events_api payload. This is the same JSON slack-go decodes into a
// typed *slackevents.MessageEvent / *AppMentionEvent, but kept as raw
// bytes so the adapter can read fields (notably `blocks`) that the
// typed structs in slack-go v0.12.3 don't surface. Returns nil when
// the payload is absent or malformed.
func innerEventJSON(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	var w struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(payload, &w); err != nil {
		return nil
	}
	return w.Event
}
