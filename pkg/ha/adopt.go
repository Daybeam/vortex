package ha

import "context"

// Adopter is implemented by components that must rehydrate before the new
// leader starts accepting writes. The hub calls every registered Adopter after
// Campaign succeeds and before flipping readiness to leader: rebuild graphs
// from GraphStore, reset StepRunning to pending, rebuild cancel/done channels,
// subscribe to EventBus, then start executors, cron and tickers.
type Adopter interface {
	Adopt(ctx context.Context, lease Lease) error
}
