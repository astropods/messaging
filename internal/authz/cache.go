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
	result    Result
	expiresAt time.Time
}

// resultCache stores authorize Results keyed by request triple. Both
// allow and deny outcomes are cached so a denied principal doesn't keep
// hammering the server. Eviction is lazy (on Get) to avoid a background
// goroutine; the working set is bounded by unique (identity, adapter) pairs
// per deployment, which is small in practice.
//
// The cached value carries the resolved WorkOS user_id so the slack adapter
// can attribute traces without re-calling the server for every message in
// a chatty thread.
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

// get returns the cached Result on a hit, or (_, false) on miss/expiry.
func (c *resultCache) get(k cacheKey) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return Result{}, false
	}
	if c.now().After(e.expiresAt) {
		delete(c.m, k)
		return Result{}, false
	}
	return e.result, true
}

func (c *resultCache) put(k cacheKey, result Result) {
	c.putWithTTL(k, result, c.ttl)
}

// putWithTTL caches with a caller-specified TTL. Used by the degraded-mode
// fallback to cache the synthesized Result for a shorter window than the
// default — long enough to absorb a burst of messages without each one
// paying the full request timeout, short enough that the server's
// recovery is reflected quickly (otherwise linked users would keep
// attributing to their raw slack id instead of their WorkOS id for the
// full default TTL after the outage clears).
func (c *resultCache) putWithTTL(k cacheKey, result Result, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = cacheEntry{result: result, expiresAt: c.now().Add(ttl)}
}
