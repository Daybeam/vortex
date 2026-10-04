package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestSOPManager_Governance(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	// Initialize with split layout so Load() looks in workspace/sops/
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": {
			"sops": "workspace/sops/"
		}
	}`), 0644)

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	sopsDir := filepath.Join(tmpDir, "workspace", "sops")
	mgr := NewSOPManager(reg, sopsDir)

	ctx := context.Background()

	// 1. Create a human-authored immutable SOP
	humanSOP := schemas.SOP{
		ID:             "human_sop",
		Version:        "1.0",
		Author:         "human_expert",
		MutableByAgent: false,
		Description:    "Immutable human SOP",
		Steps: map[string]schemas.SOPStep{
			"step1": {ID: "step1", Role: "admin", Task: "Strict task"},
		},
	}

	if err := mgr.SaveSOP(ctx, humanSOP, false, false); err != nil {
		t.Fatalf("failed to save human SOP: %v", err)
	}

	// 2. Verify ListSOPs
	list := mgr.ListSOPs()
	if len(list) != 1 {
		t.Errorf("expected 1 SOP, got %d", len(list))
	}
	if list[0].ID != "human_sop" {
		t.Errorf("expected human_sop, got %s", list[0].ID)
	}

	// 3. Attempt agent overwrite (should fail)
	agentAttempt := humanSOP
	agentAttempt.Description = "Corrupted by agent"
	err = mgr.SaveSOP(ctx, agentAttempt, true, false)
	if err == nil {
		t.Error("expected agent overwrite to fail for immutable human SOP, but it succeeded")
	} else if !strings.Contains(err.Error(), "immutable and human-authored") {
		t.Errorf("unexpected error message: %v", err)
	}

	// 4. Attempt agent proposal (should succeed)
	propID, err := mgr.ProposeSOPPatch(ctx, "human_sop", "Optimization patch", "trace_123", "agent_hermes")
	if err != nil {
		t.Fatalf("failed to propose patch: %v", err)
	}
	if propID == "" {
		t.Error("expected proposal ID, got empty string")
	}

	// 5. Verify proposal file exists
	propPath := filepath.Join(sopsDir, "proposals", propID+".json")
	if _, err := os.Stat(propPath); os.IsNotExist(err) {
		t.Errorf("proposal file not found at %s", propPath)
	}

	// 6. Admin override (should succeed)
	err = mgr.SaveSOP(ctx, agentAttempt, false, true)
	if err != nil {
		t.Fatalf("admin override failed: %v", err)
	}

	updated, _ := mgr.GetSOP("human_sop")
	if updated.Description != "Corrupted by agent" {
		t.Errorf("expected updated description, got %s", updated.Description)
	}
}

// TestSOPManager_SaveSOP_SanitizesPathTraversalID reproduces the path-traversal
// gap found on 2026-09-11 review: SaveSOP built its write path from the
// caller-supplied sop.ID with no sanitization, unlike the equivalent
// tools/subsystems.go update_sop action, which already applies
// filepath.Base(sop.ID). Confirms a malicious ID cannot escape sopsDir, and
// that the resulting file is confined to the intended directory.
func TestSOPManager_SaveSOP_SanitizesPathTraversalID(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": {
			"sops": "workspace/sops/"
		}
	}`), 0644)

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	sopsDir := filepath.Join(tmpDir, "workspace", "sops")
	mgr := NewSOPManager(reg, sopsDir)
	ctx := context.Background()

	maliciousSOP := schemas.SOP{
		ID:          "../../../escaped_sop",
		Version:     "1.0",
		Author:      "human_expert",
		Description: "Attempts to escape sopsDir via a path-traversal ID",
		Steps: map[string]schemas.SOPStep{
			"step1": {ID: "step1", Role: "admin", Task: "noop"},
		},
	}

	if err := mgr.SaveSOP(ctx, maliciousSOP, false, false); err != nil {
		t.Fatalf("SaveSOP failed: %v", err)
	}

	// The file must NOT have escaped upward out of tmpDir.
	escapedPath := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(tmpDir))), "escaped_sop.json")
	if _, err := os.Stat(escapedPath); err == nil {
		t.Fatalf("path traversal succeeded: file written outside sopsDir at %s", escapedPath)
	}

	// The file must land inside sopsDir, using only the base name.
	expectedPath := filepath.Join(sopsDir, "escaped_sop.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected sanitized file at %s, got error: %v", expectedPath, err)
	}
}

// TestSOPManager_AgentCreateNewSOP verifies an agent can create a brand-new
// SOP when no ID conflict exists. This is the Phase 1 automation entry point:
// agent explores tools, generates a SOP blueprint, and persists it via SaveSOP.
func TestSOPManager_AgentCreateNewSOP(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	sopsDir := filepath.Join(tmpDir, "workspace", "sops")
	mgr := NewSOPManager(reg, sopsDir)
	ctx := context.Background()

	newSOP := schemas.SOP{
		ID:             "agent_generated_workflow",
		Version:        "1.0",
		Author:         "agent_hermes",
		MutableByAgent: true,
		Description:    "Auto-generated by agent exploration",
		Steps: map[string]schemas.SOPStep{
			"fetch": {ID: "fetch", Role: "engineer", Task: "fetch data"},
			"save":  {ID: "save", Role: "engineer", Task: "save result", DependsOn: []string{"fetch"}},
		},
	}

	if err := mgr.SaveSOP(ctx, newSOP, true, false); err != nil {
		t.Fatalf("agent creation should succeed for new SOP: %v", err)
	}

	// Verify it's persisted and hot-reloaded
	loaded, err := mgr.GetSOP("agent_generated_workflow")
	if err != nil {
		t.Fatalf("SOP not found after save: %v", err)
	}
	if loaded.Author != "agent_hermes" {
		t.Errorf("expected author agent_hermes, got %s", loaded.Author)
	}
	if !loaded.MutableByAgent {
		t.Error("expected MutableByAgent=true")
	}
	if len(loaded.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(loaded.Steps))
	}

	// Verify file exists on disk
	filePath := filepath.Join(sopsDir, "agent_generated_workflow.json")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Error("SOP file not written to disk")
	}
}

// TestSOPManager_AgentOverwriteMutableSOP verifies an agent can overwrite
// an SOP where MutableByAgent=true, even if Author is human_expert.
func TestSOPManager_AgentOverwriteMutableSOP(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	sopsDir := filepath.Join(tmpDir, "workspace", "sops")
	mgr := NewSOPManager(reg, sopsDir)
	ctx := context.Background()

	mutableSOP := schemas.SOP{
		ID:             "mutable_sop",
		Version:        "1.0",
		Author:         "human_expert",
		MutableByAgent: true,
		Description:    "Agent can modify this",
		Steps: map[string]schemas.SOPStep{
			"step1": {ID: "step1", Role: "engineer", Task: "original task"},
		},
	}
	if err := mgr.SaveSOP(ctx, mutableSOP, false, false); err != nil {
		t.Fatalf("failed to save initial SOP: %v", err)
	}

	// Agent updates it
	updated := mutableSOP
	updated.Version = "2.0"
	updated.Steps = map[string]schemas.SOPStep{
		"step1": {ID: "step1", Role: "engineer", Task: "updated task"},
	}
	if err := mgr.SaveSOP(ctx, updated, true, false); err != nil {
		t.Fatalf("agent should be able to overwrite mutable SOP: %v", err)
	}

	loaded, _ := mgr.GetSOP("mutable_sop")
	if loaded.Version != "2.0" {
		t.Errorf("expected version 2.0, got %s", loaded.Version)
	}
}

// TestSOPManager_SaveSOP_EmptyID verifies that a SOP with an empty ID is rejected.
func TestSOPManager_SaveSOP_EmptyID(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, _ := config.NewRegistry(configPath)
	mgr := NewSOPManager(reg, filepath.Join(tmpDir, "workspace", "sops"))

	err := mgr.SaveSOP(context.Background(), schemas.SOP{
		ID:    "",
		Steps: map[string]schemas.SOPStep{"s1": {ID: "s1"}},
	}, false, false)
	if err == nil {
		t.Fatal("expected error for empty ID, got nil")
	}
}

// TestSOPManager_SaveSOP_EmptySteps verifies that a SOP with zero steps is rejected.
func TestSOPManager_SaveSOP_EmptySteps(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, _ := config.NewRegistry(configPath)
	mgr := NewSOPManager(reg, filepath.Join(tmpDir, "workspace", "sops"))

	err := mgr.SaveSOP(context.Background(), schemas.SOP{
		ID:    "empty_steps_sop",
		Steps: map[string]schemas.SOPStep{},
	}, false, false)
	if err == nil {
		t.Fatal("expected error for empty steps, got nil")
	}
}

// TestSOPManager_ValidateDAG_CycleDetectsDirectLoop verifies that validateSOPDAG
// catches a direct self-referencing cycle (step A depends on A).
func TestSOPManager_ValidateDAG_CycleDetectsDirectLoop(t *testing.T) {
	steps := map[string]schemas.SOPStep{
		"A": {ID: "A", DependsOn: []string{"A"}},
	}
	err := validateSOPDAG(steps)
	if err == nil {
		t.Fatal("expected cycle detection error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got: %v", err)
	}
}

// TestSOPManager_ValidateDAG_CycleDetectsIndirectLoop verifies that validateSOPDAG
// catches an indirect cycle (A→B→C→A).
func TestSOPManager_ValidateDAG_CycleDetectsIndirectLoop(t *testing.T) {
	steps := map[string]schemas.SOPStep{
		"A": {ID: "A", DependsOn: []string{"B"}},
		"B": {ID: "B", DependsOn: []string{"C"}},
		"C": {ID: "C", DependsOn: []string{"A"}},
	}
	err := validateSOPDAG(steps)
	if err == nil {
		t.Fatal("expected cycle detection error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got: %v", err)
	}
}

// TestSOPManager_ValidateDAG_UnknownDep verifies that validateSOPDAG rejects
// a step that references a non-existent dependency.
func TestSOPManager_ValidateDAG_UnknownDep(t *testing.T) {
	steps := map[string]schemas.SOPStep{
		"A": {ID: "A", DependsOn: []string{"nonexistent"}},
	}
	err := validateSOPDAG(steps)
	if err == nil {
		t.Fatal("expected unknown dep error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("expected 'unknown step' error, got: %v", err)
	}
}

// TestSOPManager_ValidateDAG_ValidLinear verifies that a valid linear DAG passes.
func TestSOPManager_ValidateDAG_ValidLinear(t *testing.T) {
	steps := map[string]schemas.SOPStep{
		"A": {ID: "A"},
		"B": {ID: "B", DependsOn: []string{"A"}},
		"C": {ID: "C", DependsOn: []string{"B"}},
	}
	if err := validateSOPDAG(steps); err != nil {
		t.Fatalf("valid linear DAG should pass, got: %v", err)
	}
}

// TestSOPManager_ValidateDAG_ValidDiamond verifies that a diamond DAG passes.
func TestSOPManager_ValidateDAG_ValidDiamond(t *testing.T) {
	steps := map[string]schemas.SOPStep{
		"A": {ID: "A"},
		"B": {ID: "B", DependsOn: []string{"A"}},
		"C": {ID: "C", DependsOn: []string{"A"}},
		"D": {ID: "D", DependsOn: []string{"B", "C"}},
	}
	if err := validateSOPDAG(steps); err != nil {
		t.Fatalf("valid diamond DAG should pass, got: %v", err)
	}
}

// TestSOPManager_GetSOP_NotFound verifies GetSOP returns an error for a missing ID.
func TestSOPManager_GetSOP_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, _ := config.NewRegistry(configPath)
	mgr := NewSOPManager(reg, filepath.Join(tmpDir, "workspace", "sops"))

	_, err := mgr.GetSOP("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent SOP, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

// TestSOPManager_ProposeSOPPatch_NonExistentSOP verifies that proposing a patch
// for a non-existent SOP is rejected.
func TestSOPManager_ProposeSOPPatch_NonExistentSOP(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, _ := config.NewRegistry(configPath)
	mgr := NewSOPManager(reg, filepath.Join(tmpDir, "workspace", "sops"))

	_, err := mgr.ProposeSOPPatch(context.Background(), "nonexistent", "patch", "trace", "agent")
	if err == nil {
		t.Fatal("expected error for non-existent SOP, got nil")
	}
	if !strings.Contains(err.Error(), "non-existent") {
		t.Errorf("expected 'non-existent' error, got: %v", err)
	}
}

// TestSOPManager_ValidateDAG_SaveSOPRejectsCycle verifies that SaveSOP
// rejects an SOP containing a cyclic DAG (integration test: validateSOPDAG
// is called inside SaveSOP).
func TestSOPManager_ValidateDAG_SaveSOPRejectsCycle(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, _ := config.NewRegistry(configPath)
	mgr := NewSOPManager(reg, filepath.Join(tmpDir, "workspace", "sops"))

	cyclicSOP := schemas.SOP{
		ID: "cyclic_sop",
		Steps: map[string]schemas.SOPStep{
			"A": {ID: "A", DependsOn: []string{"B"}},
			"B": {ID: "B", DependsOn: []string{"A"}},
		},
	}
	err := mgr.SaveSOP(context.Background(), cyclicSOP, false, false)
	if err == nil {
		t.Fatal("expected SaveSOP to reject cyclic DAG, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got: %v", err)
	}
}

// TestSOPManager_ValidateDAG_SaveSOPRejectsUnknownDep verifies that SaveSOP
// rejects an SOP that references a non-existent step in DependsOn.
func TestSOPManager_ValidateDAG_SaveSOPRejectsUnknownDep(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"sops": [],
		"_split_layout": { "sops": "workspace/sops/" }
	}`), 0644)

	reg, _ := config.NewRegistry(configPath)
	mgr := NewSOPManager(reg, filepath.Join(tmpDir, "workspace", "sops"))

	badDepSOP := schemas.SOP{
		ID: "bad_dep_sop",
		Steps: map[string]schemas.SOPStep{
			"A": {ID: "A", DependsOn: []string{"ghost"}},
		},
	}
	err := mgr.SaveSOP(context.Background(), badDepSOP, false, false)
	if err == nil {
		t.Fatal("expected SaveSOP to reject unknown dep, got nil")
	}
	if !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("expected 'unknown step' error, got: %v", err)
	}
}

// TestSOP_UnmarshalJSON_LegacyTriggers verifies backward compatibility: a SOP
// with a single trigger object (not array) is converted to []SOPTrigger.
func TestSOP_UnmarshalJSON_LegacyTriggers(t *testing.T) {
	data := `{"id":"legacy","version":"1.0","author":"human_expert",
	"triggers":{"keywords":["deploy","release"]},
	"steps":{"s1":{"id":"s1","role":"engineer","task":"do"}}}`

	var sop schemas.SOP
	if err := json.Unmarshal([]byte(data), &sop); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(sop.Triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(sop.Triggers))
	}
	if len(sop.Triggers[0].Keywords) != 2 || sop.Triggers[0].Keywords[0] != "deploy" {
		t.Errorf("unexpected trigger keywords: %v", sop.Triggers[0].Keywords)
	}
}

// TestSOP_UnmarshalJSON_LegacyStepsSlice verifies backward compatibility: a SOP
// with a steps array (not object) is converted to map[string]SOPStep.
func TestSOP_UnmarshalJSON_LegacyStepsSlice(t *testing.T) {
	data := `{"id":"legacy","version":"1.0","author":"human_expert",
	"triggers":[{"keywords":["test"]}],
	"steps":[{"id":"s1","role":"engineer","task":"first"},
	          {"id":"s2","role":"engineer","task":"second","depends_on":["s1"]}]}`

	var sop schemas.SOP
	if err := json.Unmarshal([]byte(data), &sop); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(sop.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(sop.Steps))
	}
	if sop.Steps["s1"].Task != "first" {
		t.Errorf("expected step s1 task 'first', got %q", sop.Steps["s1"].Task)
	}
	if sop.Steps["s2"].Task != "second" {
		t.Errorf("expected step s2 task 'second', got %q", sop.Steps["s2"].Task)
	}
	if len(sop.Steps["s2"].DependsOn) != 1 || sop.Steps["s2"].DependsOn[0] != "s1" {
		t.Errorf("expected s2 depends_on s1, got %v", sop.Steps["s2"].DependsOn)
	}
}
