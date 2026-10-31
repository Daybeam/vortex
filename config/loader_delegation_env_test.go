package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_NoConfigFile_EnvDelegationModeApplies is a regression test for D6
// (DELEGATION_HANDOVER_REDESIGN.md §2 D6): when no config.json exists,
// loadLocked() used to call bootstrapDefaultsLocked() and return immediately
// WITHOUT calling applySystemDefaults(). This silently dropped all env-var
// overrides — most critically VORTEX_DELEGATION_MODE=1 had no effect on a
// fresh install, the exact user profile that needs delegation mode.
//
// Before the fix: reg.System.DelegationMode == false (env ignored).
// After  the fix: reg.System.DelegationMode == true  (env applied).
func TestLoad_NoConfigFile_EnvDelegationModeApplies(t *testing.T) {
	os.Setenv("VORTEX_DELEGATION_MODE", "1")
	defer os.Unsetenv("VORTEX_DELEGATION_MODE")

	reg, err := NewRegistry("this_config_does_not_exist_d6_regression.json")
	if err != nil {
		t.Fatalf("NewRegistry on missing config should bootstrap, got error: %v", err)
	}
	if !reg.System.DelegationMode {
		t.Fatal("D6 regression: VORTEX_DELEGATION_MODE=1 was ignored when no config file exists; " +
			"applySystemDefaults() is missing on the no-config exit path of loadLocked()")
	}
}

// TestLoad_EmptyProviders_EnvDelegationModeSurvivesBootstrap is a regression
// test for D6 (DELEGATION_HANDOVER_REDESIGN.md §2 D6, second scenario): when
// config.json exists but has zero providers, loadLocked() calls
// bootstrapDefaultsLocked() which reassigns r.System wholesale (loader.go:639),
// wiping the env overrides applied earlier at :240. The fix re-applies
// applySystemDefaults() after bootstrap so env vars survive.
//
// Before the fix: reg.System.DelegationMode == false (bootstrap wiped env).
// After  the fix: reg.System.DelegationMode == true  (env re-applied post-bootstrap).
func TestLoad_EmptyProviders_EnvDelegationModeSurvivesBootstrap(t *testing.T) {
	os.Setenv("VORTEX_DELEGATION_MODE", "1")
	defer os.Unsetenv("VORTEX_DELEGATION_MODE")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// Empty providers → triggers the bootstrapDefaultsLocked fallback branch.
	if err := os.WriteFile(configPath, []byte(`{"providers": {}}`), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry on empty-providers config should succeed via bootstrap, got error: %v", err)
	}
	if !reg.System.DelegationMode {
		t.Fatal("D6 regression: VORTEX_DELEGATION_MODE=1 was wiped by bootstrapDefaultsLocked() " +
			"on the empty-providers path; applySystemDefaults() must be re-called after bootstrap")
	}
	// The bootstrap should have injected a default provider (avoiding the 401
	// "no provider" dead-end that D6 describes).
	if _, ok := reg.Providers["default"]; !ok {
		t.Error("expected bootstrap to inject a 'default' provider on empty-providers config")
	}
}

// TestLoad_EmptyProviders_ConfigDelegationModeSurvivesBootstrap is a regression
// test for D6 §10.2 (DELEGATION_HANDOVER_REDESIGN.md §10.2): when config.json
// has system.delegation_mode=true + providers={}, bootstrapDefaultsLocked()
// used to do `r.System = SystemSettings{...}` (full struct reassignment),
// wiping delegation_mode from config. The fix changes bootstrap to fill
// zero-value fields instead, preserving config.json values.
//
// Before the fix: reg.System.DelegationMode == false (bootstrap wiped config).
// After  the fix: reg.System.DelegationMode == true  (config preserved).
func TestLoad_EmptyProviders_ConfigDelegationModeSurvivesBootstrap(t *testing.T) {
	// Ensure no env var interferes.
	os.Unsetenv("VORTEX_DELEGATION_MODE")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// delegation_mode=true in config + empty providers → triggers bootstrap.
	configJSON := `{"system": {"delegation_mode": true}, "providers": {}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if !reg.System.DelegationMode {
		t.Fatal("D6 §10.2 regression: config.json delegation_mode=true was wiped by " +
			"bootstrapDefaultsLocked() full struct reassignment; " +
			"bootstrap must fill zero-value fields instead")
	}
}
