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

// ─── Integration Tests for Effect Boundary Defense + Tier-Gated Workspace Scope ───
//
// These tests verify the security invariants through the real production code
// paths (not mocked). Each test exercises the integration point that the design
// docs explicitly request:
//
//   - A1 retry: design doc §6 "集成测试: 重试场景下的 A1 防御验证"
//   - Workspace scope: design doc §8 "集成测试：Public tier LLM 注入 → 被拒绝"
//   - Egress redaction: design doc §8 "集成测试：tool result 跨租户 OwnerID → 脱敏"

// ─── Test 1: A1 Retry Integration ──────────────────────────────────────────────

// TestIntegration_A1_Retry_DuplicateSkipped_OneEffectRecorded verifies that
// when the Spawner's turnLoop processes a duplicate tool call, A1 idempotency
// blocks the duplicate and only one effect is recorded by the EffectTracker.
//
// This tests the real Spawner.recordEffect → GetOrCreateEffectTracker →
// EffectTracker.RecordEffect chain, and the real TurnIdempotencyContext.
// CheckAndMark logic — the same pattern used in spawner.go lines 670, 1336,
// and 1381.
//
// Design ref: docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §6
// "集成测试: 重试场景下的 A1 防御验证"
func TestIntegration_A1_Retry_DuplicateSkipped_OneEffectRecorded(t *testing.T) {
	tmpDir := t.TempDir()
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})

	spawner := &Spawner{
		outputBase: tmpDir,
		logger:     logger,
	}

	taskID := "task-a1-integration"
	stepID := "step-1"
	toolName := "write_file"
	args := map[string]any{"path": "output.txt", "content": "hello"}

	// Simulate the turnLoop's A1 + recordEffect pattern.
	// (Same pattern as spawner.go lines 670, 1336, 1381)
	turnCtx := NewTurnIdempotencyContext()

	// ── Turn 1, Call 1: A1 allows it → recordEffect records it. ──
	opKey := toolCallKey(toolName, args)
	if turnCtx.CheckAndMark(opKey) {
		t.Fatal("first call should not be skipped by A1")
	}
	// recordEffect is called (matching spawner.go line 1381).
	spawner.recordEffect(taskID, stepID, toolName, args, "written", "success")

	// ── Turn 1, Call 2 (duplicate): A1 skips it → recordEffect NOT called. ──
	opKey2 := toolCallKey(toolName, args)
	if !turnCtx.CheckAndMark(opKey2) {
		t.Fatal("second duplicate call should be skipped by A1")
	}
	// recordEffect is NOT called — matching the turnLoop's `continue` at line 1341.

	// ── Verify: EffectTracker has exactly 1 record (not 2). ──
	tracker := spawner.GetOrCreateEffectTracker(taskID)
	records := tracker.GetRecords()
	if len(records) != 1 {
		t.Errorf("expected 1 effect record (A1 skipped duplicate), got %d", len(records))
	}
	if len(records) > 0 && records[0].ToolName != toolName {
		t.Errorf("record tool = %q, want %q", records[0].ToolName, toolName)
	}
}

// TestIntegration_A1_Retry_DifferentArgs_BothRecorded verifies that A1 only
// blocks identical calls (same tool + same args). Different args should both
// be recorded.
func TestIntegration_A1_Retry_DifferentArgs_BothRecorded(t *testing.T) {
	tmpDir := t.TempDir()
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})

	spawner := &Spawner{
		outputBase: tmpDir,
		logger:     logger,
	}

	taskID := "task-a1-diff-args"
	stepID := "step-1"
	turnCtx := NewTurnIdempotencyContext()

	// Call 1: write_file with content "hello"
	args1 := map[string]any{"path": "a.txt", "content": "hello"}
	if turnCtx.CheckAndMark(toolCallKey("write_file", args1)) {
		t.Fatal("first call should not be skipped")
	}
	spawner.recordEffect(taskID, stepID, "write_file", args1, "written", "success")

	// Call 2: write_file with content "world" — different args, should NOT be skipped
	args2 := map[string]any{"path": "a.txt", "content": "world"}
	if turnCtx.CheckAndMark(toolCallKey("write_file", args2)) {
		t.Fatal("call with different args should not be skipped")
	}
	spawner.recordEffect(taskID, stepID, "write_file", args2, "written", "success")

	tracker := spawner.GetOrCreateEffectTracker(taskID)
	records := tracker.GetRecords()
	if len(records) != 2 {
		t.Errorf("expected 2 effect records (different args), got %d", len(records))
	}
}

// ─── Test 2B: Workspace Scope — validateWorkspaceScope through SubmitWithSessionIR ──

// TestIntegration_WorkspaceScope_BlockedByValidateWorkspaceScope verifies that
// the Complete Mediation (I3) defense-in-depth check in SubmitWithSessionIR
// blocks workspace roots outside the allowed scope for an owner.
//
// This tests the real DirectedEngine.SubmitWithSessionIR → validateWorkspaceScope
// code path. When a direct API handler bypasses the tools layer's
// ResolveWorkspaceRoot, the scheduler re-checks the workspace scope.
//
// Design ref: docs/plan/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §3.5, §8
// "集成测试：Public tier LLM 注入 workspace_root → 被拒绝"
func TestIntegration_WorkspaceScope_BlockedByValidateWorkspaceScope(t *testing.T) {
	engine, reg := newDelegationEngine(t)

	// Enable workspace policy with an allowed root.
	allowedRoot := t.TempDir()
	reg.Mu.Lock()
	reg.System.WorkspacePolicy = config.WorkspacePolicyConfig{
		Enabled:      true,
		AllowedRoots: []string{allowedRoot},
	}
	reg.Mu.Unlock()

	ownerID := "tenant-public"

	// ── Case 1: workspaceRoot outside allowed roots → should fail. ──
	maliciousRoot := filepath.Join(os.TempDir(), "malicious-path-that-should-be-blocked")
	_, err := engine.SubmitWithSessionIR(
		[]schemas.StepInput{{ID: "step_1", RoleID: "worker", Task: "work"}},
		nil, nil, nil, "", "", maliciousRoot, 0, 0, ownerID,
	)
	if err == nil {
		t.Fatal("expected error for workspace outside allowed roots")
	}
	if !strings.Contains(err.Error(), "workspace scope violation") {
		t.Errorf("expected 'workspace scope violation' error, got: %v", err)
	}

	// ── Case 2: workspaceRoot inside allowed roots → should succeed. ──
	safeRoot := filepath.Join(allowedRoot, "session1")
	if err := os.MkdirAll(safeRoot, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	taskID, err := engine.SubmitWithSessionIR(
		[]schemas.StepInput{{ID: "step_1", RoleID: "worker", Task: "work"}},
		nil, nil, nil, "", "", safeRoot, 0, 0, ownerID,
	)
	if err != nil {
		t.Fatalf("expected success for workspace within allowed roots, got: %v", err)
	}
	if taskID == "" {
		t.Error("expected non-empty taskID")
	}

	// ── Case 3: local mode (empty ownerID) → no check, any path succeeds. ──
	_, err = engine.SubmitWithSessionIR(
		[]schemas.StepInput{{ID: "step_1", RoleID: "worker", Task: "work"}},
		nil, nil, nil, "", "", "/any/arbitrary/path", 0, 0, "",
	)
	if err != nil {
		t.Fatalf("expected success for local mode (empty owner), got: %v", err)
	}

	// ── Case 4: policy disabled → no check, any path succeeds. ──
	reg.Mu.Lock()
	reg.System.WorkspacePolicy.Enabled = false
	reg.Mu.Unlock()
	_, err = engine.SubmitWithSessionIR(
		[]schemas.StepInput{{ID: "step_1", RoleID: "worker", Task: "work"}},
		nil, nil, nil, "", "", "/another/arbitrary/path", 0, 0, ownerID,
	)
	if err != nil {
		t.Fatalf("expected success when policy disabled, got: %v", err)
	}
}

// ─── Test 3: Egress Redaction through handleCoreToolCall ──────────────────────

// TestIntegration_EgressRedaction_SecretsStrippedFromToolResult verifies that
// when a core tool (read_file) returns content containing secrets, the
// RedactEgress function in handleCoreToolCall strips them before the result
// enters the LLM context.
//
// This tests the real Spawner.handleCoreToolCall → HandleCoreTool →
// RedactEgress code path (spawner_dispatch.go line 119).
//
// Design ref: docs/plan/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §4.2, §8
// "集成测试：tool result 中包含其他租户 OwnerID → 被脱敏"
func TestIntegration_EgressRedaction_SecretsStrippedFromToolResult(t *testing.T) {
	tmpDir := t.TempDir()
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	taskID := "task-egress-integration"

	// Create a file with secrets in the task directory.
	taskDir := filepath.Join(tmpDir, taskID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	// Content with various secret patterns + cross-tenant reference.
	secretContent := `Configuration file:
API_KEY=sk-proj1234567890abcdefghij
AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
AUTH_HEADER=Bearer abcdefghijklmnopqrstuvwxyz1234567890
JWT_TOKEN=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c
CONTACT_EMAIL=admin@company.com
PHONE=13912345678
CROSS_TENANT_REF=owner:other-tenant-456
`
	secretFile := filepath.Join(taskDir, "config.txt")
	if err := os.WriteFile(secretFile, []byte(secretContent), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Create a minimal Spawner.
	spawner := &Spawner{
		outputBase: tmpDir,
		logger:     logger,
	}

	// Create a SafePathValidator for the task directory.
	validator := NewSafePathValidator(taskDir, "", nil)

	// Call handleCoreToolCall with read_file — this goes through:
	// HandleCoreTool (reads file) → RedactEgress (strips secrets) → return
	res, status, interaction := spawner.handleCoreToolCall(
		context.Background(),
		schemas.ToolCall{
			Name:      "read_file",
			Arguments: map[string]any{"path": "config.txt"},
		},
		taskID, "step-1", validator,
	)

	if status != "success" {
		t.Fatalf("expected status=success, got %s: %s", status, res)
	}

	// ── Verify secrets are redacted from the result. ──
	secretChecks := []struct {
		name    string
		pattern string
	}{
		{"OpenAI key", "sk-proj1234567890abcdefghij"},
		{"AWS key", "AKIAIOSFODNN7EXAMPLE"},
		{"Bearer token", "Bearer abcdefghijklmnopqrstuvwxyz1234567890"},
		{"JWT", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0"},
		{"Email", "admin@company.com"},
		{"Phone", "13912345678"},
	}
	for _, sc := range secretChecks {
		if strings.Contains(res, sc.pattern) {
			t.Errorf("%s was NOT redacted from tool result", sc.name)
		}
	}

	// ── Verify redaction markers are present. ──
	if !strings.Contains(res, "[REDACTED]") {
		t.Error("expected [REDACTED] markers in redacted output")
	}

	// ── Verify the interaction record is also redacted. ──
	interactionResult, _ := interaction.Result.(string)
	for _, sc := range secretChecks {
		if strings.Contains(interactionResult, sc.pattern) {
			t.Errorf("%s leaked into interaction.Result", sc.name)
		}
	}

	// ── Verify cross-tenant detection via RedactEgressWithOwner. ──
	redacted := RedactEgressWithOwner(res, "tenant-public")
	if strings.Contains(redacted, "other-tenant-456") {
		t.Error("cross-tenant OwnerID 'other-tenant-456' was NOT redacted")
	}
	if !strings.Contains(redacted, "[OTHER_TENANT:REDACTED]") {
		t.Error("expected [OTHER_TENANT:REDACTED] marker for cross-tenant detection")
	}
}

// TestIntegration_EgressRedaction_NoSecrets_Passthrough verifies that
// content without secrets passes through RedactEgress unchanged.
func TestIntegration_EgressRedaction_NoSecrets_Passthrough(t *testing.T) {
	tmpDir := t.TempDir()
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	taskID := "task-egress-clean"

	taskDir := filepath.Join(tmpDir, taskID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	cleanContent := "This is a normal file with no secrets.\nLine 2: just text.\n"
	cleanFile := filepath.Join(taskDir, "clean.txt")
	if err := os.WriteFile(cleanFile, []byte(cleanContent), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	spawner := &Spawner{
		outputBase: tmpDir,
		logger:     logger,
	}
	validator := NewSafePathValidator(taskDir, "", nil)

	res, status, _ := spawner.handleCoreToolCall(
		context.Background(),
		schemas.ToolCall{
			Name:      "read_file",
			Arguments: map[string]any{"path": "clean.txt"},
		},
		taskID, "step-1", validator,
	)

	if status != "success" {
		t.Fatalf("expected status=success, got %s: %s", status, res)
	}

	// Content should pass through unchanged (no redaction markers).
	if strings.Contains(res, "[REDACTED]") {
		t.Error("clean content was incorrectly redacted")
	}
	if !strings.Contains(res, "This is a normal file with no secrets.") {
		t.Error("clean content was modified by redaction")
	}
}
