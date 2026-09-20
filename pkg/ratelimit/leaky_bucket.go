package ratelimit

import (
	"context"
	"sync"
	"time"
)

// RateLimiter defines the interface for proactive rate limiting.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

// LeakyBucket implements a proactive rate limiter.
// It ensures requests are processed at a steady rate (Requests Per Minute).
// For 1 RPM, it ensures at least 60 seconds between requests.
type LeakyBucket struct {
	interval time.Duration
	lastLeak time.Time
	mu       sync.Mutex
}

// NewLeakyBucket creates a new LeakyBucket with the given RPM.
func NewLeakyBucket(rpm int) *LeakyBucket {
	if rpm <= 0 {
		return nil
	}
	return &LeakyBucket{
		interval: time.Minute / time.Duration(rpm),
	}
}

// Wait blocks until a request slot is available or the context is cancelled.
// It uses a virtual "leak" time to queue up concurrent requests.
func (b *LeakyBucket) Wait(ctx context.Context) error {
	b.mu.Lock()
	now := time.Now()

	// Initialize lastLeak if it's the first call
	if b.lastLeak.IsZero() {
		b.lastLeak = now.Add(-b.interval) // Allow first request immediately
	}

	nextLeak := b.lastLeak.Add(b.interval)

	// If now is already past nextLeak, we can go now, but we don't
	// want to allow a burst if the bucket has been idle for a long time.
	// We cap the "idle credit" to 1 interval.
	if now.After(nextLeak.Add(b.interval)) {
		nextLeak = now
	} else if now.After(nextLeak) {
		nextLeak = now
	}

	delay := time.Until(nextLeak)
	b.lastLeak = nextLeak // Reserve the slot
	b.mu.Unlock()

	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
