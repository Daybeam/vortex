package store

import (
	"context"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestStatePotentialPersistence_SQLite(t *testing.T) {
	dbFile := "test_experience_potential.db"
	defer os.Remove(dbFile)

	db, err := InitDB(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := NewSQLiteExperienceBackend(db)
	ctx := context.Background()

	sp := &StatePotential{
		StateHash:    "test_tool:abc123",
		ToolID:       "test_tool",
		TotalRuns:    10,
		SuccessCount: 8,
		Potential:    0.8,
		LastUpdated:  time.Now(),
	}

	// 1. Save
	if err := backend.SaveStatePotential(ctx, sp); err != nil {
		t.Fatalf("failed to save state potential: %v", err)
	}

	// 2. Load
	data, err := backend.Load(ctx)
	if err != nil {
		t.Fatalf("failed to load experience data: %v", err)
	}

	potentials, ok := data["state_potentials"].(map[string]*StatePotential)
	if !ok {
		t.Fatal("state_potentials not found in loaded data")
	}

	loaded, exists := potentials[sp.StateHash]
	if !exists {
		t.Errorf("expected state potential %s to be loaded", sp.StateHash)
	} else {
		if loaded.TotalRuns != sp.TotalRuns || loaded.SuccessCount != sp.SuccessCount || loaded.Potential != sp.Potential {
			t.Errorf("loaded state potential mismatch: %+v", loaded)
		}
	}
}

func TestExperienceStore_UpdateStatePotential(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "exptest")
	defer os.RemoveAll(tmpDir)

	es, err := NewExperienceStore(tmpDir, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := &StepRecord{
		StepID:        "s1",
		Capability:    "test",
		StatesVisited: []string{"tool1:hash1", "tool2:hash2"},
	}

	// Success trajectory
	es.updateStatePotentialsLocked(context.Background(), rec, true)

	if sp := es.StatePotentials["tool1:hash1"]; sp == nil || sp.Potential != 1.0 {
		t.Errorf("expected potential 1.0 for tool1:hash1, got %+v", sp)
	}

	// Failure trajectory
	es.updateStatePotentialsLocked(context.Background(), rec, false)

	if sp := es.StatePotentials["tool1:hash1"]; sp == nil || sp.Potential != 0.5 {
		t.Errorf("expected potential 0.5 for tool1:hash1, got %+v", sp)
	}
}
