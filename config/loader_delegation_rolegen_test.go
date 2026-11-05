package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_DelegationMode_SuppressesRoleGen is a regression test for the
// ephemeral role gen defect (2026-11-05): DelegationMode assumes no provider
// is available, but EnableEphemeralRoleGen/EnableDynamicRoleGen default to
// true. LogInterceptor runs before tryDelegationMode and would call
// provider.Complete() for missing roles — violating the zero-outcall contract.
//
// The fix: when DelegationMode is true, suppress both role gen flags in the
// config loader so the interceptor falls through to tryDefaultRoleFallback
// instead of attempting a provider call.
//
// Before the fix: EnableEphemeralRoleGen == true even in delegation mode.
// After  the fix: EnableEphemeralRoleGen == false && EnableDynamicRoleGen == false.

func TestLoad_DelegationMode_Env_SuppressesRoleGen(t *testing.T) {
	os.Setenv("VORTEX_DELEGATION_MODE", "1")
	defer os.Unsetenv("VORTEX_DELEGATION_MODE")

	reg, err := NewRegistry("this_config_does_not_exist_role_gen_regression.json")
	if err != nil {
		t.Fatalf("NewRegistry on missing config should bootstrap, got error: %v", err)
	}
	if !reg.System.DelegationMode {
		t.Fatal("VORTEX_DELEGATION_MODE=1 was not applied")
	}
	if reg.EnableEphemeralRoleGen {
		t.Error("EnableEphemeralRoleGen should be false in delegation mode — " +
			"LogInterceptor would call provider.Complete() for missing roles, " +
			"violating the zero-outcall contract")
	}
	if reg.EnableDynamicRoleGen {
		t.Error("EnableDynamicRoleGen should be false in delegation mode — " +
			"same provider call risk as EnableEphemeralRoleGen")
	}
}

func TestLoad_DelegationMode_Config_SuppressesRoleGen(t *testing.T) {
	os.Unsetenv("VORTEX_DELEGATION_MODE")

	// NOTE (2026-11-05): this case is necessary but not sufficient. Its
	// "providers": {} sends loadLocked down the empty-providers fallback, which
	// re-runs applySystemDefaults() at the very end — so it passed even while
	// the fix was being undone on the normal path. The case that covers the
	// ordering is TestLoad_DelegationMode_ConfigWithProviders_* below.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{"system": {"delegation_mode": true}, "providers": {}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if !reg.System.DelegationMode {
		t.Fatal("config.json delegation_mode=true was not applied")
	}
	if reg.EnableEphemeralRoleGen {
		t.Error("EnableEphemeralRoleGen should be false in delegation mode")
	}
	if reg.EnableDynamicRoleGen {
		t.Error("EnableDynamicRoleGen should be false in delegation mode")
	}
}

func TestLoad_NoDelegationMode_PreservesRoleGenDefaults(t *testing.T) {
	os.Unsetenv("VORTEX_DELEGATION_MODE")

	reg, err := NewRegistry("this_config_does_not_exist_no_delegation.json")
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if reg.System.DelegationMode {
		t.Fatal("DelegationMode should be false by default")
	}
	// Role gen defaults should be preserved when delegation mode is off.
	if !reg.EnableEphemeralRoleGen {
		t.Error("EnableEphemeralRoleGen should default to true when delegation mode is off")
	}
	if !reg.EnableDynamicRoleGen {
		t.Error("EnableDynamicRoleGen should default to true when delegation mode is off")
	}
}

// TestLoad_DelegationMode_ConfigWithProviders_SuppressesRoleGen covers the
// ordering hole found on 2026-11-05: loadLocked assigns cfg.EnableDynamicRoleGen
// / cfg.EnableEphemeralRoleGen AFTER applySystemDefaults() has run, so a config
// that has providers (the normal case, which skips the bootstrap fallback) and
// turns role gen on explicitly used to end up with both flags true even in
// delegation mode. Measured before the fix: ephemeral=true dynamic=true.
func TestLoad_DelegationMode_ConfigWithProviders_SuppressesRoleGen(t *testing.T) {
	os.Unsetenv("VORTEX_DELEGATION_MODE")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
	  "default_provider": "p1",
	  "providers": {"p1": {"provider": "openai", "model": "m", "api_key": "k"}},
	  "enable_dynamic_role_gen": true,
	  "enable_ephemeral_role_gen": true,
	  "system": {"delegation_mode": true}
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if len(reg.Providers) == 0 {
		t.Fatal("fixture precondition broken: providers must be non-empty so the " +
			"bootstrap fallback (which re-applies defaults last) is NOT taken")
	}
	if !reg.System.DelegationMode {
		t.Fatal("config.json delegation_mode=true was not applied")
	}
	if reg.EnableEphemeralRoleGen || reg.EnableDynamicRoleGen {
		t.Errorf("delegation mode must suppress role gen even when the config turns it on "+
			"(ephemeral=%v dynamic=%v) — LogInterceptor would call provider.Complete()",
			reg.EnableEphemeralRoleGen, reg.EnableDynamicRoleGen)
	}
}

// TestLoad_NoDelegationMode_ConfigWithProviders_KeepsRoleGen is the negative
// half: the suppression must not leak into non-delegation deployments.
func TestLoad_NoDelegationMode_ConfigWithProviders_KeepsRoleGen(t *testing.T) {
	os.Unsetenv("VORTEX_DELEGATION_MODE")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
	  "default_provider": "p1",
	  "providers": {"p1": {"provider": "openai", "model": "m", "api_key": "k"}},
	  "enable_dynamic_role_gen": true,
	  "enable_ephemeral_role_gen": true,
	  "system": {"delegation_mode": false}
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if reg.System.DelegationMode {
		t.Fatal("delegation mode should be off")
	}
	if !reg.EnableEphemeralRoleGen || !reg.EnableDynamicRoleGen {
		t.Errorf("non-delegation config must keep role gen on (ephemeral=%v dynamic=%v)",
			reg.EnableEphemeralRoleGen, reg.EnableDynamicRoleGen)
	}
}
