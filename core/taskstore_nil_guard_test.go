package core

import (
	"context"
	"testing"
)

// TestResolveContextAttachments_NilTaskStore_ReturnsNilNotPanic is a direct
// regression test for the nil-pointer panic found in the 2026-08-01 session:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	core.(*Spawner).resolveContextAttachments(...)
//
// A Spawner built via a bare struct literal (the same pattern already used
// by TestCallRemoteMCPTool_* in remote_mcp_test.go, and matching
// DirectExecute's documented contract) has a nil taskStore field. Calling
// s.taskStore.Get directly panics; the fix routes through a nil-safe
// taskStoreGet/taskStoreSet wrapper pair instead.
func TestResolveContextAttachments_NilTaskStore_ReturnsNilNotPanic(t *testing.T) {
	s := &Spawner{}
	attachments := s.resolveContextAttachments(context.Background(), "task1", "step1")
	if attachments != nil {
		t.Fatalf("expected nil attachments for a nil taskStore, got %v", attachments)
	}
}

// TestTaskStoreSet_NilTaskStore_NoPanic covers the write-side wrapper the
// same fix introduced.
func TestTaskStoreSet_NilTaskStore_NoPanic(t *testing.T) {
	s := &Spawner{}
	s.taskStoreSet(context.Background(), "task1", "step1", nil)
}
