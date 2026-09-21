package core

import (
	"context"
	"time"
)

// TimeProvider injects the current local time into the context.
type TimeProvider struct{}

func (p *TimeProvider) Name() string {
	return "time"
}

func (p *TimeProvider) FetchContext(ctx context.Context) (map[string]any, error) {
	now := time.Now()
	return map[string]any{
		"now":       now.Format(time.RFC3339),
		"timestamp": now.Unix(),
		"weekday":   now.Weekday().String(),
	}, nil
}
