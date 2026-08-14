package slack

import (
	"strings"

	"github.com/slack-go/slack"
)

// attachmentMarker labels quoted content so an agent can tell it apart from
// what the user typed.
const attachmentMarker = "[slack_attachment]"

// renderMessage returns the message content the agent should receive: the
// text and Block Kit rendering, plus the body of any message attachments.
//
// Slack keeps the body of a shared or forwarded message, and of a link
// unfurl, in `attachments` rather than in `text` or `blocks`. Reading text
// and blocks alone delivers those messages as empty, so the quoted content
// the user is asking about never reaches the agent.
func renderMessage(text string, blocks slack.Blocks, attachments []slack.Attachment) string {
	rendered := renderBlocks(text, blocks)
	parts := make([]string, 0, len(attachments)+1)
	if rendered != "" {
		parts = append(parts, rendered)
	}
	for _, att := range attachments {
		if block := renderAttachment(att, rendered); block != "" {
			parts = append(parts, block)
		}
	}
	return strings.Join(parts, "\n\n")
}

// renderAttachment renders one attachment, and returns "" when it carries no
// text of its own. Content already present in rendered is dropped: an unfurl
// commonly repeats the text it was derived from, and bot posts repeat `text`
// in `fallback`.
func renderAttachment(att slack.Attachment, rendered string) string {
	body := renderBlocks(att.Text, att.Blocks)
	if body == "" {
		body = strings.TrimSpace(att.Fallback)
	}
	if body != "" && rendered != "" && strings.Contains(rendered, body) {
		body = ""
	}

	lines := make([]string, 0, 3+len(att.Fields))
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			lines = append(lines, s)
		}
	}
	add(att.Pretext)
	add(att.Title)
	add(body)
	for _, f := range att.Fields {
		add(strings.TrimSpace(f.Title + " " + f.Value))
	}
	if len(lines) == 0 {
		return ""
	}

	header := attachmentMarker
	if author := strings.TrimSpace(att.AuthorName); author != "" {
		header += " from " + author
	}
	return header + "\n" + strings.Join(lines, "\n")
}

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
