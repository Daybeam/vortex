package browser

import (
	"context"
	"testing"
)

// ─── Regression: BrowserVerifier.Verify must not panic on wrong/nil before state ───
// The bug: unchecked type assertion `before.(*BrowserSnapshot)` panics if before
// is nil or a different type. The fix uses `, ok` two-value assertion + error return.

func TestBrowserVerifier_NilBeforeState_NoPanic(t *testing.T) {
	bv := NewBrowserVerifier(map[string]string{})
	// nil before state — must return error, not panic
	_, err := bv.Verify(context.Background(), "", "", nil, nil, nil)
	if err == nil {
		t.Error("expected error for nil before state, got nil")
	}
}

func TestBrowserVerifier_WrongTypeBeforeState_NoPanic(t *testing.T) {
	bv := NewBrowserVerifier(map[string]string{})
	// Wrong type — must return error, not panic
	wrongState := "not-a-snapshot"
	_, err := bv.Verify(context.Background(), "", "", wrongState, nil, nil)
	if err == nil {
		t.Error("expected error for wrong before state type, got nil")
	}
}
