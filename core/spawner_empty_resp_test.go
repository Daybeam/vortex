package core

import (
	"strings"
	"testing"
)

// Regression test for eval §10.4: Xunfei MaaS intermittent empty responses.
//
// Bug: The spawner's turn loop immediately failed the step when a provider
// returned HTTP 200 with zero completion tokens (empty content, no tool calls).
// Xunfei MaaS exhibits this ~40% of the time, causing 13/15 turn failures
// in the engine arm. The fix adds retry logic: shouldRetryEmptyResponse
// retries up to maxEmptyRespRetries (2) times before failing, reducing the
// effective failure rate from ~40% to ~6.4% (0.4^3).
//
// Link: eval-workspace/eval_kit/docs/19-engine-issues-formal.md §10.4
// Fix: core/spawner.go shouldRetryEmptyResponse + turnLoop retry in doSpawn.

func TestShouldRetryEmptyResponse_RetriesUntilExhausted(t *testing.T) {
	const max = 2
	retries := 0

	// Attempt 1: should retry
	retry, err := shouldRetryEmptyResponse(&retries, max)
	if !retry || err != nil {
		t.Fatalf("attempt 1: expected retry=true, err=nil; got retry=%v, err=%v", retry, err)
	}
	if retries != 1 {
		t.Fatalf("attempt 1: expected retries counter=1; got %d", retries)
	}

	// Attempt 2: should retry
	retry, err = shouldRetryEmptyResponse(&retries, max)
	if !retry || err != nil {
		t.Fatalf("attempt 2: expected retry=true, err=nil; got retry=%v, err=%v", retry, err)
	}
	if retries != 2 {
		t.Fatalf("attempt 2: expected retries counter=2; got %d", retries)
	}

	// Attempt 3: should fail (exhausted)
	retry, err = shouldRetryEmptyResponse(&retries, max)
	if retry {
		t.Fatalf("attempt 3: expected retry=false; got true")
	}
	if err == nil {
		t.Fatal("attempt 3: expected non-nil error; got nil")
	}
	if retries != 3 {
		t.Fatalf("attempt 3: expected retries counter=3; got %d", retries)
	}
}

func TestShouldRetryEmptyResponse_ErrorFormat(t *testing.T) {
	// The error message must contain the retry count for observability.
	retries := 2 // already at max
	_, err := shouldRetryEmptyResponse(&retries, 2)
	if err == nil {
		t.Fatal("expected error when retries exhausted")
	}
	if !strings.Contains(err.Error(), "empty_response_zero_tokens") {
		t.Errorf("error should contain 'empty_response_zero_tokens'; got: %v", err)
	}
	if !strings.Contains(err.Error(), "exhausted 2 retries") {
		t.Errorf("error should contain 'exhausted 2 retries'; got: %v", err)
	}
}

func TestShouldRetryEmptyResponse_ZeroMaxNeverRetries(t *testing.T) {
	// Edge case: max=0 means no retries — first empty response fails immediately.
	// This documents the pre-fix behavior as a configurable boundary.
	retries := 0
	retry, err := shouldRetryEmptyResponse(&retries, 0)
	if retry {
		t.Fatal("max=0: expected retry=false on first attempt")
	}
	if err == nil {
		t.Fatal("max=0: expected non-nil error on first attempt")
	}
}

func TestShouldRetryEmptyResponse_CounterAdvancesEachCall(t *testing.T) {
	// Verify the counter is mutated through the pointer, not copied.
	const max = 5
	retries := 0
	for i := 1; i <= max; i++ {
		retry, _ := shouldRetryEmptyResponse(&retries, max)
		if !retry {
			t.Fatalf("attempt %d (max=%d): expected retry=true", i, max)
		}
		if retries != i {
			t.Fatalf("after attempt %d: expected counter=%d; got %d", i, i, retries)
		}
	}
	// One more should fail
	retry, err := shouldRetryEmptyResponse(&retries, max)
	if retry || err == nil {
		t.Fatalf("attempt %d: expected retry=false, err!=nil", max+1)
	}
	if retries != max+1 {
		t.Fatalf("after exhaustion: expected counter=%d; got %d", max+1, retries)
	}
}
