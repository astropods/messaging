package slack

import (
	"context"
	"log/slog"
	"sync"

	"github.com/slack-go/slack"
)

// maxDirectoryEntries bounds each cache. A workspace has a finite number of
// users and channels, so this only guards against an unbounded id space (bots,
// deleted accounts, shared channels) in a long-lived pod.
const maxDirectoryEntries = 2048

// slackDirectory resolves user and channel ids to display names, caching both
// hits and misses. Slack ids are opaque, so without it a copied thread renders
// as U0… and C0…. A miss is cached too: the lookups need users:read and
// channels:read, and an app without them must not re-ask on every message.
type slackDirectory struct {
	client *slack.Client

	mu       sync.Mutex
	users    map[string]string
	channels map[string]string
}

func newSlackDirectory(client *slack.Client) *slackDirectory {
	return &slackDirectory{
		client:   client,
		users:    make(map[string]string),
		channels: make(map[string]string),
	}
}

// userName returns a display name for a Slack user id, or "" when it cannot be
// resolved. Callers fall back to the raw id.
func (d *slackDirectory) userName(ctx context.Context, userID string) string {
	if d == nil || d.client == nil || userID == "" {
		return ""
	}
	if name, ok := d.lookup(d.users, userID); ok {
		return name
	}
	name := ""
	user, err := d.client.GetUserInfoContext(ctx, userID)
	if err != nil {
		slog.Debug("[Slack] users.info failed; falling back to the raw id",
			"user_id", userID, "err", err)
	} else if user != nil {
		name = firstNonEmpty(user.Profile.DisplayName, user.RealName, user.Name)
	}
	d.store(d.users, userID, name)
	return name
}

// channelName returns a channel name without the leading '#', or "" when it
// cannot be resolved.
func (d *slackDirectory) channelName(ctx context.Context, channelID string) string {
	if d == nil || d.client == nil || channelID == "" {
		return ""
	}
	if name, ok := d.lookup(d.channels, channelID); ok {
		return name
	}
	name := ""
	ch, err := d.client.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		slog.Debug("[Slack] conversations.info failed; falling back to the raw id",
			"channel_id", channelID, "err", err)
	} else if ch != nil {
		name = ch.Name
	}
	d.store(d.channels, channelID, name)
	return name
}

func (d *slackDirectory) lookup(m map[string]string, key string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name, ok := m[key]
	return name, ok
}

func (d *slackDirectory) store(m map[string]string, key, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(m) >= maxDirectoryEntries {
		clear(m)
	}
	m[key] = name
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
