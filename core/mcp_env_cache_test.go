package core

import (
	"os"
	"sync"
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

// TestCachedGetenv_NonExistentKey verifies that a non-existent env var
// returns an empty string and is cached (so subsequent calls don't
// re-call os.Getenv).
func TestCachedGetenv_NonExistentKey(t *testing.T) {
	key := "TEST_MCP_CACHE_NONEXISTENT_KEY"
	os.Unsetenv(key)
	defer os.Unsetenv(key)

	// First call: os.Getenv returns "", caches ""
	val := cachedGetenv(key)
	if val != "" {
		t.Errorf("expected empty string for non-existent key, got %q", val)
	}

	// Verify it was cached
	if _, ok := mcpEnvCache.Load(key); !ok {
		t.Error("non-existent key should still be cached (with empty value)")
	}

	// Second call should return cached ""
	val = cachedGetenv(key)
	if val != "" {
		t.Errorf("expected cached empty string, got %q", val)
	}
}

// TestInvalidateMCPEnvCache_EmptyCache verifies that invalidating an
// empty cache is a safe no-op.
func TestInvalidateMCPEnvCache_EmptyCache(t *testing.T) {
	// Clear the cache first
	InvalidateMCPEnvCache()

	// Verify cache is empty
	count := 0
	mcpEnvCache.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("cache should be empty after invalidation, got %d entries", count)
	}

	// Invalidate again — should be safe
	InvalidateMCPEnvCache()
}

// TestInvalidateMCPEnvCache_MultipleKeys verifies that invalidation
// clears all cached keys, not just one.
func TestInvalidateMCPEnvCache_MultipleKeys(t *testing.T) {
	keys := []string{"TEST_MCP_MULTI_1", "TEST_MCP_MULTI_2", "TEST_MCP_MULTI_3"}
	for i, k := range keys {
		os.Setenv(k, "val"+string(rune('0'+i)))
		defer os.Unsetenv(k)
	}

	// Populate cache for all keys
	for _, k := range keys {
		cachedGetenv(k)
	}

	// Verify all are cached
	for _, k := range keys {
		if _, ok := mcpEnvCache.Load(k); !ok {
			t.Fatalf("key %q should be cached", k)
		}
	}

	// Invalidate
	InvalidateMCPEnvCache()

	// Verify all are gone
	for _, k := range keys {
		if _, ok := mcpEnvCache.Load(k); ok {
			t.Errorf("key %q should have been cleared by invalidation", k)
		}
	}
}

// TestCachedGetenv_ConcurrentAccess verifies that concurrent calls to
// cachedGetenv and InvalidateMCPEnvCache do not panic or race.
// Run with -race to detect data races.
func TestCachedGetenv_ConcurrentAccess(t *testing.T) {
	key := "TEST_MCP_CACHE_CONCURRENT"
	os.Setenv(key, "concurrent_value")
	defer os.Unsetenv(key)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cachedGetenv(key)
		}()

		if i%10 == 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				InvalidateMCPEnvCache()
			}()
		}
	}
	wg.Wait()

	// Final verification: the key should resolve correctly
	val := cachedGetenv(key)
	if val != "concurrent_value" {
		t.Errorf("expected concurrent_value, got %q", val)
	}
}
