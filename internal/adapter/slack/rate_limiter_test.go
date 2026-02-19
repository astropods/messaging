package slack

import (
	"context"
	"testing"
	"time"
)

func TestRateLimiter_TryAcquire(t *testing.T) {
	// Create a rate limiter with 2 tokens/second, burst of 2
	rl := NewRateLimiter(2.0, 2)

	// Should be able to acquire 2 tokens immediately (burst)
	if !rl.TryAcquire() {
		t.Error("Expected first token acquisition to succeed")
	}

	if !rl.TryAcquire() {
		t.Error("Expected second token acquisition to succeed")
	}

	// Third acquisition should fail (no tokens left)
	if rl.TryAcquire() {
		t.Error("Expected third token acquisition to fail")
	}
}

func TestRateLimiter_Refill(t *testing.T) {
	// Create a rate limiter with 2 tokens/second, burst of 2
	rl := NewRateLimiter(2.0, 2)

	// Consume all tokens
	rl.TryAcquire()
	rl.TryAcquire()

	// Wait for refill (should get ~1 token after 500ms)
	time.Sleep(600 * time.Millisecond)

	// Should be able to acquire one more token
	if !rl.TryAcquire() {
		t.Error("Expected token acquisition after refill to succeed")
	}
}

func TestRateLimiter_Wait(t *testing.T) {
	// Create a rate limiter with 2 tokens/second, burst of 1
	rl := NewRateLimiter(2.0, 1)

	ctx := context.Background()

	// First wait should succeed immediately
	start := time.Now()
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("Expected first wait to succeed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Error("Expected first wait to complete quickly")
	}

	// Second wait should block until tokens refill
	start = time.Now()
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("Expected second wait to succeed: %v", err)
	}
	elapsed = time.Since(start)

	if elapsed < 400*time.Millisecond {
		t.Error("Expected second wait to be delayed by rate limit")
	}
}

func TestRateLimiter_ContextCancellation(t *testing.T) {
	// Create a rate limiter with very slow refill
	rl := NewRateLimiter(0.1, 1)

	// Consume the token
	rl.TryAcquire()

	// Create a context that will be cancelled
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Wait should fail due to context cancellation
	err := rl.Wait(ctx)
	if err == nil {
		t.Error("Expected wait to fail due to context cancellation")
	}
}
