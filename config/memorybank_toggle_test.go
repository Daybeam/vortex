package config

import (
	"os"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func newTestRegistryForMB() *Registry {
	return &Registry{
		Providers:   make(map[string]*ProviderConfig),
		Skills:      make(map[string]*Skill),
		MCPs:        make(map[string]*MCPDef),
		Roles:       make(map[string]*Role),
		SOPs:        make(map[string]*schemas.SOP),
		RoleGroups:  make(map[string]*RoleGroup),
		DynamicMCPs: make(map[string]*MCPDef),
		Secrets:     &EnvSecretProvider{},
	}
}

func TestMemoryBankEnabled_DefaultTrue(t *testing.T) {
	// bootstrapDefaultsLocked sets MemoryBankEnabled=true.
	// When config.json doesn't override it, it should stay true.
	r := newTestRegistryForMB()
	r.bootstrapDefaultsLocked()

	if !r.System.MemoryBankEnabled {
		t.Fatal("expected MemoryBankEnabled to default to true")
	}
}

func TestMemoryBankEnabled_EnvOverride(t *testing.T) {
	// Env var VORTEX_MEMORY_BANK_ENABLED=false should disable.
	os.Setenv("VORTEX_MEMORY_BANK_ENABLED", "false")
	defer os.Unsetenv("VORTEX_MEMORY_BANK_ENABLED")

	r := newTestRegistryForMB()
	r.bootstrapDefaultsLocked()
	r.applySystemDefaults()

	if r.System.MemoryBankEnabled {
		t.Fatal("expected MemoryBankEnabled=false when env var is 'false'")
	}

	// Now test with "true" overriding a config false
	os.Setenv("VORTEX_MEMORY_BANK_ENABLED", "true")
	r = newTestRegistryForMB()
	r.bootstrapDefaultsLocked()
	r.System.MemoryBankEnabled = false // simulate config.json setting it to false
	r.applySystemDefaults()

	if !r.System.MemoryBankEnabled {
		t.Fatal("expected MemoryBankEnabled=true when env var overrides config false")
	}
}

func TestMemoryBankEnabled_EnvUnset_KeepsConfig(t *testing.T) {
	// When env var is unset, config.json value should be preserved.
	os.Unsetenv("VORTEX_MEMORY_BANK_ENABLED")

	r := newTestRegistryForMB()
	r.bootstrapDefaultsLocked()
	// Simulate config.json explicitly setting it to false
	r.System.MemoryBankEnabled = false
	r.applySystemDefaults()

	if r.System.MemoryBankEnabled {
		t.Fatal("expected MemoryBankEnabled=false when config sets it false and env is unset")
	}
}
