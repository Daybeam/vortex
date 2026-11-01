package ha

import "time"

// Role is the hub's participation mode. The string values are part of the
// /health payload and the readiness contract; renaming them is a breaking
// change for probes and clients.
type Role string

const (
	RoleUnknown Role = "unknown"
	// RoleSolo is the HA-disabled mode: one process owns everything, exactly as
	// the hub behaves today.
	RoleSolo    Role = "solo"
	RoleLeader  Role = "leader"
	RoleStandby Role = "standby"
)

// IsLeader reports whether mutations are allowed under this role.
func (r Role) IsLeader() bool {
	return r == RoleLeader || r == RoleSolo
}

// Fence is a monotonically increasing token minted by the lease backend on
// every leadership acquisition. It is the only thing preventing a paused old
// leader from writing after a failover; expiry alone cannot.
type Fence uint64

// Lease is one leadership grant. ExpiresAt is compared against the lease
// backend's clock so clock skew cannot extend leadership.
type Lease struct {
	Name      string
	Holder    string
	Fence     Fence
	ExpiresAt time.Time
}

// Valid reports whether the grant is still inside its window at now. A lease
// without a fence is never valid: fence zero means "never elected".
func (l Lease) Valid(now time.Time) bool {
	return l.Fence > 0 && now.Before(l.ExpiresAt)
}
