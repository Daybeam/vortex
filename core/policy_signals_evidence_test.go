package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// Tests for P1 read-side threshold: unverified families (VerifiedSampleCount==0)
// require ≥5 samples instead of ≥3 to enter the policy decision surface.
// Spec: docs/completed/2026-11-27/MEMORY_TWO_TIER_KEY.md §5

func TestSignalCollector_VerifiedFamily_PassesAt3Samples(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         3,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 2,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_a"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 10 {
		t.Fatalf("HistoricalAvgTurns = %v, want 10 (verified family passes at 3 samples)", signals.HistoricalAvgTurns)
	}
}

func TestSignalCollector_UnverifiedFamily_BlockedAt3Samples(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         3,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 0,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_a"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 0 {
		t.Fatalf("HistoricalAvgTurns = %v, want 0 (unverified family blocked at 3 samples, needs ≥5)", signals.HistoricalAvgTurns)
	}
}

func TestSignalCollector_UnverifiedFamily_PassesAt5Samples(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         5,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        12,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 0,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_a"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 12 {
		t.Fatalf("HistoricalAvgTurns = %v, want 12 (unverified family passes at 5 samples)", signals.HistoricalAvgTurns)
	}
}

func TestSignalCollector_MixedVerifiedAndUnverifiedFamilies(t *testing.T) {
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				// Verified family: passes at 3 samples
				"cap_a|r1": {
					SampleCount:         3,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 2,
				},
				// Unverified family: blocked at 3 samples (needs ≥5)
				"cap_b|r2": {
					SampleCount:         3,
					AvgConfidence:       0.8,
					AvgTurnsUsed:        20,
					Capabilities:        []string{"cap_b"},
					VerifiedSampleCount: 0,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)

	// cap_a should pass (verified, 3 samples)
	roleA := &config.Role{BaseCapability: "cap_a"}
	signalsA := collector.Collect(context.Background(), &SpawnRequest{Task: "test"}, roleA)
	if signalsA.HistoricalAvgTurns != 10 {
		t.Fatalf("cap_a: HistoricalAvgTurns = %v, want 10 (verified family at 3 samples)", signalsA.HistoricalAvgTurns)
	}

	// cap_b should be blocked (unverified, only 3 samples)
	roleB := &config.Role{BaseCapability: "cap_b"}
	signalsB := collector.Collect(context.Background(), &SpawnRequest{Task: "test"}, roleB)
	if signalsB.HistoricalAvgTurns != 0 {
		t.Fatalf("cap_b: HistoricalAvgTurns = %v, want 0 (unverified family blocked at 3 samples)", signalsB.HistoricalAvgTurns)
	}
}

func TestSignalCollector_UnverifiedFamily_LowConfidenceStillBlocked(t *testing.T) {
	// Even with ≥5 samples, low confidence should still block
	exp := &fakeExperienceStore{
		GetFamilyStatsSnapshotFn: func() map[string]*store.FamilyStats {
			return map[string]*store.FamilyStats{
				"cap_a|r1": {
					SampleCount:         5,
					AvgConfidence:       0.5, // below 0.75 threshold
					AvgTurnsUsed:        10,
					Capabilities:        []string{"cap_a"},
					VerifiedSampleCount: 0,
				},
			}
		},
	}

	collector := NewStoreBackedSignalCollector(exp, nil, nil)
	role := &config.Role{BaseCapability: "cap_a"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 0 {
		t.Fatalf("HistoricalAvgTurns = %v, want 0 (low confidence blocks regardless of verification)", signals.HistoricalAvgTurns)
	}
}
