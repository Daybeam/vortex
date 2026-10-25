package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/core/skeleton"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// checkSieveGuardian runs the Sieve loop-detection interceptor on planned
// tool calls. Returns (true, "") if the call plan is valid.
// Returns (false, reason) if the Sieve detects a loop — the caller should
// abort the spawn with a failed output.
// Extracted from spawner.go per SPAWNER_REFACTORING_EXECUTION_PLAN.md Step 2.
func (s *Spawner) checkSieveGuardian(taskID, stepID string, turn int, toolCalls []schemas.ToolCall, trace []store.ToolInteraction) (bool, string) {
	plannedInteractions := make([]store.ToolInteraction, len(toolCalls))
	for i, tc := range toolCalls {
		plannedInteractions[i] = store.ToolInteraction{
			ToolName:  tc.Name,
			Arguments: tc.Arguments,
		}
	}

	valid, reason := s.sieve.InspectToolCalls(taskID, stepID, plannedInteractions)
	if !valid {
		var loopDetail []map[string]any
		for _, pi := range plannedInteractions {
			argsJSON, _ := json.Marshal(pi.Arguments)
			entry := map[string]any{
				"tool":      pi.ToolName,
				"arguments": truncateForLog(string(argsJSON), 500),
			}
			for i := len(trace) - 1; i >= 0; i-- {
				if trace[i].ToolName == pi.ToolName {
					entry["prior_result"] = truncateForLog(formatForLog(trace[i].Result), 500)
					break
				}
			}
			loopDetail = append(loopDetail, entry)
		}
		s.logger.Log(EventTaskFailed, taskID, stepID, map[string]any{
			"error":       "Sieve Guardian intercepted",
			"reason":      reason,
			"turn":        turn,
			"loop_detail": loopDetail,
		})
	}
	return valid, reason
}

// handleCoreToolCall executes a core tool (write_file, read_file, execute_code,
// decompress) directly without MCP transport. Returns the result string,
// status, and the tool interaction record for the trace.
// Extracted from spawner.go per SPAWNER_REFACTORING_EXECUTION_PLAN.md Step 2.
func (s *Spawner) handleCoreToolCall(ctx context.Context, call schemas.ToolCall, taskID, stepID string, validator *SafePathValidator, ownerID string) (string, string, store.ToolInteraction) {
	var result string
	var herr error

	if call.Name == "decompress" {
		// Handle decompress with the Spawner's FoldEngine.
		segmentID, _ := call.Arguments["segment_id"].(string)
		if segmentID == "" {
			result = "Error: segment_id is required"
			herr = fmt.Errorf("segment_id is required")
		} else if s.foldEngine == nil {
			result = "Error: decompress is not available (no fold engine)"
			herr = fmt.Errorf("fold engine not available")
		} else {
			result, herr = s.foldEngine.Decompress(segmentID, taskID)
		}
	} else if call.Name == "search_archive" {
		// Handle search_archive with the Spawner's ContextArchive.
		query, _ := call.Arguments["query"].(string)
		limit := 5
		if l, ok := call.Arguments["limit"].(float64); ok && int(l) > 0 && int(l) <= 10 {
			limit = int(l)
		}
		if query == "" {
			result = "Error: query is required"
			herr = fmt.Errorf("query is required")
		} else if s.archive == nil {
			result = "Error: search_archive is not available (no archive)"
			herr = fmt.Errorf("archive not available")
		} else {
			forest, err := s.archive.SearchForest(query, nil, limit, 0.9, taskID, taskID)
			if err != nil {
				result = fmt.Sprintf("Search error: %v", err)
				herr = err
			} else {
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("Found %d matching items:\n\n", len(forest.Nodes)))
				for i, n := range forest.Nodes {
					sb.WriteString(fmt.Sprintf("## %d. [%s] %s\n", i+1, n.NodeID, n.Intent))
					if n.Summary != "" {
						sb.WriteString(n.Summary + "\n\n")
					}
				}
				result = sb.String()
			}
		}
	} else {
		// A5 (Effect Boundary Defense): dry-run pre-check for high-risk tools.
		// If the tool has RiskLevel=high and supports dry_run, execute with
		// dry_run=true to preview the operation without committing it.
		// If high-risk but no dry_run support, log a warning.
		// See docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.4.
		toolDef := findCoreToolDef(call.Name)
		if isHighRiskTool(toolDef) {
			if supportsDryRun(toolDef) {
				// Execute with dry_run=true — preview without committing.
				dryArgs := addDryRunArg(call.Arguments)
				dryResult, dryErr := HandleCoreTool(ctx, call.Name, dryArgs, s.outputBase, taskID, validator)
				if dryErr == nil {
					result = dryRunResult(call.Name, dryResult)
					s.logger.Log("EventA5DryRunPreview", taskID, stepID, map[string]any{
						"tool":  call.Name,
						"args":  marshalArgsForLog(call.Arguments),
						"phase": "dry_run_preview",
					})
				} else {
					// dry_run failed — the real operation would likely fail too.
					result = fmt.Sprintf("[DRY-RUN FAILED] %s: %v\nOperation aborted to prevent likely failure.", call.Name, dryErr)
					herr = dryErr
				}
			} else {
				// High-risk tool without dry_run support — log warning, proceed.
				s.logger.Log("EventA5HighRiskNoDryRun", taskID, stepID, map[string]any{
					"tool":  call.Name,
					"args":  marshalArgsForLog(call.Arguments),
					"phase": "high_risk_no_dry_run",
				})
				result, herr = HandleCoreTool(ctx, call.Name, call.Arguments, s.outputBase, taskID, validator)
			}
		} else {
			result, herr = HandleCoreTool(ctx, call.Name, call.Arguments, s.outputBase, taskID, validator)
		}
	}

	res := result
	status := "success"
	if herr != nil {
		res = fmt.Sprintf("Error: %v", herr)
		status = "error"
	}
	// Egress redaction (I5): strip secrets before result enters LLM context.
	// Output bounding: adaptive truncation for oversized tool results.
	// See docs/completed/2026-10-21/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §4.2.
	res = RedactEgressWithOwner(res, ownerID)
	// Skip second truncation for execute_code — its output is already gated
	// inside executeCode() via AdaptiveOutputResult (8KB threshold). Double
	// truncation at 2000 bytes would destroy useful script output that the
	// model intentionally chose to produce instead of multiple read_file calls.
	// See docs/plan/MODEL_DRIVEN_ARCHIVE_RETRIEVAL_DESIGN.md §3.3.0.
	if call.Name != "execute_code" && len(res) > 2000 {
		res = string(skeleton.AdaptiveOutputResult([]byte(res), call.Name, "orchestrator"))
	}
	interaction := store.ToolInteraction{
		ToolName:  call.Name,
		Arguments: call.Arguments,
		Result:    res,
		Meta:      extractMeta(res), // Phase 2: extract structured metadata
	}
	s.logger.Log("EventCoreToolCall", taskID, stepID, map[string]any{
		"tool":   call.Name,
		"status": status,
	})
	return res, status, interaction
}

// ClearSieveHistory clears the Sieve loop-detection history for a task.
// Moved from spawner.go during god-class split.
func (s *Spawner) ClearSieveHistory(taskID string) {
	if s.sieve != nil {
		s.sieve.ClearHistory(taskID)
	}
}

// extractMeta extracts structured ToolMetadata from a tool result if present.
// MCP servers may return {"data": ..., "meta": {"truncated": true, ...}}.
// Returns nil if the result is not a map or has no "meta" key.
func extractMeta(result any) *store.ToolMetadata {
	m, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	metaVal, ok := m["meta"]
	if !ok {
		return nil
	}
	metaMap, ok := metaVal.(map[string]any)
	if !ok {
		return nil
	}
	meta := &store.ToolMetadata{}
	if v, ok := metaMap["total"].(float64); ok {
		meta.Total = int(v)
	}
	if v, ok := metaMap["returned"].(float64); ok {
		meta.Returned = int(v)
	}
	if v, ok := metaMap["truncated"].(bool); ok {
		meta.Truncated = v
	}
	if v, ok := metaMap["hint"].(string); ok {
		meta.Hint = v
	}
	if v, ok := metaMap["data_shape"].(string); ok {
		meta.DataShape = v
	}
	return meta
}
