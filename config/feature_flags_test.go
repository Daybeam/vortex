package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFeatureFlags_FromConfigJSON verifies that feature_flags in config.json
// are parsed into SystemSettings.FeatureFlags.
func TestFeatureFlags_FromConfigJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
		"providers": {"default": {"provider": "openai", "model": "gpt-4o"}},
		"system": {
			"feature_flags": {
				"debug": true,
				"core_tools_order_last": true,
				"disable_force_tool": true,
				"disable_egress_redaction": true,
				"disable_cgroup_sandbox": true,
				"disable_pattern_kill": true,
				"disable_thinking": true,
				"eval_obs": true,
				"fs_only_streak_limit": 7
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ff := reg.System.FeatureFlags
	checks := []struct {
		name string
		got  bool
	}{
		{"Debug", ff.Debug},
		{"CoreToolsOrderLast", ff.CoreToolsOrderLast},
		{"DisableForceTool", ff.DisableForceTool},
		{"DisableEgressRedaction", ff.DisableEgressRedaction},
		{"DisableCgroupSandbox", ff.DisableCgroupSandbox},
		{"DisablePatternKill", ff.DisablePatternKill},
		{"DisableThinking", ff.DisableThinking},
		{"EvalObs", ff.EvalObs},
	}
	for _, c := range checks {
		if !c.got {
			t.Errorf("FeatureFlags.%s = false, want true", c.name)
		}
	}
	if ff.FSOnlyStreakLimit != 7 {
		t.Errorf("FeatureFlags.FSOnlyStreakLimit = %d, want 7", ff.FSOnlyStreakLimit)
	}
}

// TestFeatureFlags_DefaultsAllFalse verifies that without config or env vars,
// all feature flags default to false/zero.
func TestFeatureFlags_DefaultsAllFalse(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
		"providers": {"default": {"provider": "openai", "model": "gpt-4o"}}
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ff := reg.System.FeatureFlags
	if ff.Debug || ff.CoreToolsOrderLast || ff.DisableForceTool ||
		ff.DisableEgressRedaction || ff.DisableCgroupSandbox ||
		ff.DisablePatternKill || ff.DisableThinking || ff.EvalObs {
		t.Errorf("FeatureFlags should all be false by default, got %+v", ff)
	}
	if ff.FSOnlyStreakLimit != 0 {
		t.Errorf("FSOnlyStreakLimit should be 0 by default, got %d", ff.FSOnlyStreakLimit)
	}
}

// TestFeatureFlags_EnvOverride verifies that env vars override config.json values.
func TestFeatureFlags_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// Config sets debug=true, but env will override disable_force_tool=true
	configJSON := `{
		"providers": {"default": {"provider": "openai", "model": "gpt-4o"}},
		"system": {
			"feature_flags": {
				"debug": true,
				"disable_force_tool": false
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Set env var to override disable_force_tool
	os.Setenv("ORCH_DISABLE_FORCE_TOOL", "1")
	defer os.Unsetenv("ORCH_DISABLE_FORCE_TOOL")

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ff := reg.System.FeatureFlags
	if !ff.Debug {
		t.Error("Debug should be true from config.json")
	}
	if !ff.DisableForceTool {
		t.Error("DisableForceTool should be true from env override")
	}
}

// TestFeatureFlags_EnvOverrideFalse verifies that env var "0" sets flag to false
// even when config.json has it true.
func TestFeatureFlags_EnvOverrideFalse(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
		"providers": {"default": {"provider": "openai", "model": "gpt-4o"}},
		"system": {
			"feature_flags": {
				"disable_egress_redaction": true
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	os.Setenv("ORCH_DISABLE_EGRESS_REDACTION", "0")
	defer os.Unsetenv("ORCH_DISABLE_EGRESS_REDACTION")

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	if reg.System.FeatureFlags.DisableEgressRedaction {
		t.Error("DisableEgressRedaction should be false from env override '0'")
	}
}

// TestFeatureFlags_FSOnlyStreakLimit_EnvOverride verifies env var override for int flag.
func TestFeatureFlags_FSOnlyStreakLimit_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
		"providers": {"default": {"provider": "openai", "model": "gpt-4o"}}
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	os.Setenv("ORCH_FS_ONLY_STREAK_LIMIT", "5")
	defer os.Unsetenv("ORCH_FS_ONLY_STREAK_LIMIT")

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	if reg.System.FeatureFlags.FSOnlyStreakLimit != 5 {
		t.Errorf("FSOnlyStreakLimit = %d, want 5", reg.System.FeatureFlags.FSOnlyStreakLimit)
	}
}

// TestInvalidateAllEnvAPIKeys verifies that InvalidateAllEnvAPIKeys clears the cache.
func TestInvalidateAllEnvAPIKeys(t *testing.T) {
	// Populate the cache
	os.Setenv("TEST_INVALIDATE_KEY", "cached_value")
	defer os.Unsetenv("TEST_INVALIDATE_KEY")

	pc := &ProviderConfig{APIKeyEnv: "TEST_INVALIDATE_KEY"}
	val := pc.ResolveAPIKey(nil) // nil SecretProvider → uses envAPIKeyCache
	if val != "cached_value" {
		t.Fatalf("expected cached_value, got %s", val)
	}

	// Verify the cache is populated
	if _, ok := envAPIKeyCache.Load("TEST_INVALIDATE_KEY"); !ok {
		t.Fatal("cache should be populated after ResolveAPIKey")
	}

	// Invalidate
	InvalidateAllEnvAPIKeys()

	// Verify the cache is cleared
	if _, ok := envAPIKeyCache.Load("TEST_INVALIDATE_KEY"); ok {
		t.Error("cache should be empty after InvalidateAllEnvAPIKeys")
	}

	// Change the env var and resolve again — should get the new value
	os.Setenv("TEST_INVALIDATE_KEY", "new_value")
	val = pc.ResolveAPIKey(nil)
	if val != "new_value" {
		t.Errorf("expected new_value after invalidation, got %s", val)
	}
}
