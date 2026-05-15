package authz

import (
	"sync"
	"time"
)

// cacheKey identifies one cached authorize result. The deployment_id is
// implicit (the cache lives in a single Authorizer bound to one deployment).
// identityScope disambiguates identity_id values that aren't globally
// unique — e.g. a slack user_id is only unique within its team_id, so two
// teams with overlapping user_ids must NOT share a cache slot.
type cacheKey struct {
	identityType  string
	identityID    string
	adapter       string
	identityScope string
}

type cacheEntry struct {
	allowed   bool
	userID    string
	expiresAt time.Time
}

// resultCache stores authorize results keyed by request triple. Both allow and
// deny outcomes are cached so a denied principal doesn't keep hammering the
// server. The resolved WorkOS user_id is cached alongside the bool so callers
// can recover the canonical identity on hits without a round-trip. Eviction is
// lazy (on Get) to avoid a background goroutine; the working set is bounded by
// unique (identity, adapter) pairs per deployment, which is small in practice.
type resultCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[cacheKey]cacheEntry
	now func() time.Time // injected for tests
}

func newResultCache(ttl time.Duration) *resultCache {
	return &resultCache{
		ttl: ttl,
		m:   make(map[cacheKey]cacheEntry),
		now: time.Now,
	}
}

// get returns (allowed, userID, true) on a hit, or (_, _, false) on miss/expiry.
func (c *resultCache) get(k cacheKey) (bool, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return false, "", false
	}
	if c.now().After(e.expiresAt) {
		delete(c.m, k)
		return false, "", false
	}
	return e.allowed, e.userID, true
}

func (c *resultCache) put(k cacheKey, allowed bool, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = cacheEntry{allowed: allowed, userID: userID, expiresAt: c.now().Add(c.ttl)}
}
