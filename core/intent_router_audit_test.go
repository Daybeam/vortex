package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// slowEmbeddingClient simulates a slow embedding API. Each Embed call blocks
// for embedDelay, allowing us to verify that RefreshEmbeddings does NOT hold
// the write lock during network I/O.
type slowEmbeddingClient struct {
	mu         sync.Mutex
	callCount  int
	embedDelay time.Duration
}

func (c *slowEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	c.mu.Lock()
	c.callCount++
	c.mu.Unlock()
	select {
	case <-time.After(c.embedDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []float32{0.1, 0.2, 0.3}, nil
}

// TestRefreshEmbeddings_DoesNotHoldLockDuringIO verifies that the write lock
// is NOT held during embedding network I/O. If RefreshEmbeddings held the
// lock during Embed(), concurrent Route() calls (which need a read lock)
// would block until all embeddings complete. With the fix, Route can proceed
// while embeddings are being computed.
func TestRefreshEmbeddings_DoesNotHoldLockDuringIO(t *testing.T) {
	// Use a slow embedding client: each Embed call takes 200ms.
	client := &slowEmbeddingClient{embedDelay: 200 * time.Millisecond}

	router := NewIntentRouter(client)

	// Build a registry with 3 SOPs that all need embedding (fresh cache).
	reg := &config.Registry{
		Mu:   sync.RWMutex{},
		SOPs: make(map[string]*schemas.SOP),
	}
	for i := 0; i < 3; i++ {
		id := "sop-" + string(rune('A'+i))
		reg.SOPs[id] = &schemas.SOP{
			ID:          id,
			Description: "Test SOP " + id,
			Version:     "v1",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start RefreshEmbeddings in a goroutine. It will make 3 slow Embed calls
	// totaling ~600ms. During this time, the write lock should NOT be held.
	refreshDone := make(chan struct{})
	go func() {
		router.RefreshEmbeddings(ctx, reg)
		close(refreshDone)
	}()

	// Give RefreshEmbeddings a moment to start and enter the embedding phase.
	time.Sleep(50 * time.Millisecond)

	// Now try to call Route. If the write lock were held during I/O,
	// Route's read-lock acquisition would block for ~550ms (remaining embeddings).
	// With the fix, the lock is only held briefly for the batch update at the end,
	// so Route should be able to acquire its read lock quickly.
	// We use a short timeout to detect blocking.
	routeCtx, routeCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer routeCancel()

	routeDone := make(chan error, 1)
	go func() {
		_, err := router.Route(routeCtx, reg, "test query", 1)
		routeDone <- err
	}()

	select {
	case <-routeDone:
		// Route completed quickly — the lock was NOT held during I/O. PASS.
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Route was blocked for >150ms — RefreshEmbeddings is holding the write lock during network I/O")
	}

	// Wait for RefreshEmbeddings to finish.
	select {
	case <-refreshDone:
	case <-time.After(3 * time.Second):
		t.Fatal("RefreshEmbeddings did not complete within 3s")
	}
}
