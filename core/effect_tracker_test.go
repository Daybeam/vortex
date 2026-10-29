package core

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ─── Unit Tests: Side-Effect Classification ────────────────────────────────

func TestClassifySideEffect_CoreTools(t *testing.T) {
	tests := []struct {
		toolName string
		expected SideEffectClass
	}{
		{"read_file", SideEffectNone},
		{"list_files", SideEffectNone},
		{"decompress", SideEffectNone},
		{"write_file", SideEffectFile},
		{"execute_code", SideEffectFile},
	}
	for _, tt := range tests {
		got := classifySideEffect(tt.toolName, true)
		if got != tt.expected {
			t.Errorf("classifySideEffect(%q, core=true) = %q, want %q", tt.toolName, got, tt.expected)
		}
	}
}

func TestClassifySideEffect_MCPTools(t *testing.T) {
	tests := []struct {
		toolName string
		expected SideEffectClass
	}{
		{"get_user", SideEffectNone},
		{"list_orders", SideEffectNone},
		{"search_products", SideEffectNone},
		{"query_database", SideEffectNone},
		{"check_status", SideEffectNone},
		{"is_valid", SideEffectNone},
		{"has_permission", SideEffectNone},
		{"create_order", SideEffectAPI},
		{"update_user", SideEffectAPI},
		{"delete_record", SideEffectAPI},
		{"cancel_booking", SideEffectAPI},
		{"set_config", SideEffectAPI},
		{"send_email", SideEffectAPI},
		{"post_webhook", SideEffectAPI},
		{"put_object", SideEffectAPI},
		{"unknown_tool", SideEffectNetwork},
	}
	for _, tt := range tests {
		got := classifySideEffect(tt.toolName, false)
		if got != tt.expected {
			t.Errorf("classifySideEffect(%q, core=false) = %q, want %q", tt.toolName, got, tt.expected)
		}
	}
}

// ─── Unit Tests: RecordEffect ─────────────────────────────────────────────

func TestEffectTracker_RecordEffect(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	args := map[string]any{"path": "/tmp/file.txt", "content": "hello"}

	rec := tracker.RecordEffect("task-1", "step-1", "write_file", args, "written", "success")

	if rec.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want task-1", rec.TaskID)
	}
	if rec.StepID != "step-1" {
		t.Errorf("StepID = %q, want step-1", rec.StepID)
	}
	if rec.ToolName != "write_file" {
		t.Errorf("ToolName = %q, want write_file", rec.ToolName)
	}
	if rec.SideEffect != SideEffectFile {
		t.Errorf("SideEffect = %q, want file", rec.SideEffect)
	}
	if rec.Status != "success" {
		t.Errorf("Status = %q, want success", rec.Status)
	}
	if rec.OpID == "" {
		t.Error("OpID should not be empty")
	}
	if rec.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestEffectTracker_RecordEffect_ReadTool_NoSideEffect(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	rec := tracker.RecordEffect("task-1", "step-1", "read_file", nil, "content", "success")
	if rec.SideEffect != SideEffectNone {
		t.Errorf("SideEffect = %q, want none", rec.SideEffect)
	}
}

func TestEffectTracker_RecordEffect_MCPWrite_APIEffect(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	rec := tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "created", "success")
	if rec.SideEffect != SideEffectAPI {
		t.Errorf("SideEffect = %q, want api", rec.SideEffect)
	}
}

func TestEffectTracker_RecordEffect_ResultTruncation(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	longResult := strings.Repeat("x", 5000)
	rec := tracker.RecordEffect("task-1", "step-1", "write_file", nil, longResult, "success")
	if len(rec.Result) > 4110 { // 4096 + "...[truncated]"
		t.Errorf("Result not truncated: len=%d", len(rec.Result))
	}
	if !strings.Contains(rec.Result, "[truncated]") {
		t.Error("Result should contain truncation marker")
	}
}

// ─── Unit Tests: GetByStep ────────────────────────────────────────────────

func TestEffectTracker_GetByStep(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "write_file", nil, "ok", "success")
	tracker.RecordEffect("task-1", "step-1", "create_order", nil, "ok", "success")
	tracker.RecordEffect("task-1", "step-2", "read_file", nil, "ok", "success")

	step1Records := tracker.GetByStep("step-1")
	if len(step1Records) != 2 {
		t.Errorf("GetByStep(step-1) = %d records, want 2", len(step1Records))
	}

	step2Records := tracker.GetByStep("step-2")
	if len(step2Records) != 1 {
		t.Errorf("GetByStep(step-2) = %d records, want 1", len(step2Records))
	}

	step3Records := tracker.GetByStep("step-3")
	if len(step3Records) != 0 {
		t.Errorf("GetByStep(step-3) = %d records, want 0", len(step3Records))
	}
}

func TestEffectTracker_GetRecords(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "write_file", nil, "ok", "success")
	tracker.RecordEffect("task-1", "step-2", "read_file", nil, "ok", "success")

	all := tracker.GetRecords()
	if len(all) != 2 {
		t.Errorf("GetRecords() = %d, want 2", len(all))
	}
}

// ─── Unit Tests: CompensateStep ───────────────────────────────────────────

func TestCompensateStep_NoSideEffect_Skipped(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "read_file", nil, "content", "success")

	results := tracker.CompensateStep("step-1")
	if len(results) != 1 {
		t.Fatalf("CompensateStep = %d results, want 1", len(results))
	}
	if results[0].Action != "skipped" {
		t.Errorf("Action = %q, want skipped", results[0].Action)
	}
}

func TestCompensateStep_FileEffect_DelegatedToStagedWorkspace(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "write_file", nil, "written", "success")

	results := tracker.CompensateStep("step-1")
	if len(results) != 1 {
		t.Fatalf("CompensateStep = %d results, want 1", len(results))
	}
	if results[0].Action != "file_rollback" {
		t.Errorf("Action = %q, want file_rollback", results[0].Action)
	}
}

func TestCompensateStep_ExternalNotApplied_Skipped(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "created", "success")

	// Register a prober that says the effect was NOT applied.
	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeNotApplied, Detail: "order does not exist"}
	})

	results := tracker.CompensateStep("step-1")
	if len(results) != 1 {
		t.Fatalf("CompensateStep = %d results, want 1", len(results))
	}
	if results[0].Action != "skipped" {
		t.Errorf("Action = %q, want skipped", results[0].Action)
	}
	if !strings.Contains(results[0].Detail, "not applied") {
		t.Errorf("Detail should mention 'not applied', got: %s", results[0].Detail)
	}
}

func TestCompensateStep_ExternalApplied_Compensated(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "created", "success")

	// Register contract: compensatable with a compensator action.
	tracker.RegisterContract("create_order", EffectContract{
		SideEffectClass:   SideEffectAPI,
		Compensatable:     true,
		CompensatorAction: "delete_order",
	})
	// Register prober: effect was applied.
	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeApplied, Detail: "order exists"}
	})
	// Register compensator: deletes the order.
	compensatorCalled := false
	tracker.RegisterCompensator("create_order", func(rec *EffectRecord) (string, error) {
		compensatorCalled = true
		return "order deleted", nil
	})

	results := tracker.CompensateStep("step-1")
	if len(results) != 1 {
		t.Fatalf("CompensateStep = %d results, want 1", len(results))
	}
	if results[0].Action != "compensated" {
		t.Errorf("Action = %q, want compensated", results[0].Action)
	}
	if !compensatorCalled {
		t.Error("Compensator was not called")
	}
}

func TestCompensateStep_ExternalApplied_NotCompensatable_NeedsReview(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "send_email", map[string]any{"to": "user@example.com"}, "sent", "success")

	// Register prober: effect was applied.
	tracker.RegisterProber("send_email", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeApplied, Detail: "email was sent"}
	})
	// No contract registered → not compensatable by default.

	results := tracker.CompensateStep("step-1")
	if len(results) != 1 {
		t.Fatalf("CompensateStep = %d results, want 1", len(results))
	}
	if results[0].Action != "needs_review" {
		t.Errorf("Action = %q, want needs_review", results[0].Action)
	}
	if tracker.NeedsReviewCount() != 1 {
		t.Errorf("NeedsReviewCount = %d, want 1", tracker.NeedsReviewCount())
	}
}

func TestCompensateStep_ExternalApplied_CompensatorNotRegistered_NeedsReview(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "created", "success")

	// Register contract: compensatable, but DON'T register the compensator function.
	tracker.RegisterContract("create_order", EffectContract{
		SideEffectClass:   SideEffectAPI,
		Compensatable:     true,
		CompensatorAction: "delete_order",
	})
	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeApplied}
	})

	results := tracker.CompensateStep("step-1")
	if results[0].Action != "needs_review" {
		t.Errorf("Action = %q, want needs_review", results[0].Action)
	}
	if !strings.Contains(results[0].Detail, "not registered") {
		t.Errorf("Detail should mention 'not registered', got: %s", results[0].Detail)
	}
}

func TestCompensateStep_ExternalApplied_CompensatorError_NeedsReview(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "created", "success")

	tracker.RegisterContract("create_order", EffectContract{
		SideEffectClass:   SideEffectAPI,
		Compensatable:     true,
		CompensatorAction: "delete_order",
	})
	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeApplied}
	})
	tracker.RegisterCompensator("create_order", func(rec *EffectRecord) (string, error) {
		return "", fmt.Errorf("API returned 500")
	})

	results := tracker.CompensateStep("step-1")
	if results[0].Action != "needs_review" {
		t.Errorf("Action = %q, want needs_review", results[0].Action)
	}
	if !strings.Contains(results[0].Detail, "compensator error") {
		t.Errorf("Detail should mention 'compensator error', got: %s", results[0].Detail)
	}
}

func TestCompensateStep_ProbeUnknown_NeedsReview(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "created", "success")

	// No prober registered → ProbeUnknown.

	results := tracker.CompensateStep("step-1")
	if results[0].Action != "needs_review" {
		t.Errorf("Action = %q, want needs_review", results[0].Action)
	}
	if !strings.Contains(results[0].Detail, "unknown") {
		t.Errorf("Detail should mention 'unknown', got: %s", results[0].Detail)
	}
}

func TestCompensateStep_ReverseOrder(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "1"}, "created", "success")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "2"}, "created", "success")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "3"}, "created", "success")

	// Register prober and compensator that record call order.
	var callOrder []string
	var mu sync.Mutex
	tracker.RegisterContract("create_order", EffectContract{
		SideEffectClass:   SideEffectAPI,
		Compensatable:     true,
		CompensatorAction: "delete_order",
	})
	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeApplied}
	})
	tracker.RegisterCompensator("create_order", func(rec *EffectRecord) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		callOrder = append(callOrder, rec.Arguments["id"].(string))
		return "deleted", nil
	})

	results := tracker.CompensateStep("step-1")
	if len(results) != 3 {
		t.Fatalf("CompensateStep = %d results, want 3", len(results))
	}
	// Reverse order: id=3 first, then id=2, then id=1.
	expected := []string{"3", "2", "1"}
	if len(callOrder) != 3 {
		t.Fatalf("callOrder = %v, want 3 calls", callOrder)
	}
	for i, want := range expected {
		if callOrder[i] != want {
			t.Errorf("callOrder[%d] = %q, want %q", i, callOrder[i], want)
		}
	}
}

// ─── Unit Tests: RegisterContract ─────────────────────────────────────────

func TestRegisterContract_OverridesClassification(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	// Override read_file to have a database side effect (unusual but possible).
	tracker.RegisterContract("read_file", EffectContract{
		SideEffectClass:   SideEffectDatabase,
		Compensatable:     true,
		CompensatorAction: "restore_from_backup",
	})

	rec := tracker.RecordEffect("task-1", "step-1", "read_file", nil, "data", "success")
	if rec.SideEffect != SideEffectDatabase {
		t.Errorf("SideEffect = %q, want database (overridden by contract)", rec.SideEffect)
	}
	if !rec.Compensatable {
		t.Error("Compensatable should be true from contract")
	}
	if rec.Compensator != "restore_from_backup" {
		t.Errorf("Compensator = %q, want restore_from_backup", rec.Compensator)
	}
}

// ─── Unit Tests: NeedsReviewCount ─────────────────────────────────────────

func TestNeedsReviewCount(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", nil, "ok", "success")
	tracker.RecordEffect("task-1", "step-1", "send_email", nil, "ok", "success")

	// No probers → both will be ProbeUnknown → both needs_review.
	results := tracker.CompensateStep("step-1")
	reviewCount := 0
	for _, r := range results {
		if r.Action == "needs_review" {
			reviewCount++
		}
	}
	if reviewCount != 2 {
		t.Errorf("needs_review count = %d, want 2", reviewCount)
	}
	if tracker.NeedsReviewCount() != 2 {
		t.Errorf("NeedsReviewCount() = %d, want 2", tracker.NeedsReviewCount())
	}
}

// ─── Flow Tests ───────────────────────────────────────────────────────────

// TestFlow_ToolCall_Recorded_Then_Compensated verifies the full A2/A4/A3 flow:
// tool call → effect recorded → step failure → compensation attempted.
func TestFlow_ToolCall_Recorded_Then_Compensated(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")

	// Simulate a step that makes an API call.
	tracker.RecordEffect("task-1", "step-1", "create_order",
		map[string]any{"order_id": "ORD-001"}, "order created", "success")

	// Register prober: confirms the order exists.
	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeApplied, Detail: "order ORD-001 exists"}
	})
	// Register contract + compensator.
	tracker.RegisterContract("create_order", EffectContract{
		SideEffectClass:   SideEffectAPI,
		Compensatable:     true,
		CompensatorAction: "cancel_order",
	})
	compensated := false
	tracker.RegisterCompensator("create_order", func(rec *EffectRecord) (string, error) {
		compensated = true
		return "order cancelled", nil
	})

	// Simulate step failure → trigger compensation.
	results := tracker.CompensateStep("step-1")
	if len(results) != 1 {
		t.Fatalf("expected 1 compensation result, got %d", len(results))
	}
	if results[0].Action != "compensated" {
		t.Errorf("Action = %q, want compensated", results[0].Action)
	}
	if !compensated {
		t.Error("compensator was not called")
	}
}

// TestFlow_Idempotency_Plus_EffectTracking verifies that A1 idempotency
// and A2 effect tracking work together: a duplicate call is skipped by A1,
// so only one effect is recorded.
func TestFlow_Idempotency_Plus_EffectTracking(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	turnCtx := NewTurnIdempotencyContext()

	args := map[string]any{"path": "/tmp/file.txt", "content": "hello"}
	opKey := toolCallKey("write_file", args)

	// First call: A1 allows it, A2 records the effect.
	if !turnCtx.CheckAndMark(opKey) {
		tracker.RecordEffect("task-1", "step-1", "write_file", args, "written", "success")
	}

	// Second call (duplicate): A1 skips it, A2 does NOT record.
	if turnCtx.CheckAndMark(opKey) {
		// Skipped — no recording.
	} else {
		tracker.RecordEffect("task-1", "step-1", "write_file", args, "written", "success")
	}

	records := tracker.GetRecords()
	if len(records) != 1 {
		t.Errorf("expected 1 effect record (A1 skipped duplicate), got %d", len(records))
	}
}

// TestFlow_DryRun_NotRecorded verifies that a dry-run preview (A5) does not
// record an effect, since the operation was not actually executed.
func TestFlow_DryRun_NotRecorded(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")

	// Simulate A5 dry-run: the tool returns a preview, not a real result.
	// The caller should NOT call RecordEffect for a dry-run.
	dryRunPreview := dryRunResult("write_file", "would write 100 bytes")

	// Verify the result looks like a dry-run preview.
	if !strings.Contains(dryRunPreview, "DRY-RUN") {
		t.Error("dry-run result should contain DRY-RUN marker")
	}

	// No effect should be recorded for a dry-run.
	records := tracker.GetRecords()
	if len(records) != 0 {
		t.Errorf("expected 0 effect records for dry-run, got %d", len(records))
	}
}

// TestFlow_MultipleEffects_ReverseCompensation verifies that when a step
// produces multiple external effects, compensation happens in reverse order
// (last effect compensated first).
func TestFlow_MultipleEffects_ReverseCompensation(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")

	// Simulate a step that: creates an order, sends a confirmation email, charges payment.
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "ORD-1"}, "created", "success")
	tracker.RecordEffect("task-1", "step-1", "send_email", map[string]any{"to": "user@example.com"}, "sent", "success")
	tracker.RecordEffect("task-1", "step-1", "charge_payment", map[string]any{"amount": 100}, "charged", "success")

	// Register contracts: order and payment are compensatable, email is not.
	tracker.RegisterContract("create_order", EffectContract{
		SideEffectClass: SideEffectAPI, Compensatable: true, CompensatorAction: "cancel_order",
	})
	tracker.RegisterContract("charge_payment", EffectContract{
		SideEffectClass: SideEffectAPI, Compensatable: true, CompensatorAction: "refund_payment",
	})

	// Register probers: all effects were applied.
	for _, tool := range []string{"create_order", "send_email", "charge_payment"} {
		toolName := tool
		tracker.RegisterProber(toolName, func(rec *EffectRecord) ProbeResult {
			return ProbeResult{State: ProbeApplied}
		})
	}

	// Register compensators for order and payment.
	var compSequence []string
	var mu sync.Mutex
	tracker.RegisterCompensator("create_order", func(rec *EffectRecord) (string, error) {
		mu.Lock()
		compSequence = append(compSequence, "cancel_order")
		mu.Unlock()
		return "cancelled", nil
	})
	tracker.RegisterCompensator("charge_payment", func(rec *EffectRecord) (string, error) {
		mu.Lock()
		compSequence = append(compSequence, "refund_payment")
		mu.Unlock()
		return "refunded", nil
	})

	results := tracker.CompensateStep("step-1")
	if len(results) != 3 {
		t.Fatalf("expected 3 compensation results, got %d", len(results))
	}

	// Reverse order: charge_payment first, then send_email, then create_order.
	// charge_payment → compensated
	// send_email → needs_review (not compensatable)
	// create_order → compensated
	if results[0].ToolName != "charge_payment" {
		t.Errorf("results[0].ToolName = %q, want charge_payment", results[0].ToolName)
	}
	if results[0].Action != "compensated" {
		t.Errorf("results[0].Action = %q, want compensated", results[0].Action)
	}
	if results[1].ToolName != "send_email" {
		t.Errorf("results[1].ToolName = %q, want send_email", results[1].ToolName)
	}
	if results[1].Action != "needs_review" {
		t.Errorf("results[1].Action = %q, want needs_review", results[1].Action)
	}
	if results[2].ToolName != "create_order" {
		t.Errorf("results[2].ToolName = %q, want create_order", results[2].ToolName)
	}
	if results[2].Action != "compensated" {
		t.Errorf("results[2].Action = %q, want compensated", results[2].Action)
	}

	// Compensators called in reverse order: refund first, then cancel.
	if len(compSequence) != 2 {
		t.Fatalf("compSequence = %v, want 2 calls", compSequence)
	}
	if compSequence[0] != "refund_payment" {
		t.Errorf("compSequence[0] = %q, want refund_payment", compSequence[0])
	}
	if compSequence[1] != "cancel_order" {
		t.Errorf("compSequence[1] = %q, want cancel_order", compSequence[1])
	}
}

// TestFlow_MixedFileAndExternalEffects verifies that a step with both file
// and external effects handles them correctly: file effects are delegated,
// external effects are probed + compensated.
func TestFlow_MixedFileAndExternalEffects(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")

	// Step writes a file AND calls an API.
	tracker.RecordEffect("task-1", "step-1", "write_file", map[string]any{"path": "out.json"}, "written", "success")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "created", "success")

	tracker.RegisterContract("create_order", EffectContract{
		SideEffectClass: SideEffectAPI, Compensatable: true, CompensatorAction: "delete_order",
	})
	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeApplied}
	})
	tracker.RegisterCompensator("create_order", func(rec *EffectRecord) (string, error) {
		return "deleted", nil
	})

	results := tracker.CompensateStep("step-1")
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Reverse order: create_order first, then write_file.
	if results[0].ToolName != "create_order" {
		t.Errorf("results[0].ToolName = %q, want create_order", results[0].ToolName)
	}
	if results[0].Action != "compensated" {
		t.Errorf("results[0].Action = %q, want compensated", results[0].Action)
	}
	if results[1].ToolName != "write_file" {
		t.Errorf("results[1].ToolName = %q, want write_file", results[1].ToolName)
	}
	if results[1].Action != "file_rollback" {
		t.Errorf("results[1].Action = %q, want file_rollback", results[1].Action)
	}
}

// TestFlow_EmptyStep_NoCompensation verifies that compensating a step with
// no effects returns empty results.
func TestFlow_EmptyStep_NoCompensation(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	results := tracker.CompensateStep("nonexistent-step")
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty step, got %d", len(results))
	}
}

// TestFlow_ProbeNotApplied_SafeSkip verifies that when a prober confirms
// the effect was NOT applied (e.g., the API call failed before reaching
// the server), no compensation is needed.
func TestFlow_ProbeNotApplied_SafeSkip(t *testing.T) {
	tracker := NewEffectTracker("/tmp/test")
	tracker.RecordEffect("task-1", "step-1", "create_order", map[string]any{"id": "123"}, "timeout", "error")

	tracker.RegisterProber("create_order", func(rec *EffectRecord) ProbeResult {
		return ProbeResult{State: ProbeNotApplied, Detail: "order does not exist in system"}
	})

	results := tracker.CompensateStep("step-1")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Action != "skipped" {
		t.Errorf("Action = %q, want skipped (effect not applied)", results[0].Action)
	}
	if tracker.NeedsReviewCount() != 0 {
		t.Errorf("NeedsReviewCount = %d, want 0 (effect was not applied)", tracker.NeedsReviewCount())
	}
}

// TestClearEffectTracker_Regression is a regression test for the effectTrackers
// memory leak (audit HIGH 2026-10-28). The effectTrackers sync.Map had
// LoadOrStore per taskID but no Delete, causing unbounded memory growth.
// This test verifies that ClearEffectTracker properly removes the entry.
func TestClearEffectTracker_Regression(t *testing.T) {
	s := &Spawner{outputBase: t.TempDir()}
	taskID := "test-task-leak-fix"

	// Create an effect tracker for the task.
	tracker := s.GetOrCreateEffectTracker(taskID)
	if tracker == nil {
		t.Fatal("GetOrCreateEffectTracker returned nil")
	}

	// Verify the tracker is in the map.
	if _, ok := s.effectTrackers.Load(taskID); !ok {
		t.Fatal("effectTrackers should contain the task after GetOrCreateEffectTracker")
	}

	// Clear the tracker — this is the fix.
	s.ClearEffectTracker(taskID)

	// Verify the tracker is gone.
	if _, ok := s.effectTrackers.Load(taskID); ok {
		t.Error("effectTrackers should NOT contain the task after ClearEffectTracker — memory leak regression")
	}
}
