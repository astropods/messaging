package store

import (
	"context"
	"sync"
	"time"
)

// FeedbackStore maps a platform message ID (e.g. Slack message timestamp)
// to the observability trace ID the agent emitted for that response. The
// Slack adapter writes the mapping when the agent finalizes a message and
// reads it back when a user clicks a feedback button minutes (or hours)
// later, so a thumbs-up/down can be submitted to the right Langfuse trace.
//
// Implementations must be safe for concurrent use. Entries should expire
// after a reasonable window — feedback received days after the original
// reply has limited value and unbounded growth would leak memory.
type FeedbackStore interface {
	// SetTraceID associates platformMessageID with traceID. Overwrites any
	// existing mapping. Empty traceID is treated as a delete to keep callers
	// simple — agents that don't emit a trace shouldn't poison the store.
	SetTraceID(ctx context.Context, platformMessageID, traceID string) error

	// GetTraceID returns the stored trace ID, or "" if no mapping exists
	// (including the case where the entry has expired). Never returns an
	// error for a missing key — callers should branch on the empty string.
	GetTraceID(ctx context.Context, platformMessageID string) (string, error)
}

// DefaultFeedbackTTL is how long a (message → trace) mapping is retained
// in the in-memory store. Seven days matches the spec and covers the
// realistic window during which a user might come back to thumb a reply.
const DefaultFeedbackTTL = 7 * 24 * time.Hour

// MemoryFeedbackStore is a process-local FeedbackStore with TTL-based
// eviction. Suitable for single-pod messaging deployments; multi-pod
// deployments will want a Redis-backed implementation (planned follow-up).
type MemoryFeedbackStore struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]feedbackEntry
}

type feedbackEntry struct {
	traceID   string
	expiresAt time.Time
}

// NewMemoryFeedbackStore builds a store with the given TTL. Pass 0 to use
// DefaultFeedbackTTL.
func NewMemoryFeedbackStore(ttl time.Duration) *MemoryFeedbackStore {
	if ttl <= 0 {
		ttl = DefaultFeedbackTTL
	}
	return &MemoryFeedbackStore{
		ttl:     ttl,
		entries: make(map[string]feedbackEntry),
	}
}

func (s *MemoryFeedbackStore) SetTraceID(_ context.Context, platformMessageID, traceID string) error {
	if platformMessageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if traceID == "" {
		delete(s.entries, platformMessageID)
		return nil
	}
	s.entries[platformMessageID] = feedbackEntry{
		traceID:   traceID,
		expiresAt: time.Now().Add(s.ttl),
	}
	// Opportunistic cleanup: every write sweeps a handful of entries so
	// the map doesn't grow unbounded under steady load. Full sweeps would
	// stall the lock; sampling a tiny slice keeps writes O(1) amortized.
	s.sampleEvict()
	return nil
}

func (s *MemoryFeedbackStore) GetTraceID(_ context.Context, platformMessageID string) (string, error) {
	s.mu.RLock()
	entry, ok := s.entries[platformMessageID]
	s.mu.RUnlock()
	if !ok {
		return "", nil
	}
	if time.Now().After(entry.expiresAt) {
		// Promote the read into a write to GC the expired entry; missing the
		// lock upgrade race is fine — a second goroutine will hit the same
		// expired branch and idempotently delete.
		s.mu.Lock()
		delete(s.entries, platformMessageID)
		s.mu.Unlock()
		return "", nil
	}
	return entry.traceID, nil
}

// sampleEvict iterates a small slice of the map and removes expired entries.
// Bounded so a write never pays more than O(sampleEvictBudget) cleanup cost.
const sampleEvictBudget = 8

func (s *MemoryFeedbackStore) sampleEvict() {
	now := time.Now()
	checked := 0
	for k, v := range s.entries {
		if checked >= sampleEvictBudget {
			break
		}
		if now.After(v.expiresAt) {
			delete(s.entries, k)
		}
		checked++
	}
}

// Size returns the number of entries currently held. Test-only helper.
func (s *MemoryFeedbackStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
