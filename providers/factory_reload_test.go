package providers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestReloadScriptProvider_UnwrapsRetryingProvider is a regression guard for W1:
// ReloadScriptProvider must find the *ScriptProvider inside the wrapper chain
// (RateLimitedProvider → RetryingProvider → ScriptProvider) that Get() installs.
// Before the unwrap fix, the type assertion p.(*ScriptProvider) always failed
// because cache stores *RetryingProvider, so reload was impossible.
func TestReloadScriptProvider_UnwrapsRetryingProvider(t *testing.T) {
	scriptPath := writeMinimalLuaScript(t)
	defer os.Remove(scriptPath)

	cfg := &config.ProviderConfig{
		Provider: "script",
		BaseURL:  "http://localhost:9999",
		Extra:    map[string]any{"script_path": scriptPath},
	}

	defer ClearCache()
	if _, err := Get(cfg, config.ExternalRuntimes{}); err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	listed := GetScriptProviders()
	if len(listed) == 0 {
		t.Fatal("GetScriptProviders() returned empty — unwrap is broken")
	}
	found := false
	for _, m := range listed {
		if m["script_path"] == scriptPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("script %q not found in GetScriptProviders() result: %v", scriptPath, listed)
	}

	if err := ReloadScriptProvider("test_script", cfg); err != nil {
		t.Fatalf("ReloadScriptProvider failed: %v", err)
	}
}

// TestReloadScriptProvider_NotFound verifies the error path when the provider
// is not in the cache.
func TestReloadScriptProvider_NotFound(t *testing.T) {
	defer ClearCache()
	cfg := &config.ProviderConfig{
		Provider: "script",
		BaseURL:  "http://localhost:9999",
	}
	err := ReloadScriptProvider("nonexistent", cfg)
	if err == nil {
		t.Fatal("expected error for nonexistent provider")
	}
}

// TestReloadScriptProvider_NotAScript verifies the error path when the cached
// provider is not a script provider.
func TestReloadScriptProvider_NotAScript(t *testing.T) {
	cfg := &config.ProviderConfig{
		Provider: "ollama",
		BaseURL:  "http://localhost:11434",
	}
	defer ClearCache()
	if _, err := Get(cfg, config.ExternalRuntimes{}); err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	err := ReloadScriptProvider("ollama_provider", cfg)
	if err == nil {
		t.Fatal("expected error for non-script provider")
	}
}

func writeMinimalLuaScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test_provider.lua")
	content := `provider = {
  name = "test_script",
  complete = function(req)
    return { text = "hello from " .. req.user }
  end
}
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
