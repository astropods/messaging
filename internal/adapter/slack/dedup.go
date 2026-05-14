package slack

import (
	"log/slog"
	"sync"
	"time"
)

// slackMsgDedup suppresses duplicate Slack deliveries (retries, rapid duplicates)
// within a 2-minute window using an in-memory map bounded by maxKeys.
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
// is how long ago the same dedup key was first accepted (helps distinguish Slack
// retries from rapid user duplicates).
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
			"evicted_key", oldestK, "max_keys", d.maxKeys)
		delete(d.m, oldestK)
	}
	return true, 0
}
