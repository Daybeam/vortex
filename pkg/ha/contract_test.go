package ha

import (
	"errors"
	"testing"
	"time"
)

// These tests pin the parts of the reserved contract that leak outside Go:
// role strings appear in /health and probes, and the errors are matched by HTTP
// middleware and store implementations. Changing them silently would break
// clients and the Phase 1-3 implementations, so keep them locked here.

func TestRoleWireValues(t *testing.T) {
	want := map[Role]string{
		RoleUnknown: "unknown",
		RoleSolo:    "solo",
		RoleLeader:  "leader",
		RoleStandby: "standby",
	}
	for role, text := range want {
		if string(role) != text {
			t.Errorf("role %q serializes as %q, want %q", text, string(role), text)
		}
	}
}

func TestRoleIsLeader(t *testing.T) {
	cases := map[Role]bool{
		RoleSolo:    true,
		RoleLeader:  true,
		RoleStandby: false,
		RoleUnknown: false,
	}
	for role, want := range cases {
		if got := role.IsLeader(); got != want {
			t.Errorf("Role(%q).IsLeader() = %v, want %v", role, got, want)
		}
	}
}

func TestLeaseValidRequiresFenceAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	expired := Lease{Fence: 3, ExpiresAt: now.Add(-time.Second)}
	unfenced := Lease{Fence: 0, ExpiresAt: now.Add(time.Minute)}
	live := Lease{Fence: 3, ExpiresAt: now.Add(time.Minute)}
	if expired.Valid(now) {
		t.Error("expired lease reported valid")
	}
	if unfenced.Valid(now) {
		t.Error("zero-fence lease reported valid; fence zero means never elected")
	}
	if !live.Valid(now) {
		t.Error("live lease reported invalid")
	}
}

func TestErrNotLeaderKeepsLeaderHint(t *testing.T) {
	var target *ErrNotLeader
	err := error(&ErrNotLeader{Leader: "http://hub-0:8000"})
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(*ErrNotLeader) failed for %v", err)
	}
	if target.Leader != "http://hub-0:8000" {
		t.Errorf("leader hint = %q", target.Leader)
	}
	if got, want := (&ErrNotLeader{}).Error(), "ha: not the leader"; got != want {
		t.Errorf("empty hint message = %q, want %q", got, want)
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	for _, err := range []error{ErrLeaseLost, ErrNotElectable, ErrStaleFence} {
		if errors.Is(err, ErrLeaseLost) && err != ErrLeaseLost {
			t.Errorf("%v unexpectedly matches ErrLeaseLost", err)
		}
	}
}
