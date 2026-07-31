package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

const (
	// threadSummaryMaxMessages / threadSummaryMaxRunes bound the transcript we
	// prepend so a long thread can't blow up the prompt.
	threadSummaryMaxMessages = 50
	threadSummaryMaxRunes    = 12000
)

// threadTranscript fetches a Slack thread and renders it as "<@user> text"
// lines, capped by threadSummaryMaxMessages and threadSummaryMaxRunes. Bot
// messages with no user (status posts, the loading indicator) are dropped.
// Returns "" when there is no thread or the fetch fails, so callers can prepend
// unconditionally without guarding.
func (a *SlackAdapter) threadTranscript(ctx context.Context, channelID, threadTS string) string {
	if threadTS == "" || a.client == nil {
		return ""
	}
	msgs, _, _, err := a.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     threadSummaryMaxMessages,
	})
	if err != nil {
		slog.Warn("[Slack] threadTranscript: conversations.replies failed",
			"channel", channelID, "thread", threadTS, "err", err)
		return ""
	}
	var b strings.Builder
	runes := 0
	for _, m := range msgs {
		if m.BotID != "" && m.User == "" {
			continue
		}
		text := strings.TrimSpace(renderBlocks(m.Text, m.Blocks))
		if text == "" {
			continue
		}
		line := text
		if m.User != "" {
			line = fmt.Sprintf("<@%s> %s", m.User, text)
		}
		need := utf8.RuneCountInString(line) + 1
		if runes+need > threadSummaryMaxRunes {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		runes += need
	}
	return strings.TrimSpace(b.String())
}
