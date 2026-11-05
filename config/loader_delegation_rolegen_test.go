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
