package authz

import (
	"sync"
	"time"
)

// cacheKey identifies one cached authorize result. The deployment_id is
// implicit (the cache lives in a single Authorizer bound to one deployment),
// so we only key on the request triple.
type cacheKey struct {
	identityType string
	identityID   string
	adapter      string
}

type cacheEntry struct {
	allowed   bool
	expiresAt time.Time
}

// resultCache stores boolean authorize results keyed by request triple. Both
// allow and deny outcomes are cached so a denied principal doesn't keep
// hammering the server. Eviction is lazy (on Get) to avoid a background
// goroutine; the working set is bounded by unique (identity, adapter) pairs
// per deployment, which is small in practice.
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

// get returns (allowed, true) on a hit, or (_, false) on miss/expiry.
func (c *resultCache) get(k cacheKey) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return false, false
	}
	if c.now().After(e.expiresAt) {
		delete(c.m, k)
		return false, false
	}
	return e.allowed, true
}

func (c *resultCache) put(k cacheKey, allowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = cacheEntry{allowed: allowed, expiresAt: c.now().Add(c.ttl)}
}
