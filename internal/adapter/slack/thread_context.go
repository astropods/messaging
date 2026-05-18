package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

const slackDedupTTL = 10 * time.Minute

type slackDedupEntry struct {
	expires time.Time
}

func (a *SlackAdapter) initDedup() {
	if a.recentSlackMsg == nil {
		a.recentSlackMsg = make(map[string]slackDedupEntry)
	}
}

func (a *SlackAdapter) slackMsgDedup(channelID, messageTS string) bool {
	a.initDedup()
	key := channelID + ":" + messageTS
	now := time.Now()
	for k, e := range a.recentSlackMsg {
		if now.After(e.expires) {
			delete(a.recentSlackMsg, k)
		}
	}
	if _, ok := a.recentSlackMsg[key]; ok {
		return true
	}
	a.recentSlackMsg[key] = slackDedupEntry{expires: now.Add(slackDedupTTL)}
	return false
}

func (a *SlackAdapter) isObserverChannel(channelID string) bool {
	return slicesContains(a.config.ObserverChannelIDs, channelID)
}

func slicesContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (a *SlackAdapter) matchesAutoLink(channelID, text string) bool {
	if len(a.config.AutoLinkTextSubstrings) == 0 {
		return false
	}
	if len(a.config.AutoLinkChannelIDs) > 0 && !slicesContains(a.config.AutoLinkChannelIDs, channelID) {
		return false
	}
	for _, sub := range a.config.AutoLinkTextSubstrings {
		if sub != "" && strings.Contains(text, sub) {
			return true
		}
	}
	return false
}

func (a *SlackAdapter) threadMaxMessages() int {
	if a.config.ThreadMaxMessages > 0 {
		return a.config.ThreadMaxMessages
	}
	return 50
}

func (a *SlackAdapter) threadMaxRunes() int {
	if a.config.ThreadMaxRunes > 0 {
		return a.config.ThreadMaxRunes
	}
	return 12000
}

func (a *SlackAdapter) threadTranscript(ctx context.Context, channelID, threadTS string) string {
	if threadTS == "" || a.client == nil {
		return ""
	}
	limit := a.threadMaxMessages()
	msgs, _, _, err := a.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     limit,
	})
	if err != nil {
		slog.Warn("[Slack] threadTranscript: conversations.replies failed", "channel", channelID, "thread", threadTS, "err", err)
		return ""
	}
	var b strings.Builder
	runes := 0
	maxRunes := a.threadMaxRunes()
	for _, m := range msgs {
		if m.BotID != "" && m.User == "" {
			continue
		}
		line := fmt.Sprintf("<@%s> %s", m.User, strings.TrimSpace(m.Text))
		if line == "<@> " {
			line = strings.TrimSpace(m.Text)
		}
		need := utf8.RuneCountInString(line) + 1
		if runes+need > maxRunes {
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

func (a *SlackAdapter) buildSlackInboundContent(ctx context.Context, channelID, messageTS, threadTS, userText string, opts inboundContentOpts) string {
	meta := formatSlackMetaLine(channelID, messageTS, threadTS, a.resolvePermalink(ctx, channelID, messageTS))
	content := userText
	if opts.prependThreadSummary {
		summary := a.threadTranscript(ctx, channelID, threadIDForSummary(threadTS, messageTS))
		content = prependSlackThreadSummary(content, summary)
	}
	content = prependSlackMeta(content, meta)
	if opts.prependObserverMarker {
		content = prependSlackObserverMarker(content, a.config.ObserverPrependMarker)
	}
	if opts.prependAutoLink {
		content = prependSlackAutoLink(content)
	}
	return content
}

type inboundContentOpts struct {
	prependObserverMarker bool
	prependThreadSummary  bool
	prependAutoLink       bool
}

func threadIDForSummary(threadTS, messageTS string) string {
	if threadTS != "" {
		return threadTS
	}
	return messageTS
}

func (a *SlackAdapter) resolvePermalink(ctx context.Context, channelID, messageTS string) string {
	if a.client == nil {
		return resolveSlackPermalink(a.config.TeamURL, channelID, messageTS)
	}
	link, err := a.client.GetPermalinkContext(ctx, &slack.PermalinkParameters{
		Channel: channelID,
		Ts:      messageTS,
	})
	if err == nil && link != "" {
		return link
	}
	return resolveSlackPermalink(a.config.TeamURL, channelID, messageTS)
}

// observerTopLevelDelivery assembles top-level observer-channel bypass content (PR1).
func (a *SlackAdapter) observerTopLevelDelivery(ctx context.Context, channelID, messageTS, userText string) string {
	return a.buildSlackInboundContent(ctx, channelID, messageTS, "", userText, inboundContentOpts{
		prependObserverMarker: true,
		prependThreadSummary:  a.config.ObserverPrependThread,
	})
}
