package core

import (
	"context"
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
