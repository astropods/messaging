package slack

import (
	"encoding/json"
	"fmt"
	"strings"
)

type slackMetaPayload struct {
	ChannelID string `json:"channel_id"`
	MessageTS string `json:"message_ts"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Permalink string `json:"permalink,omitempty"`
}

func formatSlackMetaLine(channelID, messageTS, threadTS, permalink string) string {
	p := slackMetaPayload{
		ChannelID: channelID,
		MessageTS: messageTS,
	}
	if threadTS != "" && threadTS != messageTS {
		p.ThreadTS = threadTS
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

func prependSlackThreadURL(content, url string) string {
	if url == "" {
		return content
	}
	line := "[slack_thread_url] " + url
	if content == "" {
		return line
	}
	return line + "\n" + content
}

func prependSlackThreadSummary(content, summary string) string {
	if summary == "" {
		return content
	}
	block := "[slack_thread_summary]\n" + summary
	if content == "" {
		return block
	}
	return block + "\n\n" + content
}

func prependSlackObserverMarker(content string, enabled bool) string {
	if !enabled {
		return content
	}
	if content == "" {
		return "[slack_observer]"
	}
	return "[slack_observer]\n" + content
}

func prependSlackAutoLink(content string) string {
	if content == "" {
		return "[slack_auto_link]"
	}
	return "[slack_auto_link]\n" + content
}
