package providers

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestRegisterConfiguredInstances_OptIn_NoInstances_NoOp verifies that a
// ProviderConfig without an Instances list is completely unaffected -- this
// is the "opt-in, zero behavior change for existing configs" guarantee.
func TestRegisterConfiguredInstances_OptIn_NoInstances_NoOp(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"plain": {Provider: "ollama", Model: "llama3.1", BaseURL: "http://noop-instances-test.invalid:9"},
		},
	}

	GlobalRouter.mu.RLock()
	before := len(GlobalRouter.slots)
	GlobalRouter.mu.RUnlock()

	RegisterConfiguredInstances(reg, config.ExternalRuntimes{}, nil)

	GlobalRouter.mu.RLock()
	after := len(GlobalRouter.slots)
	GlobalRouter.mu.RUnlock()

	if after != before {
		t.Errorf("expected no new slots registered for a config without Instances, before=%d after=%d", before, after)
	}
}

// TestRegisterConfiguredInstances_RegistersDistinctSlots is the core
// multi-instance-failover test: two Instances entries under one named
// provider config, distinguished by BaseURL, must both end up as separate,
// independently addressable slots in GlobalRouter -- not collapsed into one.
func TestRegisterConfiguredInstances_RegistersDistinctSlots(t *testing.T) {
	primaryBaseURL := "http://instances-test-primary.invalid:1"
	secondaryBaseURL := "http://instances-test-secondary.invalid:2"

	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"multi": {
				Provider: "ollama",
				Model:    "llama3.1",
				Instances: []config.ProviderInstance{
					{ID: "primary-instances-test", BaseURL: primaryBaseURL},
					{ID: "secondary-instances-test", BaseURL: secondaryBaseURL},
				},
			},
		},
	}

	var logs []string
	RegisterConfiguredInstances(reg, config.ExternalRuntimes{}, func(msg string) {
		logs = append(logs, msg)
	})

	primaryKey := providerCacheKey(&config.ProviderConfig{Provider: "ollama", BaseURL: primaryBaseURL})
	secondaryKey := providerCacheKey(&config.ProviderConfig{Provider: "ollama", BaseURL: secondaryBaseURL})

	if primaryKey == secondaryKey {
		t.Fatal("test setup bug: primary and secondary keys should differ")
	}

	GlobalRouter.mu.RLock()
	primarySlot, primaryOK := GlobalRouter.slots[primaryKey]
	secondarySlot, secondaryOK := GlobalRouter.slots[secondaryKey]
	GlobalRouter.mu.RUnlock()

	if !primaryOK {
		t.Error("expected primary instance to be registered as its own slot")
	}
	if !secondaryOK {
		t.Error("expected secondary instance to be registered as its own slot")
	}
	if primaryOK && secondaryOK && primarySlot == secondarySlot {
		t.Error("expected primary and secondary instances to be distinct slots, got the same slot object")
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 log lines (one per instance), got %d: %v", len(logs), logs)
	}

	// Next() should be able to find a healthy slot in this parent's pool.
	// FIX (2026-07-19): Next() now matches by PoolID (defaulting to the
	// parent config's registry key, "multi" here -- see
	// RegisterConfiguredInstances setting derived.PoolID = job.parentID),
	// not by the bare provider type string. Asserting on "ollama" (the type)
	// was testing the exact cross-provider pooling bug that fix closed --
	// see PoolID's doc comment in config.go for the full incident.
	if _, err := GlobalRouter.Next("multi"); err != nil {
		t.Errorf("expected Next(\"multi\") to find a healthy slot after registration, got: %v", err)
	}
}

// TestRegisterConfiguredInstances_UnsupportedProviderType_LoggedNotPanicked
// verifies a bad instance definition (e.g. a typo'd provider type) is
// reported via logFn and skipped, rather than panicking or aborting the
// whole registration pass.
func TestRegisterConfiguredInstances_UnsupportedProviderType_LoggedNotPanicked(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"bad": {
				Provider: "not-a-real-provider-type-instances-test",
				Instances: []config.ProviderInstance{
					{ID: "bad-instance", APIKeyEnv: "SOME_KEY"},
				},
			},
		},
	}

	var logs []string
	RegisterConfiguredInstances(reg, config.ExternalRuntimes{}, func(msg string) {
		logs = append(logs, msg)
	})

	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 log line for the failed instance, got %d: %v", len(logs), logs)
	}
	if !strings.Contains(logs[0], "failed to register") {
		t.Errorf("expected a failure log message, got: %s", logs[0])
	}
}

// TestRegisterConfiguredInstances_NilRegistry_NoPanic guards a defensive nil
// check -- callers should be able to call this unconditionally at startup
// without worrying about Registry construction order.
func TestRegisterConfiguredInstances_NilRegistry_NoPanic(t *testing.T) {
	RegisterConfiguredInstances(nil, config.ExternalRuntimes{}, nil)
}

// TestRegisterConfiguredInstances_DirectAPIKey_DoesNotCollide verifies the
// providerCacheKey fix (2026-07-11): two instances sharing an empty BaseURL
// and empty APIKeyEnv, but distinguished only by a direct APIKey override,
// must not collapse into a single cache/router slot.
func TestRegisterConfiguredInstances_DirectAPIKey_DoesNotCollide(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"direct-key-multi": {
				Provider: "ollama",
				Model:    "llama3.1",
				BaseURL:  "http://instances-test-directkey.invalid:3",
				Instances: []config.ProviderInstance{
					{ID: "direct-key-a", APIKey: "instances-test-key-a"},
					{ID: "direct-key-b", APIKey: "instances-test-key-b"},
				},
			},
		},
	}

	RegisterConfiguredInstances(reg, config.ExternalRuntimes{}, nil)

	keyA := providerCacheKey(&config.ProviderConfig{Provider: "ollama", BaseURL: "http://instances-test-directkey.invalid:3", APIKey: "instances-test-key-a"})
	keyB := providerCacheKey(&config.ProviderConfig{Provider: "ollama", BaseURL: "http://instances-test-directkey.invalid:3", APIKey: "instances-test-key-b"})

	if keyA == keyB {
		t.Fatal("test setup bug: keyA and keyB should differ")
	}

	GlobalRouter.mu.RLock()
	_, okA := GlobalRouter.slots[keyA]
	_, okB := GlobalRouter.slots[keyB]
	GlobalRouter.mu.RUnlock()

	if !okA || !okB {
		t.Errorf("expected both direct-api-key instances to register as distinct slots, okA=%v okB=%v", okA, okB)
	}
}
