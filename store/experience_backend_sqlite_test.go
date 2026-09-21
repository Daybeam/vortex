package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteExperienceBackend(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "exp_store_test")
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteExperienceBackend(db)
	ctx := context.Background()

	// 1. Test TaskPattern
	p := &TaskPattern{
		ID:             "pat_1",
		TaskType:       "test_task",
		SampleCount:    5,
		AvgConfidence:  0.85,
		LastSeen:       time.Now().Round(time.Second),
		Embedding:      []float32{0.1, 0.2, 0.3},
		EmbeddingModel: "text-embedding-3-small",
	}
	if err := backend.SaveTaskPattern(ctx, p); err != nil {
		t.Errorf("SaveTaskPattern failed: %v", err)
	}

	// 2. Test RoleProfile
	rp := &RoleProfile{
		RoleID:        "role_1",
		TotalRuns:     10,
		SuccessCount:  8,
		AvgConfidence: 0.9,
		LastUpdated:   time.Now().Round(time.Second),
	}
	if err := backend.SaveRoleProfile(ctx, rp); err != nil {
		t.Errorf("SaveRoleProfile failed: %v", err)
	}

	// 3. Test RouteWeight
	rw := &RouteWeight{
		RoleID:       "role_1",
		ModelID:      "model_1",
		Capability:   "cap_1",
		SkillID:      "skill_1",
		TotalRuns:    5,
		SuccessCount: 4,
		Weight:       0.75,
		LastUpdated:  time.Now().Round(time.Second),
	}
	if err := backend.SaveRouteWeight(ctx, rw); err != nil {
		t.Errorf("SaveRouteWeight failed: %v", err)
	}

	// 4. Test Load
	data, err := backend.Load(ctx)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	patterns := data["task_patterns"].(map[string]TaskPattern)
	if lp, ok := patterns[p.ID]; !ok || lp.SampleCount != p.SampleCount {
		t.Errorf("TaskPattern load mismatch: got %+v, want %+v", lp, p)
	}
	// Check embedding round-trip
	if len(patterns[p.ID].Embedding) != 3 || patterns[p.ID].Embedding[0] != 0.1 {
		t.Errorf("Embedding round-trip failed: got %v", patterns[p.ID].Embedding)
	}

	profiles := data["role_profiles"].(map[string]*RoleProfile)
	if lrp, ok := profiles[rp.RoleID]; !ok || lrp.TotalRuns != rp.TotalRuns {
		t.Errorf("RoleProfile load mismatch")
	}

	matrix := data["routing_matrix"].(map[string]map[string]map[string]map[string]*RouteWeight)
	if lrw := matrix["role_1"]["model_1"]["cap_1"]["skill_1"]; lrw == nil || lrw.Weight != rw.Weight {
		t.Errorf("RouteWeight load mismatch")
	}
}
