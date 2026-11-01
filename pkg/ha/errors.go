package ha

import (
	"errors"
	"fmt"
)

var (
	// ErrLeaseLost is returned by Elector.Renew when another holder already owns
	// the lease. The hub must stop leader-only work immediately instead of
	// waiting for the lease to expire.
	ErrLeaseLost = errors.New("ha: lease lost")

	// ErrNotElectable is returned by Elector.Campaign when HA is disabled
	// (RoleSolo); callers should treat the local process as the leader.
	ErrNotElectable = errors.New("ha: election disabled")

	// ErrStaleFence is returned by stores when a write carries a fence older
	// than the highest fence they have accepted for the row.
	ErrStaleFence = errors.New("ha: stale fence")
)

// ErrNotLeader is returned by write endpoints while the hub is a standby.
// Leader optionally carries the base URL clients should retry against; it is
// empty when the current leader is not known yet.
type ErrNotLeader struct {
	Leader string
}

func (e *ErrNotLeader) Error() string {
	if e.Leader == "" {
		return "ha: not the leader"
	}
	return fmt.Sprintf("ha: not the leader (leader=%s)", e.Leader)
}
