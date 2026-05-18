package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

type slackMetaPayload struct {
	ChannelID string `json:"channel_id"`
	MessageTS string `json:"message_ts"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Permalink string `json:"permalink,omitempty"`
}

func formatSlackMetaLine(channelID, messageTS, threadRootTS, permalink string) string {
	p := slackMetaPayload{
		ChannelID: channelID,
		MessageTS: messageTS,
	}
	if threadRootTS != "" && threadRootTS != messageTS {
		p.ThreadTS = threadRootTS
	}
	if permalink != "" {
		p.Permalink = permalink
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return "[slack_meta] " + string(b)
}

func resolveSlackPermalink(teamURL, channelID, messageTS string) string {
	if channelID == "" || messageTS == "" {
		return ""
	}
	base := strings.TrimSuffix(teamURL, "/")
	if base == "" {
		return ""
	}
	ts := strings.ReplaceAll(messageTS, ".", "")
	return fmt.Sprintf("%s/archives/%s/p%s", base, channelID, ts)
}

func prependSlackMeta(content, metaLine string) string {
	if metaLine == "" {
		return content
	}
	if content == "" {
		return metaLine
	}
	return metaLine + "\n" + content
}

func (a *SlackAdapter) resolvePermalink(ctx context.Context, channelID, messageTS string) string {
	if a.client != nil {
		link, err := a.client.GetPermalinkContext(ctx, &slack.PermalinkParameters{
			Channel: channelID,
			Ts:      messageTS,
		})
		if err == nil && link != "" {
			return link
		}
	}
	return resolveSlackPermalink(a.teamURL, channelID, messageTS)
}

func (a *SlackAdapter) prependInboundMeta(ctx context.Context, channelID, messageTS, threadRootTS, body string) string {
	meta := formatSlackMetaLine(channelID, messageTS, threadRootTS, a.resolvePermalink(ctx, channelID, messageTS))
	return prependSlackMeta(body, meta)
}
