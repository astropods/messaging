package slack

import (
	"context"
	"sync"
	"time"
)

// RateLimiter implements a token bucket rate limiter
type RateLimiter struct {
	tokens       float64
	maxTokens    float64
	refillRate   float64 // tokens per second
	lastRefill   time.Time
	mu           sync.Mutex
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(requestsPerSecond float64, burstSize int) *RateLimiter {
	return &RateLimiter{
		tokens:     float64(burstSize),
		maxTokens:  float64(burstSize),
		refillRate: requestsPerSecond,
		lastRefill: time.Now(),
	}
}

// Wait blocks until a token is available, respecting the context
func (rl *RateLimiter) Wait(ctx context.Context) error {
	for {
		if rl.TryAcquire() {
			return nil
		}

		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			// Continue waiting
		}
	}
}

// TryAcquire attempts to acquire a token without blocking
func (rl *RateLimiter) TryAcquire() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.refill()

	if rl.tokens >= 1 {
		rl.tokens -= 1
		return true
	}

	return false
}

// refill adds tokens based on elapsed time
func (rl *RateLimiter) refill() {
	now := time.Now()
	elapsed := now.Sub(rl.lastRefill).Seconds()
	newTokens := elapsed * rl.refillRate

	rl.tokens = min(rl.maxTokens, rl.tokens+newTokens)
	rl.lastRefill = now
}

// min returns the minimum of two float64 values
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
