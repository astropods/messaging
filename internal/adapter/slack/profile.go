package slack

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/authz"
)

const (
	slackUserProfileCacheTTL        = 24 * time.Hour
	slackUserProfileFailureCacheTTL = 5 * time.Minute
	slackUserProfileCacheMaxEntries = 10000
	slackUserProfileLookupTimeout   = 2 * time.Second
)

type slackUserProfileCache struct {
	mu      sync.Mutex
	entries map[string]slackUserProfileCacheEntry
	now     func() time.Time
}

type slackUserProfileCacheEntry struct {
	profile   authz.SlackUserProfile
	expiresAt time.Time
}

func newSlackUserProfileCache() *slackUserProfileCache {
	return &slackUserProfileCache{
		entries: make(map[string]slackUserProfileCacheEntry),
		now:     time.Now,
	}
}

func (c *slackUserProfileCache) get(teamID, userID string) (authz.SlackUserProfile, bool) {
	if c == nil || teamID == "" || userID == "" {
		return authz.SlackUserProfile{}, false
	}
	key := teamID + ":" + userID
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return authz.SlackUserProfile{}, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return authz.SlackUserProfile{}, false
	}
	return entry.profile, true
}

func (c *slackUserProfileCache) set(teamID, userID string, profile authz.SlackUserProfile, ttl time.Duration) {
	if c == nil || teamID == "" || userID == "" || ttl <= 0 {
		return
	}
	key := teamID + ":" + userID
	c.mu.Lock()
	now := c.now()
	if _, ok := c.entries[key]; !ok && len(c.entries) >= slackUserProfileCacheMaxEntries {
		c.sweepExpiredLocked(now)
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= slackUserProfileCacheMaxEntries {
		for evictKey := range c.entries {
			delete(c.entries, evictKey)
			break
		}
	}
	c.entries[key] = slackUserProfileCacheEntry{
		profile:   profile,
		expiresAt: now.Add(ttl),
	}
	c.mu.Unlock()
}

func (c *slackUserProfileCache) sweepExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

func (a *SlackAdapter) lookupSlackUserProfile(ctx context.Context, teamID, userID string) authz.SlackUserProfile {
	if teamID == "" || userID == "" || a.client == nil {
		return authz.SlackUserProfile{}
	}
	if a.profileCache == nil {
		a.profileCache = newSlackUserProfileCache()
	}
	if profile, ok := a.profileCache.get(teamID, userID); ok {
		return profile
	}

	lookupCtx, cancel := context.WithTimeout(ctx, slackUserProfileLookupTimeout)
	defer cancel()

	user, err := a.client.GetUserInfoContext(lookupCtx, userID)
	if err != nil {
		a.profileCache.set(teamID, userID, authz.SlackUserProfile{}, slackUserProfileFailureCacheTTL)
		slog.Debug("[Slack] user profile lookup skipped",
			"user_id", userID,
			"err", err,
		)
		return authz.SlackUserProfile{}
	}
	if user == nil {
		a.profileCache.set(teamID, userID, authz.SlackUserProfile{}, slackUserProfileFailureCacheTTL)
		return authz.SlackUserProfile{}
	}

	profile := authz.SlackUserProfile{
		Present:     true,
		DisplayName: firstNonEmpty(user.Profile.DisplayName, user.Profile.RealName, user.RealName),
		Username:    firstNonEmpty(user.Name, user.Profile.DisplayName, user.Profile.RealName),
		AvatarURL:   firstNonEmpty(user.Profile.Image72, user.Profile.Image48, user.Profile.Image192, user.Profile.Image512),
		IsBot:       user.IsBot,
		Deleted:     user.Deleted,
	}
	a.profileCache.set(teamID, userID, profile, slackUserProfileCacheTTL)
	return profile
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
