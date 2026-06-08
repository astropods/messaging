package slack

import (
	"fmt"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/authz"
)

func TestSlackUserProfileCacheGetEmptyMiss(t *testing.T) {
	cache := newSlackUserProfileCache()

	if profile, ok := cache.get("T123", "U123"); ok {
		t.Fatalf("expected cache miss, got hit with profile %+v", profile)
	}
}

func TestSlackUserProfileCacheSetThenGet(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	cache := newSlackUserProfileCache()
	cache.now = func() time.Time { return now }

	want := authz.SlackUserProfile{
		Present:     true,
		DisplayName: "Jesse Morgan",
		Username:    "jesse",
		AvatarURL:   "https://avatars.slack-edge.com/jesse.png",
	}
	cache.set("T123", "U123", want, slackUserProfileCacheTTL)

	got, ok := cache.get("T123", "U123")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != want {
		t.Fatalf("cached profile mismatch: got %+v want %+v", got, want)
	}
}

func TestSlackUserProfileCacheGetAfterTTLExpiryMiss(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	cache := newSlackUserProfileCache()
	cache.now = func() time.Time { return now }

	cache.set("T123", "U123", authz.SlackUserProfile{Present: true}, time.Hour)
	now = now.Add(time.Hour + time.Nanosecond)

	if profile, ok := cache.get("T123", "U123"); ok {
		t.Fatalf("expected expired entry to miss, got hit with profile %+v", profile)
	}
	if got := cacheLen(cache); got != 0 {
		t.Fatalf("expected expired entry to be removed, got %d entries", got)
	}
}

func TestSlackUserProfileCacheFailureExpiresSooner(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	cache := newSlackUserProfileCache()
	cache.now = func() time.Time { return now }

	success := authz.SlackUserProfile{Present: true, DisplayName: "Jesse Morgan"}
	cache.set("T123", "U-success", success, slackUserProfileCacheTTL)
	cache.set("T123", "U-failure", authz.SlackUserProfile{}, slackUserProfileFailureCacheTTL)

	now = now.Add(slackUserProfileFailureCacheTTL - time.Second)
	if _, ok := cache.get("T123", "U-failure"); !ok {
		t.Fatal("expected failure cache entry to hit before failure TTL")
	}

	now = now.Add(2 * time.Second)
	if profile, ok := cache.get("T123", "U-failure"); ok {
		t.Fatalf("expected failure cache entry to expire, got hit with profile %+v", profile)
	}
	if got, ok := cache.get("T123", "U-success"); !ok || got != success {
		t.Fatalf("expected success cache entry to remain valid, got %+v ok=%v", got, ok)
	}
}

func TestSlackUserProfileCacheEmptyKeysAreNoops(t *testing.T) {
	cache := newSlackUserProfileCache()
	profile := authz.SlackUserProfile{Present: true, DisplayName: "Jesse Morgan"}

	cache.set("", "U123", profile, slackUserProfileCacheTTL)
	cache.set("T123", "", profile, slackUserProfileCacheTTL)
	cache.set("T123", "U123", profile, 0)

	if got := cacheLen(cache); got != 0 {
		t.Fatalf("expected empty-key and zero-TTL sets to be no-ops, got %d entries", got)
	}
	if _, ok := cache.get("", "U123"); ok {
		t.Fatal("expected empty teamID get to miss")
	}
	if _, ok := cache.get("T123", ""); ok {
		t.Fatal("expected empty userID get to miss")
	}
}

func TestSlackUserProfileCacheCapsEntries(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	cache := newSlackUserProfileCache()
	cache.now = func() time.Time { return now }
	profile := authz.SlackUserProfile{Present: true, DisplayName: "Jesse Morgan"}

	for i := 0; i < slackUserProfileCacheMaxEntries; i++ {
		cache.set("T123", fmt.Sprintf("U%05d", i), profile, slackUserProfileCacheTTL)
	}
	cache.set("T123", "U-new", profile, slackUserProfileCacheTTL)

	if got := cacheLen(cache); got != slackUserProfileCacheMaxEntries {
		t.Fatalf("expected capped cache length %d, got %d", slackUserProfileCacheMaxEntries, got)
	}
	if _, ok := cache.get("T123", "U-new"); !ok {
		t.Fatal("expected newly inserted entry to be cached after cap eviction")
	}

	now = now.Add(slackUserProfileCacheTTL + time.Nanosecond)
	cache.set("T123", "U-fresh", profile, slackUserProfileCacheTTL)
	if got := cacheLen(cache); got != 1 {
		t.Fatalf("expected expired entries to be swept before insert, got %d entries", got)
	}
}

func cacheLen(cache *slackUserProfileCache) int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.entries)
}
