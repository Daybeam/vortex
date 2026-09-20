package core

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestMCPConnectionManager_DiscoveryLock_PerMCPSerialization is a regression
// test for audit finding H8: concurrent buildMCPServers calls for the same
// MCP raced on FullToolDefinitions. The fix adds per-MCP discoveryLocks so
// access is serialized per MCP ID.
//
// We verify that:
//  1. The same MCP ID always returns the same mutex (serialization).
//  2. Different MCP IDs get different mutexes (no cross-contamination).
//  3. Concurrent goroutines holding the same lock are serialized (only one
//     runs at a time).
func TestMCPConnectionManager_DiscoveryLock_PerMCPSerialization(t *testing.T) {
	mgr := NewMCPConnectionManager(&config.Registry{}, nil)

	// 1. Same ID → same mutex
	m1 := mgr.discoveryLock("mcp-a")
	m2 := mgr.discoveryLock("mcp-a")
	if m1 != m2 {
		t.Fatal("same MCP ID returned different mutexes — not serialized")
	}

	// 2. Different IDs → different mutexes
	m3 := mgr.discoveryLock("mcp-b")
	if m1 == m3 {
		t.Fatal("different MCP IDs returned same mutex — cross-contamination")
	}

	// 3. Concurrent goroutines are serialized — only one holds the lock at a time
	var concurrent int32
	var maxConcurrent int32
	var done sync.WaitGroup

	for i := 0; i < 10; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			lock := mgr.discoveryLock("mcp-a")
			lock.Lock()
			cur := atomic.AddInt32(&concurrent, 1)
			// Track max concurrency — should never exceed 1
			for {
				max := atomic.LoadInt32(&maxConcurrent)
				if cur <= max || atomic.CompareAndSwapInt32(&maxConcurrent, max, cur) {
					break
				}
			}
			atomic.AddInt32(&concurrent, -1)
			lock.Unlock()
		}()
	}
	done.Wait()

	if max := atomic.LoadInt32(&maxConcurrent); max > 1 {
		t.Fatalf("max concurrent holders = %d, expected 1 — lock not serializing", max)
	}
}
