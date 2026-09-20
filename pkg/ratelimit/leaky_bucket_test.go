package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLeakyBucket(t *testing.T) {
	// 600 RPM = 10 requests per second = 100ms per request
	bucket := NewLeakyBucket(600)
	ctx := context.Background()

	start := time.Now()

	// First request should be immediate
	err := bucket.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("first request took too long: %v", elapsed)
	}

	// Second request should take ~100ms
	err = bucket.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("second request was too fast: %v", elapsed)
	}
}

func TestLeakyBucket_Concurrency(t *testing.T) {
	// 600 RPM = 100ms interval
	bucket := NewLeakyBucket(600)
	ctx := context.Background()

	var wg sync.WaitGroup
	numRequests := 5
	results := make([]time.Duration, numRequests)
	start := time.Now()

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = bucket.Wait(ctx)
			results[idx] = time.Since(start)
		}(i)
	}

	wg.Wait()

	// At least one request should have taken ~400ms (5 * 100ms, with first one at 0)
	maxElapsed := time.Duration(0)
	for _, d := range results {
		if d > maxElapsed {
			maxElapsed = d
		}
	}

	expectedMin := 400 * time.Millisecond
	if maxElapsed < expectedMin {
		t.Errorf("expected max elapsed to be at least %v, got %v", expectedMin, maxElapsed)
	}
}

func TestLeakyBucket_CancelledContext(t *testing.T) {
	bucket := NewLeakyBucket(1) // 1 RPM = 60s delay
	ctx, cancel := context.WithCancel(context.Background())

	// Consume the immediate slot
	_ = bucket.Wait(ctx)

	// Next one should wait
	errChan := make(chan error)
	go func() {
		errChan <- bucket.Wait(ctx)
	}()

	// Cancel after a short delay
	time.Sleep(100 * time.Millisecond)
	cancel()

	err := <-errChan
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
