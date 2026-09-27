package extreme_tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/safelimits"
)

// TestExtreme_RapidConfigChanges verifies that rapid concurrent
// reads and writes to a config file don't cause races or corruption.
//
// Extreme scenario: Administrator rapidly modifies config while
// tasks are actively reading it.
//
// T-C16 fix: previously the reader goroutines silently ignored unmarshal
// errors and the test only checked the final config. Now it tracks
// unmarshal failures and asserts the final config has the expected version.
func TestExtreme_RapidConfigChanges(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Initial config
	initial := map[string]any{"version": 0, "roles": map[string]string{}}
	data, _ := json.Marshal(initial)
	os.WriteFile(configPath, data, 0644)

	var wg sync.WaitGroup
	var unmarshalFailures int32 // atomic counter for unmarshal errors

	// Writer: 100 rapid config updates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 100; i++ {
			cfg := map[string]any{
				"version": i,
				"roles":   map[string]string{"role-1": "active"},
			}
			data, _ := json.Marshal(cfg)
			os.WriteFile(configPath, data, 0644)
			time.Sleep(time.Millisecond)
		}
	}()

	// Readers: 10 goroutines reading config concurrently
	for r := 0; r < 10; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				data, err := os.ReadFile(configPath)
				if err != nil {
					continue
				}
				var cfg map[string]any
				if err := json.Unmarshal(data, &cfg); err != nil {
					atomic.AddInt32(&unmarshalFailures, 1)
				}
				time.Sleep(time.Millisecond)
			}
		}(r)
	}

	wg.Wait()

	// Verify final config is valid JSON with expected version
	final, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read final config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(final, &cfg); err != nil {
		t.Errorf("final config is corrupted: %v", err)
	}
	if version, ok := cfg["version"].(float64); !ok || int(version) != 100 {
		t.Errorf("final config version mismatch: got %v, expected 100", cfg["version"])
	}

	// Log unmarshal failures for diagnostics (some are expected during concurrent writes)
	t.Logf("concurrent read unmarshal failures: %d/1000 (expected some during concurrent writes)", unmarshalFailures)
}

// TestExtreme_BoundedRead_SuperLargeResponse verifies that reading
// from a malicious server returning >50MB is capped at 50MB.
//
// Extreme scenario: MCP tool returns a 200MB response (attack or bug).
func TestExtreme_BoundedRead_SuperLargeResponse(t *testing.T) {
	// Server that returns 200MB of data
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		// Write 200MB in chunks (don't allocate all at once)
		chunk := make([]byte, 1024*1024) // 1MB
		for i := 0; i < 200; i++ {
			w.Write(chunk)
		}
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Use bounded read — should cap at 50MB
	bounded := io.LimitReader(resp.Body, safelimits.MaxResponseBody)
	n, err := io.Copy(io.Discard, bounded)
	if err != nil {
		t.Fatalf("bounded read failed: %v", err)
	}

	if n != safelimits.MaxResponseBody {
		t.Errorf("bounded read got %d bytes, expected %d (50MiB)", n, safelimits.MaxResponseBody)
	}
}

// TestExtreme_BoundedRead_NormalResponseUnaffected verifies that
// normal-sized responses (<50MB) are not truncated.
//
// Extreme scenario: Normal MCP tool returns 1MB response.
func TestExtreme_BoundedRead_NormalResponseUnaffected(t *testing.T) {
	normalSize := 1024 * 1024 // 1MB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		data := make([]byte, normalSize)
		w.Write(data)
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	bounded := io.LimitReader(resp.Body, safelimits.MaxResponseBody)
	n, err := io.Copy(io.Discard, bounded)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if n != int64(normalSize) {
		t.Errorf("normal response truncated: got %d, expected %d", n, normalSize)
	}
}

// TestExtreme_ContextTimeout_LongOperation verifies that a context
// with timeout actually cancels a long-running operation.
//
// Extreme scenario: Task hangs indefinitely, context timeout must fire.
func TestExtreme_ContextTimeout_LongOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Simulate a 10-second operation
		select {
		case <-ctx.Done():
			done <- ctx.Err()
			return
		case <-time.After(10 * time.Second):
			done <- fmt.Errorf("operation completed (should have been cancelled)")
			return
		}
	}()

	err := <-done
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}
