package config

import (
	"encoding/json"
	"os"
	"strings"
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

func boolPtr(b bool) *bool { return &b }

func TestMemoryBankEnabled_DefaultTrue(t *testing.T) {
	// bootstrapDefaultsLocked sets MemoryBankEnabled=true.
	// When config.json doesn't override it, it should stay true.
	r := newTestRegistryForMB()
	r.bootstrapDefaultsLocked()

	if !r.System.MemoryBankEnabledEffective() {
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

	if r.System.MemoryBankEnabledEffective() {
		t.Fatal("expected MemoryBankEnabled=false when env var is 'false'")
	}

	// Now test with "true" overriding a config false
	os.Setenv("VORTEX_MEMORY_BANK_ENABLED", "true")
	r = newTestRegistryForMB()
	r.bootstrapDefaultsLocked()
	r.System.MemoryBankEnabled = boolPtr(false) // simulate config.json setting it to false
	r.applySystemDefaults()

	if !r.System.MemoryBankEnabledEffective() {
		t.Fatal("expected MemoryBankEnabled=true when env var overrides config false")
	}
}

func TestMemoryBankEnabled_EnvUnset_KeepsConfig(t *testing.T) {
	// When env var is unset, config.json value should be preserved.
	os.Unsetenv("VORTEX_MEMORY_BANK_ENABLED")

	r := newTestRegistryForMB()
	r.bootstrapDefaultsLocked()
	// Simulate config.json explicitly setting it to false
	r.System.MemoryBankEnabled = boolPtr(false)
	r.applySystemDefaults()

	if r.System.MemoryBankEnabledEffective() {
		t.Fatal("expected MemoryBankEnabled=false when config sets it false and env is unset")
	}
}

// TestMemoryBankEnabled_ExplicitFalse_SurvivesBootstrap is the N4 regression
// test (DELEGATION_HANDOVER_REDESIGN.md §11.1). Before the fix,
// bootstrapDefaultsLocked had `if !r.System.MemoryBankEnabled { = true }`
// which used the zero value (false) as a sentinel for "unset", so an explicit
// `memory_bank_enabled: false` from config.json was silently overridden to
// true. The *bool change makes nil the sentinel; explicit false is preserved.
func TestMemoryBankEnabled_ExplicitFalse_SurvivesBootstrap(t *testing.T) {
	// Simulate config.json unmarshaling `memory_bank_enabled: false`.
	r := newTestRegistryForMB()
	r.System.MemoryBankEnabled = boolPtr(false)

	// bootstrapDefaultsLocked must NOT override explicit false.
	r.bootstrapDefaultsLocked()

	if r.System.MemoryBankEnabledEffective() {
		t.Fatal("expected explicit false to survive bootstrapDefaultsLocked — " +
			"the *bool nil sentinel must distinguish 'omitted' from 'explicit false' " +
			"(DELEGATION_HANDOVER_REDESIGN.md §11.1 N4)")
	}
}

// TestMemoryBankEnabled_JSONRoundTrip_PreservesExplicitFalse verifies the
// JSON serialization contract: `memory_bank_enabled: false` in config.json
// unmarshals to &false, survives bootstrap, and re-marshals back to false.
func TestMemoryBankEnabled_JSONRoundTrip_PreservesExplicitFalse(t *testing.T) {
	raw := `{"system": {"memory_bank_enabled": false}}`
	r := newTestRegistryForMB()
	if err := json.Unmarshal([]byte(raw), r); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if r.System.MemoryBankEnabled == nil {
		t.Fatal("expected non-nil *bool after unmarshal of explicit false")
	}
	if *r.System.MemoryBankEnabled != false {
		t.Fatal("expected *MemoryBankEnabled == false after unmarshal")
	}

	r.bootstrapDefaultsLocked()
	if r.System.MemoryBankEnabledEffective() {
		t.Fatal("expected false to survive bootstrap after JSON unmarshal")
	}

	out, err := json.Marshal(r.System)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if !strings.Contains(string(out), `"memory_bank_enabled":false`) {
		t.Fatalf("expected marshaled JSON to contain memory_bank_enabled:false, got: %s", out)
	}
}
