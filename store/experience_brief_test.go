package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// delayedEmbedder is a mock IEmbeddingClient that sleeps before returning
// the embedding. The delay makes the async embedding race deterministic:
// without embeddingWg.Wait() in PersistAll, the goroutine will not have
// finished writing the embedding before PersistAll snapshots the node.
type delayedEmbedder struct {
	delay time.Duration
}

func (m *delayedEmbedder) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	time.Sleep(m.delay)
	return []float32{0.42, 0.84, 0.16}, "delayed-mock", nil
}

// TestPersistAll_WaitsForAsyncEmbedding is a regression test for the
// DecisionPrecedent async embedding timing gap (DB-audit P1).
//
// Without the fix: UpsertDecisionPrecedent fires an async goroutine to
// compute the embedding, but PersistAll snapshots the in-memory node
// immediately. If the goroutine hasn't finished, the persisted node
// has no embedding — it self-heals on the next PersistAll, but a crash
// between cycles loses the embedding permanently.
//
// With the fix: PersistAll calls embeddingWg.Wait() before snapshotting,
// guaranteeing the embedding is present in the first persist.
func TestPersistAll_WaitsForAsyncEmbedding(t *testing.T) {
	es := newTestStore(t)
	es.SetEmbeddingClient(&delayedEmbedder{delay: 100 * time.Millisecond})

	node := &schemas.DecisionNode{
		ID:        "dp-timing-test",
		StepID:    "step-1",
		Turn:      1,
		Type:      "routing",
		Reasoning: "Choose tool A because it has higher affinity for this task type",
		Action:    "tool_a",
		Timestamp: time.Now(),
	}

	if err := es.UpsertDecisionPrecedent(context.Background(), node); err != nil {
		t.Fatalf("UpsertDecisionPrecedent: %v", err)
	}

	// PersistAll should wait for the async embedding goroutine.
	if err := es.PersistAll(context.Background()); err != nil {
		t.Fatalf("PersistAll: %v", err)
	}

	// Read back the persisted JSON and verify the embedding is present.
	data, err := os.ReadFile(filepath.Join(es.dir, "decision_precedents.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var persisted map[string]*schemas.DecisionNode
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	got, ok := persisted["dp-timing-test"]
	if !ok {
		t.Fatalf("node dp-timing-test not found in persisted data")
	}

	if len(got.Embedding) == 0 {
		t.Fatalf("persisted node has no embedding — PersistAll did not wait for async embedding goroutine")
	}

	if got.EmbeddingModel != "delayed-mock" {
		t.Fatalf("expected embedding model 'delayed-mock', got %q", got.EmbeddingModel)
	}

	// Verify the embedding values match what the mock returned.
	expected := []float32{0.42, 0.84, 0.16}
	if len(got.Embedding) != len(expected) {
		t.Fatalf("embedding length mismatch: got %d, want %d", len(got.Embedding), len(expected))
	}
	for i, v := range expected {
		if got.Embedding[i] != v {
			t.Fatalf("embedding[%d]: got %f, want %f", i, got.Embedding[i], v)
		}
	}
}
