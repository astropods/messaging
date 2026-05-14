package slack

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

// slackMsgDedup suppresses duplicate Slack deliveries (retries, rapid duplicates)
// within a short window using an in-memory map bounded by maxKeys.
// Duplicate suppressions are logged at the call site with channel/user/ts context;
// capacity evictions log at Debug from shouldDeliver.
type slackMsgDedup struct {
	mu      sync.Mutex
	m       map[string]time.Time
	maxKeys int
}

func newSlackMsgDedup(maxKeys int) *slackMsgDedup {
	if maxKeys < 8 {
		maxKeys = 8
	}
	return &slackMsgDedup{m: make(map[string]time.Time), maxKeys: maxKeys}
}

// shouldDeliver returns whether this delivery should proceed. If false, sinceFirst
// is how long ago the same dedup key was first accepted (helps tell Slack retries
// from rapid user duplicates). Callers should log structured context on false.
func (d *slackMsgDedup) shouldDeliver(key string) (accept bool, sinceFirst time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, t := range d.m {
		if now.Sub(t) > 2*time.Minute {
			delete(d.m, k)
		}
	}
	if t0, ok := d.m[key]; ok {
		return false, now.Sub(t0)
	}
	d.m[key] = now
	for len(d.m) > d.maxKeys {
		var oldestK string
		var oldestT time.Time
		first := true
		for k, t := range d.m {
			if first || t.Before(oldestT) {
				oldestT = t
				oldestK = k
				first = false
			}
		}
		slog.Debug("[Slack] Dedup: evicted oldest cache entry at capacity",
			"evicted_key", oldestK,
			"max_keys", d.maxKeys,
		)
		delete(d.m, oldestK)
	}
	return true, 0
}

func (a *SlackAdapter) threadTranscript(ctx context.Context, channelID, rootTS string) string {
	maxMsg := a.config.ThreadMaxMessages
	if maxMsg <= 0 {
		maxMsg = 30
	}
	maxRunes := a.config.ThreadMaxRunes
	if maxRunes <= 0 {
		maxRunes = 12000
	}

	msgs, _, _, err := a.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: rootTS,
		Limit:     200,
	})
	if err != nil || len(msgs) == 0 {
		return ""
	}

	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Timestamp < msgs[j].Timestamp })

	var b strings.Builder
	runes := 0
	count := 0
	for _, m := range msgs {
		if m.Type != "message" || m.SubType == "bot_message" {
			continue
		}
		if count >= maxMsg {
			break
		}
		line := fmt.Sprintf("[%s] <%s> %s\n", m.Timestamp, m.User, m.Text)
		n := utf8.RuneCountInString(line)
		if runes+n > maxRunes {
			break
		}
		b.WriteString(line)
		runes += n
		count++
	}
	return strings.TrimSpace(b.String())
}

func (a *SlackAdapter) isObserverChannel(channelID string) bool {
	if channelID == "" || channelID[0] == 'D' {
		return false
	}
	if len(a.observerChannels) == 0 {
		return false
	}
	return a.observerChannels[channelID]
}

func (a *SlackAdapter) autoLinkChannelAllowed(channelID string) bool {
	chans := a.config.AutoLinkChannelIDs
	if len(chans) == 0 {
		chans = a.config.AllowedChannelIDs
	}
	if len(chans) == 0 {
		return true
	}
	return slices.Contains(chans, channelID)
}
