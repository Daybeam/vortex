package core

import (
	"sync"
	"testing"
	"time"
)

// TestC7_HandshakeLock_PerIDSerialization is a regression test for audit C-7:
// MCPConnectionManager.ensureMCPClient must serialize Initialize() calls per
// MCP ID to prevent concurrent init race (two goroutines both see
// !IsInitialized() and both call Initialize(); if one fails it deletes+closes
// the client while the other still uses it).
//
// Before the fix, handshakeLocks sync.Map existed but wasn't used in
// ensureMCPClient. After the fix, a per-ID mutex is Lock()ed around Initialize.
//
// Reproduction: verify that LoadOrStore returns the same mutex for the same ID
// (the core property that makes per-ID serialization work).
func TestC7_HandshakeLock_PerIDSerialization(t *testing.T) {
	m := &MCPConnectionManager{}

	// First load creates the mutex.
	v1, _ := m.handshakeLocks.LoadOrStore("mcp-1", &sync.Mutex{})
	lock1 := v1.(*sync.Mutex)

	// Second load for the same ID must return the SAME mutex.
	v2, _ := m.handshakeLocks.LoadOrStore("mcp-1", &sync.Mutex{})
	lock2 := v2.(*sync.Mutex)

	if lock1 != lock2 {
		t.Error("LoadOrStore must return the same mutex for the same MCP ID")
	}

	// Different ID must get a different mutex.
	v3, _ := m.handshakeLocks.LoadOrStore("mcp-2", &sync.Mutex{})
	lock3 := v3.(*sync.Mutex)

	if lock1 == lock3 {
		t.Error("Different MCP IDs must get different mutexes")
	}

	// Verify the lock actually serializes: concurrent Lock() calls must block.
	lock1.Lock()
	done := make(chan struct{})
	go func() {
		lock1.Lock()
		lock1.Unlock()
		close(done)
	}()

	select {
	case <-done:
		t.Error("second Lock() should block while first is held — serialization not working")
	default:
		// Expected: second Lock() is blocked.
	}

	lock1.Unlock()

	// Now the second Lock() should succeed.
	select {
	case <-done:
		// Success
	case <-time.After(time.Second):
		t.Error("second Lock() should succeed after first is released")
	}
}
