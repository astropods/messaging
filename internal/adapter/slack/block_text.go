package slack

import (
	"strings"

	"github.com/slack-go/slack"
)

// renderBlocks returns the message content the agent should receive,
// merging the plain-text fallback with any Block Kit content.
//
// Slack auto-derives `text` from rich_text for user-typed messages, so
// re-rendering rich_text would duplicate. When `text` is non-empty we
// treat it as authoritative for rich_text and only render non-rich_text
// blocks (section, header, context) on top. When `text` is empty (apps
// can omit it) we render every block we know about.
func renderBlocks(text string, blocks slack.Blocks) string {
	text = strings.TrimSpace(text)
	if len(blocks.BlockSet) == 0 {
		return text
	}

	includeRichText := text == ""
	parts := make([]string, 0, len(blocks.BlockSet))
	for _, b := range blocks.BlockSet {
		if _, isRichText := b.(*slack.RichTextBlock); isRichText && !includeRichText {
			continue
		}
		if t := strings.TrimSpace(renderBlock(b)); t != "" {
			parts = append(parts, t)
		}
	}

	rendered := strings.Join(parts, "\n\n")
	switch {
	case rendered == "":
		return text
	case text == "":
		return rendered
	default:
		return text + "\n\n" + rendered
	}
}

// renderBlock renders a single Block Kit block to plain text. Non-text
// blocks (divider, image, actions, file, input) return "".
func renderBlock(block slack.Block) string {
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
			if t := renderRichTextElement(e); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// renderRichTextElement handles the rich_text container variants:
// section (inline run), list, quote, preformatted. All four wrap
// RichTextSectionElement sequences; only their layout differs.
func renderRichTextElement(elem slack.RichTextElement) string {
	switch e := elem.(type) {
	case *slack.RichTextSection:
		return renderSectionElements(e.Elements)
	case *slack.RichTextQuote:
		return renderSectionElements(e.Elements)
	case *slack.RichTextPreformatted:
		return renderSectionElements(e.Elements)
	case *slack.RichTextList:
		var parts []string
		for _, item := range e.Elements {
			if t := renderRichTextElement(item); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func renderSectionElements(elems []slack.RichTextSectionElement) string {
	var sb strings.Builder
	for _, se := range elems {
		sb.WriteString(renderRichTextSectionElement(se))
	}
	return sb.String()
}

// renderRichTextSectionElement renders inline rich_text children using
// Slack's wire-format mention syntax (<@U…>, <#C…>, <!subteam^S…>) so
// downstream consumers (e.g. stripMentions) parse them the same way they
// parse the regular `text` field.
func renderRichTextSectionElement(se slack.RichTextSectionElement) string {
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
	case *slack.RichTextSectionTeamElement:
		return "<!team^" + e.TeamID + ">"
	case *slack.RichTextSectionEmojiElement:
		return ":" + e.Name + ":"
	case *slack.RichTextSectionBroadcastElement:
		return "<!" + e.Range + ">"
	case *slack.RichTextSectionColorElement:
		return e.Value
	}
	return ""
}
