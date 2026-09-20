package providers

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestRegisterConfiguredInstances_MultiKeyExpansion(t *testing.T) {
	reg, _ := config.NewRegistry("")
	reg.Providers["openai-multi"] = &config.ProviderConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		APIKeys:  []string{"key-1", "key-2", "key-3"},
		PoolID:   "openai-multi",
	}

	// RegisterConfiguredInstances normally logs, so we don't have an easy way to check internal state
	// without inspecting GlobalRouter slots.
	RegisterConfiguredInstances(reg, config.ExternalRuntimes{}, nil)

	// Check GlobalRouter slots
	GlobalRouter.mu.RLock()
	defer GlobalRouter.mu.RUnlock()

	count := 0
	for _, slot := range GlobalRouter.slots {
		if slot.PoolID == "openai-multi" {
			count++
		}
	}

	if count != 3 {
		t.Errorf("Expected 3 slots for openai-multi pool, got %d", count)
	}
}
