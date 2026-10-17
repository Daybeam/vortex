package core

import (
	"os"
	"testing"
)

// TestInvalidateMCPEnvCache verifies that InvalidateMCPEnvCache clears the
// mcpEnvCache so subsequent cachedGetenv calls re-read os.Getenv.
func TestInvalidateMCPEnvCache(t *testing.T) {
	os.Setenv("TEST_MCP_CACHE_KEY", "first_value")
	defer os.Unsetenv("TEST_MCP_CACHE_KEY")

	// Populate cache
	val := cachedGetenv("TEST_MCP_CACHE_KEY")
	if val != "first_value" {
		t.Fatalf("expected first_value, got %s", val)
	}

	// Verify cache is populated
	if _, ok := mcpEnvCache.Load("TEST_MCP_CACHE_KEY"); !ok {
		t.Fatal("cache should be populated after cachedGetenv")
	}

	// Change env var (simulating .env hot-reload)
	os.Setenv("TEST_MCP_CACHE_KEY", "second_value")

	// Without invalidation, cache returns stale value
	val = cachedGetenv("TEST_MCP_CACHE_KEY")
	if val != "first_value" {
		t.Errorf("expected stale first_value before invalidation, got %s", val)
	}

	// Invalidate
	InvalidateMCPEnvCache()

	// After invalidation, cachedGetenv re-reads env
	val = cachedGetenv("TEST_MCP_CACHE_KEY")
	if val != "second_value" {
		t.Errorf("expected second_value after invalidation, got %s", val)
	}
}
