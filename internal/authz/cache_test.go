package authz

import (
	"testing"
	"time"
)

func TestCache_HitWithinTTL(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := newResultCache(60 * time.Second)
	c.now = func() time.Time { return now }
	k := cacheKey{identityType: "user", identityID: "alice", adapter: "web"}

	c.put(k, true)
	if got, ok := c.get(k); !ok || !got {
		t.Errorf("expected cache hit allowed=true; got ok=%v allowed=%v", ok, got)
	}

	now = now.Add(30 * time.Second)
	if got, ok := c.get(k); !ok || !got {
		t.Errorf("expected hit at t+30s; got ok=%v allowed=%v", ok, got)
	}
}

func TestCache_MissAfterTTL(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := newResultCache(60 * time.Second)
	c.now = func() time.Time { return now }
	k := cacheKey{identityType: "user", identityID: "alice", adapter: "web"}

	c.put(k, true)
	now = now.Add(61 * time.Second)
	if _, ok := c.get(k); ok {
		t.Errorf("expected miss after TTL")
	}
}

// Denied results are cached too — we don't want a denied principal to keep
// hammering the server.
func TestCache_CachesDeny(t *testing.T) {
	c := newResultCache(60 * time.Second)
	k := cacheKey{identityType: "user", identityID: "bob", adapter: "web"}
	c.put(k, false)
	got, ok := c.get(k)
	if !ok {
		t.Fatal("expected hit")
	}
	if got {
		t.Error("expected allowed=false to be cached as false, got true")
	}
}

// Different request triples are independent cache entries — a hit on one
// must not leak to another.
func TestCache_KeysAreTupleScoped(t *testing.T) {
	c := newResultCache(60 * time.Second)
	c.put(cacheKey{"user", "alice", "web"}, true)
	if _, ok := c.get(cacheKey{"user", "alice", "slack"}); ok {
		t.Error("different adapter must not hit")
	}
	if _, ok := c.get(cacheKey{"user", "bob", "web"}); ok {
		t.Error("different identity_id must not hit")
	}
	if _, ok := c.get(cacheKey{"slack", "alice", "web"}); ok {
		t.Error("different identity_type must not hit")
	}
}
