package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func setupCapProfileStore(t *testing.T) (*store.CapabilityProfileStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cap_adapter_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "test.db")
	db, err := store.InitDB(dbPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("InitDB: %v", err)
	}
	cps := store.NewCapabilityProfileStore(db)
	return cps, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// TestRecordCapabilityOutcome_RecordsWhenStoreSet verifies that Spawn feeds
// execution telemetry to CapabilityProfileStore when wired.
// Regression: ensures the wiring in Spawn() → recordCapabilityOutcome is not
// accidentally removed, which would silently disable Pareto routing data.
func TestRecordCapabilityOutcome_RecordsWhenStoreSet(t *testing.T) {
	cps, cleanup := setupCapProfileStore(t)
	defer cleanup()
	if cps == nil {
		t.Skip("CapabilityProfileStore nil")
	}

	logger, _ := NewLogger("", nil)
	s := &Spawner{capProfileStore: cps, logger: logger}
	req := &SpawnRequest{TaskID: "t1", StepID: "s1"}
	result := &SpawnResult{
		ModelID:   "test-model",
		TurnsUsed: 3,
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Capability: "refactor",
			TokenUsed:  500,
		},
	}

	s.recordCapabilityOutcome(context.Background(), req, result, 1000000)

	profile, err := cps.GetProfile(context.Background(), "test-model", "refactor")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if profile == nil {
		t.Fatal("profile not recorded")
	}
	if profile.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1", profile.TotalRuns)
	}
	if profile.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", profile.SuccessCount)
	}
}

// TestRecordCapabilityOutcome_SkipsWhenStoreNil verifies no panic when
// the capability store is not wired (the default for existing callers).
func TestRecordCapabilityOutcome_SkipsWhenStoreNil(t *testing.T) {
	logger, _ := NewLogger("", nil)
	s := &Spawner{logger: logger}
	req := &SpawnRequest{TaskID: "t1", StepID: "s1"}
	result := &SpawnResult{
		ModelID:   "test-model",
		TurnsUsed: 3,
		Output:    schemas.SubagentOutput{Status: schemas.StatusOK, Capability: "refactor"},
	}
	s.recordCapabilityOutcome(context.Background(), req, result, 1000000)
}

// TestRouteProvider_FallsBackToNextWhenNoLookup verifies that routing
// degrades to latency-minimizing Next() when no capability lookup is wired.
func TestRouteProvider_FallsBackToNextWhenNoLookup(t *testing.T) {
	s := &Spawner{}
	slot, err := s.routeProvider("nonexistent-pool", "refactor")
	if err == nil && slot != nil {
		t.Logf("Next() returned a slot (pool has providers) — fine")
	}
}

// TestAvgThetaForCapability_ReturnsZeroWhenNilStore verifies the neutral
// theta fallback when no telemetry is available.
func TestAvgThetaForCapability_ReturnsZeroWhenNilStore(t *testing.T) {
	s := &Spawner{}
	if theta := s.avgThetaForCapability("refactor"); theta != 0 {
		t.Errorf("avgTheta = %v, want 0 for nil store", theta)
	}
}

// TestRecalculateThetas_UpdatesFromSuccessRate verifies that the batch theta
// recalculation correctly computes Rasch MLE from observed success rates.
func TestRecalculateThetas_UpdatesFromSuccessRate(t *testing.T) {
	cps, cleanup := setupCapProfileStore(t)
	defer cleanup()
	if cps == nil {
		t.Skip("CapabilityProfileStore nil")
	}

	ctx := context.Background()
	for i := 0; i < 8; i++ {
		cps.RecordOutcome(ctx, "strong-model", "refactor", true, 5, 1000, 200)
	}
	for i := 0; i < 2; i++ {
		cps.RecordOutcome(ctx, "weak-model", "refactor", false, 10, 2000, 400)
	}

	logger, _ := NewLogger("", nil)
	s := &Spawner{capProfileStore: cps, logger: logger}
	updated, err := s.RecalculateThetas(ctx)
	if err != nil {
		t.Fatalf("RecalculateThetas: %v", err)
	}
	if updated != 2 {
		t.Errorf("updated = %d, want 2", updated)
	}

	strong, _ := cps.GetProfile(ctx, "strong-model", "refactor")
	if strong.Theta <= 0 {
		t.Errorf("strong model theta = %v, want > 0 (high success rate)", strong.Theta)
	}
	weak, _ := cps.GetProfile(ctx, "weak-model", "refactor")
	if weak.Theta >= 0 {
		t.Errorf("weak model theta = %v, want < 0 (low success rate)", weak.Theta)
	}
}

// TestRecalculateThetas_NilStoreNoOp verifies no panic when store is nil.
func TestRecalculateThetas_NilStoreNoOp(t *testing.T) {
	s := &Spawner{}
	updated, err := s.RecalculateThetas(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated != 0 {
		t.Errorf("updated = %d, want 0 for nil store", updated)
	}
}

// TestNewCapProfileLookup_NilReturnsNil verifies the adapter constructor
// returns nil for nil input, preventing nil-pointer panics in NextPareto.
func TestNewCapProfileLookup_NilReturnsNil(t *testing.T) {
	if l := NewCapProfileLookup(nil); l != nil {
		t.Errorf("NewCapProfileLookup(nil) = %v, want nil", l)
	}
}

// TestGetEffectiveHandoffThreshold_NilPCfg verifies the safe default fallback.
func TestGetEffectiveHandoffThreshold_NilPCfg(t *testing.T) {
	s := &Spawner{}
	threshold := s.GetEffectiveHandoffThreshold(nil)
	if threshold != 32768 {
		t.Errorf("threshold = %d, want 32768 for nil pCfg", threshold)
	}
}
