package tools

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// TestDelegationEnvelopeWaitFromConfig verifies §4.2:
// the sync envelope wait window reads from config with correct defaults.
func TestDelegationEnvelopeWaitFromConfig(t *testing.T) {
	t.Run("nil registry defaults to 30s", func(t *testing.T) {
		got := delegationEnvelopeWait(nil)
		if got != 30*time.Second {
			t.Errorf("nil registry: want 30s, got %v", got)
		}
	})

	t.Run("unset defaults to 30s", func(t *testing.T) {
		reg := &config.Registry{}
		got := delegationEnvelopeWait(reg)
		if got != 30*time.Second {
			t.Errorf("unset: want 30s, got %v", got)
		}
	})

	t.Run("explicit value", func(t *testing.T) {
		reg := &config.Registry{
			System: config.SystemSettings{
				DelegationEnvelopeWaitSeconds: 45,
			},
		}
		got := delegationEnvelopeWait(reg)
		if got != 45*time.Second {
			t.Errorf("explicit 45: want 45s, got %v", got)
		}
	})

	t.Run("negative disables (0s)", func(t *testing.T) {
		reg := &config.Registry{
			System: config.SystemSettings{
				DelegationEnvelopeWaitSeconds: -1,
			},
		}
		got := delegationEnvelopeWait(reg)
		if got != 0 {
			t.Errorf("disabled: want 0s, got %v", got)
		}
	})
}
