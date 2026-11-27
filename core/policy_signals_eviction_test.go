package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// Tests for P2 auto-eviction: families with ConsecutiveFailures >= 3 are
// excluded from the policy decision surface in Collect().
// Spec: docs/completed/2026-11-27/MEMORY_TWO_TIER_KEY.md §5 (P2)

func TestSignalCollector_EvictedFamily_ExcludedFromDecisionSurface(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         5,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 3,
					ConsecutiveFailures: 3,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_a"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 0 {
		t.Fatalf("HistoricalAvgTurns = %v, want 0 (evicted family must not contribute)", signals.HistoricalAvgTurns)
	}
}

func TestSignalCollector_NonEvictedFamily_StillIncluded(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         5,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 3,
					ConsecutiveFailures: 2,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_a"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 10 {
		t.Fatalf("HistoricalAvgTurns = %v, want 10 (family with 2 failures should still contribute)", signals.HistoricalAvgTurns)
	}
}

func TestSignalCollector_EvictedFamily_BoundaryCase_Exactly3(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         5,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 3,
					ConsecutiveFailures: 3,
				},
				"cap_b|r2": {
					SampleCount:         5,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        20,
					Capabilities:        []string{"cap_b"},
					VerifiedSampleCount: 3,
					ConsecutiveFailures: 2,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_b"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 20 {
		t.Fatalf("HistoricalAvgTurns = %v, want 20 (cap_b with 2 failures should contribute; cap_a with 3 should be excluded)", signals.HistoricalAvgTurns)
	}
}

func TestSignalCollector_EvictedFamily_MixedEvictedAndHealthy(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         5,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 3,
					ConsecutiveFailures: 5,
				},
				"cap_a|r2": {
					SampleCount:         4,
					AvgConfidence:       0.9,
					AvgTurnsUsed:        15,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 2,
					ConsecutiveFailures: 0,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_a"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 15 {
		t.Fatalf("HistoricalAvgTurns = %v, want 15 (only the healthy cap_a family should contribute; the evicted one must be skipped)", signals.HistoricalAvgTurns)
	}
}
