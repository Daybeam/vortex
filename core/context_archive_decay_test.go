package core

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// TestComputeDecayWeight verifies the exponential decay function used for
// temporal ranking of memory items (memory tiering gap C, 2026-10-23).
func TestComputeDecayWeight(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name      string
		timestamp time.Time
		wantApprox float64
		tolerance  float64
	}{
		{"zero_timestamp_no_decay", time.Time{}, 1.0, 0.0},
		{"future_timestamp_no_decay", now.Add(24 * time.Hour), 1.0, 0.0},
		{"now_full_weight", now, 1.0, 0.001},
		{"1_day_ago", now.Add(-24 * time.Hour), 0.989, 0.01},
		{"30_days_ago", now.Add(-30 * 24 * time.Hour), 0.717, 0.01},
		{"90_days_ago_half_life", now.Add(-90 * 24 * time.Hour), 1.0 / math.E, 0.01},
		{"180_days_ago", now.Add(-180 * 24 * time.Hour), 1.0 / (math.E * math.E), 0.01},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeDecayWeight(tc.timestamp, now)
			if math.Abs(got-tc.wantApprox) > tc.tolerance {
				t.Errorf("computeDecayWeight() = %.4f, want ~%.4f (±%.4f)",
					got, tc.wantApprox, tc.tolerance)
			}
		})
	}
}

// TestComputeDecayWeight_MonotonicDecreasing verifies that older items get
// strictly lower weights than newer items.
func TestComputeDecayWeight_MonotonicDecreasing(t *testing.T) {
	now := time.Now()
	recent := computeDecayWeight(now.Add(-1*time.Hour), now)
	old := computeDecayWeight(now.Add(-30*24*time.Hour), now)
	ancient := computeDecayWeight(now.Add(-365*24*time.Hour), now)

	if recent <= old {
		t.Errorf("recent (%.4f) should > old (%.4f)", recent, old)
	}
	if old <= ancient {
		t.Errorf("old (%.4f) should > ancient (%.4f)", old, ancient)
	}
}

// TestSearchWithOptions_DecayWeightRanking verifies that when two items have
// identical content (same BM25 score) but different ages, the more recent
// one ranks first due to the temporal decay weight.
func TestSearchWithOptions_DecayWeightRanking(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	// Two items with identical content → identical BM25 scores.
	// The only differentiator is Timestamp (age).
	recentItem := MemoryItem{
		TaskID:    "task-recent",
		NodeID:    "node-recent",
		Intent:    "unique searchable content",
		Summary:   "same summary",
		Timestamp: time.Now().Add(-1 * time.Hour), // 1 hour ago
	}
	staleItem := MemoryItem{
		TaskID:    "task-stale",
		NodeID:    "node-stale",
		Intent:    "unique searchable content",
		Summary:   "same summary",
		Timestamp: time.Now().Add(-180 * 24 * time.Hour), // 180 days ago
	}

	if err := archive.Append(recentItem); err != nil {
		t.Fatalf("Append recent: %v", err)
	}
	if err := archive.Append(staleItem); err != nil {
		t.Fatalf("Append stale: %v", err)
	}

	results := archive.Search("searchable", nil, 10, "", "")
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// The recent item should rank first due to higher decay weight.
	if results[0].NodeID != "node-recent" {
		t.Errorf("expected node-recent first (decay-weighted ranking), got %s",
			results[0].NodeID)
	}
	if results[1].NodeID != "node-stale" {
		t.Errorf("expected node-stale second, got %s", results[1].NodeID)
	}
}

// TestSearchWithOptions_ZeroTimestampNoDecay verifies that items with zero
// Timestamp (legacy data) are not penalized by the decay weight.
func TestSearchWithOptions_ZeroTimestampNoDecay(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	// Legacy item with zero Timestamp — should get weight 1.0 (no decay).
	legacyItem := MemoryItem{
		TaskID:    "task-legacy",
		NodeID:    "node-legacy",
		Intent:    "legacy searchable content",
		Summary:   "legacy summary",
		Timestamp: time.Time{}, // zero
	}
	recentItem := MemoryItem{
		TaskID:    "task-recent",
		NodeID:    "node-recent",
		Intent:    "legacy searchable content",
		Summary:   "legacy summary",
		Timestamp: time.Now(),
	}

	if err := archive.Append(legacyItem); err != nil {
		t.Fatalf("Append legacy: %v", err)
	}
	if err := archive.Append(recentItem); err != nil {
		t.Fatalf("Append recent: %v", err)
	}

	results := archive.Search("legacy", nil, 10, "", "")
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// Both should have weight 1.0, so ranking is determined by BM25 alone
	// (identical content → identical score → either can be first).
	// Just verify both are present.
	found := map[string]bool{}
	for _, r := range results {
		found[r.NodeID] = true
	}
	if !found["node-legacy"] || !found["node-recent"] {
		t.Errorf("expected both items in results, got: %v", found)
	}
}
