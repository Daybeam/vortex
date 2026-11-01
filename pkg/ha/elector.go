package ha

import "context"

// Elector implements leader election over a shared lease backend. Planned
// backends: Kubernetes Lease (production) and a file lease for local/compose.
type Elector interface {
	// Campaign blocks until this process acquires the lease or ctx is done.
	// It must not return nil merely because another process holds the lease;
	// it keeps campaigning until it wins.
	Campaign(ctx context.Context) (Lease, error)

	// Renew extends the current lease. ErrLeaseLost means another holder owns
	// it now: the caller must demote within one renew interval.
	Renew(ctx context.Context) (Lease, error)

	// Resign releases leadership and bumps the fence so any in-flight write
	// from this process becomes stale. Used for graceful handover.
	Resign(ctx context.Context) error

	// Observe returns the currently recorded lease without modifying it, so a
	// standby can learn the leader endpoint for client hints.
	Observe(ctx context.Context) (Lease, error)

	// Role reports the local view of participation. It is advisory for
	// routing; writes are still validated by Fence.
	Role() Role

	// Lost is closed when leadership is lost. It is the cancellation signal
	// for leader-only work: graph executors, cron, tickers, chat turns.
	Lost() <-chan struct{}
}
