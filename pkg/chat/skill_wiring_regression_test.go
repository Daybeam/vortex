package chat

import (
	"context"
	"testing"
	"time"
)

// TestChatSessionStore_SetLifecycleContext_PropagatesToDBCtx is a regression
// guard for audit C-12. Commit 3fcf97bcf added SetLifecycleContext and made
// dbCtx() derive from it, but never wired the call site, so lifecycleCtx
// stayed nil and dbCtx() silently fell back to context.Background() — leaving
// shutdown unable to cancel chat-session DB ops. This pins the contract that
// a set lifecycle ctx actually propagates to dbCtx().
//
// Moved from core/skill_wiring_regression_test.go during pkg/chat extraction
// because it needs the unexported dbCtx() method.
func TestChatSessionStore_SetLifecycleContext_PropagatesToDBCtx(t *testing.T) {
	st := NewChatSessionStore(t.TempDir(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	st.SetLifecycleContext(ctx)
	cancel() // simulate shutdown

	dbCtx, dbCancel := st.dbCtx()
	defer dbCancel()
	select {
	case <-dbCtx.Done():
		// expected: dbCtx inherits the cancelled lifecycleCtx
	case <-time.After(time.Second):
		t.Fatal("dbCtx() did not inherit the cancelled lifecycleCtx (C-12 wiring missing)")
	}
}
