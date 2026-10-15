package core

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ─── Effect Boundary Defense: A2/A4 Effect Tracking + A3 Outcome Probe ────
//
// Design ref: docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.3
//
// EffectTracker records every tool call's external effect (not just file
// operations like StagedWorkspace). On step failure, CompensateStep probes
// external state (A3) and attempts compensation in reverse order (A2/A4).
//
// Relationship with StagedWorkspace:
//   - File effects (SideEffectFile): delegated to StagedWorkspace.Rollback
//   - External effects (api/database/network): probed + compensated here
//   - Unknown effects: marked needs_manual_review

// SideEffectClass classifies the external effect of a tool call.
type SideEffectClass string

const (
	SideEffectNone     SideEffectClass = "none"     // read-only, no side effect
	SideEffectFile     SideEffectClass = "file"     // file system operation
	SideEffectAPI      SideEffectClass = "api"      // external API call
	SideEffectDatabase SideEffectClass = "database" // database write
	SideEffectNetwork  SideEffectClass = "network"  // network operation
)

// EffectRecord records a single tool call's external effect.
type EffectRecord struct {
	OpID          string          `json:"op_id"`                   // toolCallKey (FNV-1a)
	StepID        string          `json:"step_id"`                 // step that produced this effect
	TaskID        string          `json:"task_id"`                 // task this effect belongs to
	ToolName      string          `json:"tool_name"`               // tool that was called
	Arguments     map[string]any  `json:"arguments"`               // call arguments
	Result        string          `json:"result"`                  // call result (truncated)
	Status        string          `json:"status"`                  // success, error, unknown
	SideEffect    SideEffectClass `json:"side_effect"`             // classified side effect
	Compensatable bool            `json:"compensatable"`           // whether compensation is possible
	Compensator   string          `json:"compensator,omitempty"`   // compensator action description
	Timestamp     time.Time       `json:"timestamp"`               // when the effect was recorded
	NeedsReview   bool            `json:"needs_review,omitempty"`  // A3: flagged for manual review
	ReviewReason  string          `json:"review_reason,omitempty"` // why manual review is needed
}

// EffectContract declares a tool's effect profile for compensation.
// Registered per tool name to override the default classification.
type EffectContract struct {
	SideEffectClass   SideEffectClass
	Compensatable     bool
	CompensatorAction string // human-readable description of the compensator
}

// ProbeState is the result of probing external state (A3 outcome probe).
type ProbeState int

const (
	ProbeNotApplied ProbeState = 0  // effect confirmed not applied externally
	ProbeApplied    ProbeState = 1  // effect confirmed applied externally
	ProbeUnknown    ProbeState = -1 // cannot determine external state
)

// ProbeResult wraps a ProbeState with optional detail.
type ProbeResult struct {
	State  ProbeState
	Detail string
}

// CompensationResult records the outcome of one compensation attempt.
type CompensationResult struct {
	OpID     string `json:"op_id"`
	ToolName string `json:"tool_name"`
	Action   string `json:"action"` // "skipped", "compensated", "needs_review", "file_rollback"
	Detail   string `json:"detail"` // human-readable explanation
}

// ProberFunc probes external state to check if an effect was applied.
type ProberFunc func(*EffectRecord) ProbeResult

// CompensatorFunc attempts to compensate (reverse) an effect.
type CompensatorFunc func(*EffectRecord) (string, error)

// EffectTracker tracks all external effects for one task.
type EffectTracker struct {
	TaskDir      string
	records      []*EffectRecord
	contracts    map[string]EffectContract  // toolName → contract
	probers      map[string]ProberFunc      // toolName → prober
	compensators map[string]CompensatorFunc // toolName → compensator
	mu           sync.Mutex
}

// NewEffectTracker creates a new EffectTracker for a task.
func NewEffectTracker(taskDir string) *EffectTracker {
	return &EffectTracker{
		TaskDir:      taskDir,
		contracts:    make(map[string]EffectContract),
		probers:      make(map[string]ProberFunc),
		compensators: make(map[string]CompensatorFunc),
	}
}

// classifySideEffect determines the side-effect class of a tool call.
// Core tools are classified by name; MCP tools by name prefix.
func classifySideEffect(toolName string, isCore bool) SideEffectClass {
	if isCore {
		switch toolName {
		case "read_file", "list_files", "decompress":
			return SideEffectNone
		default:
			return SideEffectFile // write_file, execute_code, etc.
		}
	}
	// MCP tools: classify by name prefix (heuristic).
	lower := strings.ToLower(toolName)
	// Read-only prefixes — no side effect.
	for _, prefix := range []string{"get_", "list_", "search_", "query_", "read_", "fetch_", "check_", "is_", "has_"} {
		if strings.HasPrefix(lower, prefix) {
			return SideEffectNone
		}
	}
	// Write prefixes — external side effect.
	for _, prefix := range []string{"cancel_", "update_", "create_", "delete_", "set_", "send_", "post_", "put_"} {
		if strings.HasPrefix(lower, prefix) {
			return SideEffectAPI
		}
	}
	// Unknown tool — assume it has a network side effect (conservative).
	return SideEffectNetwork
}

// RecordEffect records a tool call's effect after execution.
// Called from the Spawner turnLoop after each tool call completes.
func (t *EffectTracker) RecordEffect(taskID, stepID, toolName string, args map[string]any, result string, status string) *EffectRecord {
	isCore := CoreToolNames[toolName]
	sideEffect := classifySideEffect(toolName, isCore)

	// Override with registered contract if available.
	compensatable := false
	compensator := ""
	t.mu.Lock()
	if contract, ok := t.contracts[toolName]; ok {
		sideEffect = contract.SideEffectClass
		compensatable = contract.Compensatable
		compensator = contract.CompensatorAction
	}
	t.mu.Unlock()

	rec := &EffectRecord{
		OpID:          toolCallKey(toolName, args),
		StepID:        stepID,
		TaskID:        taskID,
		ToolName:      toolName,
		Arguments:     args,
		Result:        truncateEffectResult(result),
		Status:        status,
		SideEffect:    sideEffect,
		Compensatable: compensatable,
		Compensator:   compensator,
		Timestamp:     time.Now(),
	}

	t.mu.Lock()
	t.records = append(t.records, rec)
	t.mu.Unlock()

	return rec
}

// truncateEffectResult limits stored result size to avoid unbounded memory.
func truncateEffectResult(result string) string {
	const maxLen = 4096
	if len(result) > maxLen {
		return result[:maxLen] + "...[truncated]"
	}
	return result
}

// GetByStep returns all effect records for a given step, in execution order.
func (t *EffectTracker) GetByStep(stepID string) []*EffectRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	var result []*EffectRecord
	for _, rec := range t.records {
		if rec.StepID == stepID {
			result = append(result, rec)
		}
	}
	return result
}

// GetRecords returns all effect records (for inspection/testing).
func (t *EffectTracker) GetRecords() []*EffectRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]*EffectRecord, len(t.records))
	copy(result, t.records)
	return result
}

// RegisterContract registers an effect contract for a tool (overrides classification).
func (t *EffectTracker) RegisterContract(toolName string, contract EffectContract) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.contracts[toolName] = contract
}

// RegisterProber registers an external state prober for a tool (A3).
func (t *EffectTracker) RegisterProber(toolName string, prober ProberFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.probers[toolName] = prober
}

// RegisterCompensator registers a compensator function for a tool.
func (t *EffectTracker) RegisterCompensator(toolName string, compensator CompensatorFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.compensators[toolName] = compensator
}

// ProbeExternalState checks if an effect was actually applied externally (A3).
// If no prober is registered for the tool, returns ProbeUnknown.
func (t *EffectTracker) ProbeExternalState(rec *EffectRecord) ProbeResult {
	t.mu.Lock()
	prober, ok := t.probers[rec.ToolName]
	t.mu.Unlock()
	if !ok {
		return ProbeResult{State: ProbeUnknown, Detail: "no prober registered"}
	}
	return prober(rec)
}

// CompensateStep attempts to compensate all effects for a step in reverse order.
// File effects are noted as "file_rollback" (delegated to StagedWorkspace).
// External effects are probed (A3) and compensated if possible.
// Returns one CompensationResult per effect record.
func (t *EffectTracker) CompensateStep(stepID string) []CompensationResult {
	records := t.GetByStep(stepID)
	var results []CompensationResult

	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		results = append(results, t.compensateOne(rec))
	}

	return results
}

// compensateOne handles compensation for a single effect record.
func (t *EffectTracker) compensateOne(rec *EffectRecord) CompensationResult {
	switch rec.SideEffect {
	case SideEffectNone:
		return CompensationResult{
			OpID: rec.OpID, ToolName: rec.ToolName,
			Action: "skipped", Detail: "no side effect",
		}

	case SideEffectFile:
		// File effects are handled by StagedWorkspace.Rollback (existing).
		// EffectTracker just notes the delegation.
		return CompensationResult{
			OpID: rec.OpID, ToolName: rec.ToolName,
			Action: "file_rollback", Detail: "delegated to StagedWorkspace",
		}

	default:
		// External effect (api, database, network) — A3 outcome probe.
		return t.compensateExternal(rec)
	}
}

// compensateExternal probes external state and attempts compensation.
// Contracts and compensators are looked up at compensation time, not just
// at recording time — this allows registering contracts after effects are
// recorded (e.g., contracts registered at startup, effects during execution).
func (t *EffectTracker) compensateExternal(rec *EffectRecord) CompensationResult {
	probed := t.ProbeExternalState(rec)

	switch probed.State {
	case ProbeNotApplied:
		// Effect confirmed not applied — safe, no compensation needed.
		return CompensationResult{
			OpID: rec.OpID, ToolName: rec.ToolName,
			Action: "skipped", Detail: "effect not applied (probe confirmed)",
		}

	case ProbeApplied:
		// Effect confirmed applied — check compensatability.
		// Look up contract at compensation time (may have been registered
		// after the effect was recorded).
		t.mu.Lock()
		contract, hasContract := t.contracts[rec.ToolName]
		compensator, hasCompensator := t.compensators[rec.ToolName]
		t.mu.Unlock()

		compensatable := rec.Compensatable
		compensatorAction := rec.Compensator
		if hasContract {
			compensatable = contract.Compensatable
			compensatorAction = contract.CompensatorAction
		}

		if !compensatable || compensatorAction == "" {
			t.markNeedsReview(rec, "effect applied but not compensatable")
			return CompensationResult{
				OpID: rec.OpID, ToolName: rec.ToolName,
				Action: "needs_review", Detail: "effect applied, not compensatable",
			}
		}

		if !hasCompensator {
			t.markNeedsReview(rec, "compensator declared but not registered")
			return CompensationResult{
				OpID: rec.OpID, ToolName: rec.ToolName,
				Action: "needs_review", Detail: "compensator not registered",
			}
		}

		detail, err := compensator(rec)
		if err != nil {
			t.markNeedsReview(rec, fmt.Sprintf("compensator failed: %v", err))
			return CompensationResult{
				OpID: rec.OpID, ToolName: rec.ToolName,
				Action: "needs_review", Detail: fmt.Sprintf("compensator error: %v", err),
			}
		}
		return CompensationResult{
			OpID: rec.OpID, ToolName: rec.ToolName,
			Action: "compensated", Detail: detail,
		}

	default: // ProbeUnknown
		t.markNeedsReview(rec, "probe returned unknown")
		return CompensationResult{
			OpID: rec.OpID, ToolName: rec.ToolName,
			Action: "needs_review", Detail: "probe unknown — cannot determine if effect was applied",
		}
	}
}

// markNeedsReview marks an effect record as needing manual review.
func (t *EffectTracker) markNeedsReview(rec *EffectRecord, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec.NeedsReview = true
	rec.ReviewReason = reason
}

// NeedsReviewCount returns the number of records flagged for manual review.
func (t *EffectTracker) NeedsReviewCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, rec := range t.records {
		if rec.NeedsReview {
			count++
		}
	}
	return count
}
