package providers

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"
)

// RetryingProvider wraps another provider and implements exponential backoff.
type RetryingProvider struct {
	Base       Provider
	MaxRetries int
	SlotID     string // ADDED (2026-08-16): Reference to GlobalRouter slot
}

func (rp *RetryingProvider) Name() string { return rp.Base.Name() }

func (rp *RetryingProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	var lastErr error
	startTime := time.Now()
	for attempt := 0; attempt <= rp.MaxRetries; attempt++ {
		if err := rp.wait(ctx, attempt, lastErr); err != nil {
			return nil, err
		}

		resp, err := rp.Base.StreamComplete(ctx, req, onChunk)
		if err == nil {
			rp.reportSuccess(startTime)
			return resp, nil
		}
		if !isRetryable(err) {
			rp.reportError(err)
			return nil, err
		}
		lastErr = err
	}
	rp.reportError(lastErr)
	return nil, lastErr
}

func (rp *RetryingProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	var lastErr error
	startTime := time.Now()
	for attempt := 0; attempt <= rp.MaxRetries; attempt++ {
		if err := rp.wait(ctx, attempt, lastErr); err != nil {
			return nil, err
		}

		resp, err := rp.Base.Complete(ctx, req)
		if err == nil {
			rp.reportSuccess(startTime)
			return resp, nil
		}
		if !isRetryable(err) {
			rp.reportError(err)
			return nil, err
		}
		lastErr = err
	}
	rp.reportError(lastErr)
	return nil, lastErr
}

func (rp *RetryingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	var lastErr error
	startTime := time.Now()
	for attempt := 0; attempt <= rp.MaxRetries; attempt++ {
		if err := rp.wait(ctx, attempt, lastErr); err != nil {
			return nil, err
		}

		res, err := rp.Base.Embed(ctx, text)
		if err == nil {
			rp.reportSuccess(startTime)
			return res, nil
		}
		if !isRetryable(err) {
			rp.reportError(err)
			return nil, err
		}
		lastErr = err
	}
	rp.reportError(lastErr)
	return nil, lastErr
}

func (rp *RetryingProvider) reportSuccess(startTime time.Time) {
	if rp.SlotID != "" {
		GlobalRouter.ReportSuccess(rp.SlotID, float64(time.Since(startTime).Milliseconds()))
	}
}

func (rp *RetryingProvider) reportError(err error) {
	if rp.SlotID == "" {
		return
	}
	var rlErr *RateLimitError
	if errors.As(err, &rlErr) {
		GlobalRouter.ReportRateLimit(rp.SlotID, rlErr.RetryAfter)
	} else {
		GlobalRouter.ReportError(rp.SlotID)
	}
}

func (rp *RetryingProvider) wait(ctx context.Context, attempt int, err error) error {
	if attempt == 0 {
		return nil
	}

	var wait time.Duration
	var rlErr *RateLimitError
	if errors.As(err, &rlErr) && rlErr.RetryAfter > 0 {
		wait = time.Duration(rlErr.RetryAfter) * time.Second
	} else {
		// Exponential backoff: 1s, 2s, 4s...
		wait = time.Duration(1<<uint(attempt-1)) * time.Second
		// Add ±10% jitter
		jitter := time.Duration(rand.Intn(int(wait)/5+1)) - (wait / 10)
		wait += jitter
	}

	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rp *RetryingProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return rp.Base.CountTokens(ctx, text)
}

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Check for rate limit errors
	var rlErr *RateLimitError
	if errors.As(err, &rlErr) {
		return true
	}
	// Check for provider errors (5xx are retryable)
	var pErr *ProviderError
	if errors.As(err, &pErr) {
		if strings.Contains(pErr.Msg, "HTTP 5") || strings.Contains(pErr.Msg, "HTTP 429") {
			return true
		}
	}
	// Timeouts are retryable
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return true
	}
	return false
}
