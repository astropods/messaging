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

	c.put(k, true, "alice")
	got, userID, ok := c.get(k)
	if !ok || !got || userID != "alice" {
		t.Errorf("expected cache hit allowed=true user=alice; got ok=%v allowed=%v userID=%q", ok, got, userID)
	}

	now = now.Add(30 * time.Second)
	got, userID, ok = c.get(k)
	if !ok || !got || userID != "alice" {
		t.Errorf("expected hit at t+30s; got ok=%v allowed=%v userID=%q", ok, got, userID)
	}
}

func TestCache_MissAfterTTL(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := newResultCache(60 * time.Second)
	c.now = func() time.Time { return now }
	k := cacheKey{identityType: "user", identityID: "alice", adapter: "web"}

	c.put(k, true, "alice")
	now = now.Add(61 * time.Second)
	if _, _, ok := c.get(k); ok {
		t.Errorf("expected miss after TTL")
	}
}

// Denied results are cached too — we don't want a denied principal to keep
// hammering the server.
func TestCache_CachesDeny(t *testing.T) {
	c := newResultCache(60 * time.Second)
	k := cacheKey{identityType: "user", identityID: "bob", adapter: "web"}
	c.put(k, false, "")
	got, _, ok := c.get(k)
	if !ok {
		t.Fatal("expected hit")
	}
	if got {
		t.Error("expected allowed=false to be cached as false, got true")
	}
}

// The resolved user_id must round-trip through the cache so callers can recover
// the canonical WorkOS user on hits without re-querying the server.
func TestCache_PreservesUserID(t *testing.T) {
	c := newResultCache(60 * time.Second)
	k := cacheKey{identityType: "slack", identityID: "U01", adapter: "slack", identityScope: "T1"}
	c.put(k, true, "user_workos_42")
	allowed, userID, ok := c.get(k)
	if !ok || !allowed || userID != "user_workos_42" {
		t.Errorf("expected hit with userID=user_workos_42; got ok=%v allowed=%v userID=%q", ok, allowed, userID)
	}
}

// Different request tuples are independent cache entries — a hit on one
// must not leak to another. identityScope is part of the key so two slack
// workspaces with overlapping user_ids never collide.
func TestCache_KeysAreTupleScoped(t *testing.T) {
	c := newResultCache(60 * time.Second)
	c.put(cacheKey{"user", "alice", "web", ""}, true, "alice")
	if _, _, ok := c.get(cacheKey{"user", "alice", "slack", ""}); ok {
		t.Error("different adapter must not hit")
	}
	if _, _, ok := c.get(cacheKey{"user", "bob", "web", ""}); ok {
		t.Error("different identity_id must not hit")
	}
	if _, _, ok := c.get(cacheKey{"slack", "alice", "web", ""}); ok {
		t.Error("different identity_type must not hit")
	}
	// Same identity_id in two different slack workspaces is a collision
	// risk — slack user_ids are only unique within one team.
	c.put(cacheKey{"slack", "U01", "slack", "T1"}, true, "")
	if _, _, ok := c.get(cacheKey{"slack", "U01", "slack", "T2"}); ok {
		t.Error("different identity_scope must not hit (cross-workspace leak)")
	}
}
