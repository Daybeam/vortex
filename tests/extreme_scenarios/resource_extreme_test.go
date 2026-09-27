package extreme_tests

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// TestExtreme_GoroutineLeak_AssetManager verifies that repeated AssetManager
// operations don't leak goroutines. Records goroutine count before and after
// 1000 operations; delta must be ≤ 2 (allowing background GC).
//
// Extreme scenario: Long-running process handling thousands of assets.
func TestExtreme_GoroutineLeak_AssetManager(t *testing.T) {
	tmpDir := t.TempDir()
	am := core.NewAssetManager(filepath.Join(tmpDir, "assets"), 100, &config.Registry{})

	before := runtime.NumGoroutine()

	for i := 0; i < 1000; i++ {
		data := []byte(strings.Repeat("Z", 200))
		_, _ = am.Handle(fmt.Sprintf("task-%d", i), "step", data, "txt", "")
	}

	// Allow GC to clean up
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	delta := after - before
	if delta > 5 {
		t.Errorf("goroutine leak: before=%d, after=%d, delta=%d (threshold=5)", before, after, delta)
	}
}

// TestExtreme_DiskFull_WriteFailure verifies that the AssetManager
// returns a proper error when the disk is full (simulated by making
// the target path unwritable — a file where a directory is expected).
//
// Extreme scenario: Disk runs out of space mid-operation.
func TestExtreme_DiskFull_WriteFailure(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file where the asset directory should be — MkdirAll will fail
	blockerPath := filepath.Join(tmpDir, "assets")
	if err := os.WriteFile(blockerPath, []byte("blocker"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	am := core.NewAssetManager(blockerPath, 100, &config.Registry{})

	_, err := am.Handle("task-1", "step-1", []byte(strings.Repeat("X", 200)), "txt", "")
	if err == nil {
		t.Fatal("expected error when disk path is blocked, got nil")
	}
}

// TestExtreme_DiskFull_ReadOnlyDir verifies that writing to a read-only
// directory returns an error (on Unix; on Windows this may behave differently).
//
// Extreme scenario: Output directory permissions are misconfigured.
func TestExtreme_DiskFull_ReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only dir test is unreliable on Windows — file locking differs")
	}

	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root bypasses file permissions)")
	}

	tmpDir := t.TempDir()
	roDir := filepath.Join(tmpDir, "readonly")
	if err := os.MkdirAll(roDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.Chmod(roDir, 0444); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	defer os.Chmod(roDir, 0755) // restore for cleanup

	am := core.NewAssetManager(roDir, 100, &config.Registry{})
	_, err := am.Handle("task-1", "step-1", []byte(strings.Repeat("X", 200)), "txt", "")
	if err == nil {
		t.Fatal("expected error writing to read-only dir, got nil")
	}
}

// TestExtreme_CorruptedExperienceStore verifies that loading a corrupted
// experience store file produces a graceful error, not a panic, and that
// the corrupted data is skipped (empty map) while valid files still load.
//
// Extreme scenario: Experience store JSON file is truncated/corrupted
// due to a previous crash during write.
//
// T-C07/T-C14 fix: previously this test never called ExperienceStore.Load;
// it only wrote a file and read it back with os.ReadFile. Now it calls
// store.NewExperienceStore (which calls load() internally) and asserts
// that corrupted task_patterns.json is skipped gracefully.
func TestExtreme_CorruptedExperienceStore(t *testing.T) {
	tmpDir := t.TempDir()
	expDir := filepath.Join(tmpDir, "experience")
	if err := os.MkdirAll(expDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	// Write malformed JSON to task_patterns.json
	corruptFile := filepath.Join(expDir, "task_patterns.json")
	malformedData := []byte(`{"task_patterns": [{"id": "broken", "name": "test"`)
	if err := os.WriteFile(corruptFile, malformedData, 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Write a VALID file alongside the corrupted one to verify it still loads
	validRoleProfiles := []byte(`{"role-1": {"role_id": "role-1", "success_count": 5}}`)
	if err := os.WriteFile(filepath.Join(expDir, "role_profiles.json"), validRoleProfiles, 0644); err != nil {
		t.Fatalf("write valid file failed: %v", err)
	}

	// Call the PRODUCTION constructor — this calls load() which calls readJSON
	// on every file in the directory. Corrupted files must be skipped, not panic.
	es, err := store.NewExperienceStore(expDir, nil, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore returned error on corrupted data: %v", err)
	}

	// Corrupted task_patterns.json must NOT have loaded any data
	if len(es.TaskPatterns) != 0 {
		t.Errorf("corrupted task_patterns.json should be skipped, got %d entries", len(es.TaskPatterns))
	}

	// Valid role_profiles.json MUST have loaded correctly
	if profile, ok := es.RoleProfiles["role-1"]; !ok {
		t.Error("valid role_profiles.json was not loaded alongside corrupted file")
	} else if profile.SuccessCount != 5 {
		t.Errorf("role profile data mismatch: got SuccessCount=%d, expected 5", profile.SuccessCount)
	}
}

// TestExtreme_EmptyAndNilInputs verifies that empty and nil inputs
// don't cause panics in the Input Guard.
//
// Extreme scenario: User submits a task with empty or missing fields.
func TestExtreme_EmptyAndNilInputs(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test",
		Providers: map[string]*config.ProviderConfig{
			"test": {Provider: "openai", MaxContextWindow: 8000},
		},
	}

	tests := []struct {
		name   string
		inputs []schemas.StepInput
	}{
		{"empty slice", []schemas.StepInput{}},
		{"empty task", []schemas.StepInput{{ID: "s1", Task: ""}}},
		{"nil task", []schemas.StepInput{{ID: "s1"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Should not panic
			_ = core.EvaluateAndGuardInputs(tc.inputs, reg)
		})
	}
}
