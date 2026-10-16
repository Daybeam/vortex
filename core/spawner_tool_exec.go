package core

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// toolExecState holds the mutable state shared between the tool execution
// loop in doSpawn and the extracted processToolCalls method.
type toolExecState struct {
	toolResults       []string
	trace             []store.ToolInteraction
	toolCallCount     map[string]int
	toolFailCount     map[string]int
	needsVerification bool
	turnAttachments   []schemas.Attachment
	statesVisited     []string
	previousState     string
}

// processToolCalls executes each tool call in the provider response, updating
// the shared state in place. Extracted from doSpawn's tool execution loop.
func (s *Spawner) processToolCalls(
	ctx context.Context,
	req *SpawnRequest,
	resp *providers.ProviderResponse,
	bindings []config.MCPBinding,
	turnDecisionID string,
	turnCtx *TurnIdempotencyContext,
	state *toolExecState,
) {
	for _, call := range resp.ToolCalls {
		// #2+#14 (eval §8.34): Side-effect classification. Core tools
	// (write_file/read_file/execute_code) are fs-only — no domain
	// side-effect. Domain MCP tools have real side-effects. Pure
	// metrics, zero behavior change.
	if CoreToolNames[call.Name] {
		observability.GetGlobalMetrics().Inc("orchestrator.calls.fs_only", 1)
	} else {
		observability.GetGlobalMetrics().Inc("orchestrator.calls.domain", 1)
	}

	// Inject InputMapping (ADDED 2026-08-17)
	if call.Arguments == nil {
		call.Arguments = make(map[string]any)
	}
	for k, v := range req.InputMapping {
		if _, exists := call.Arguments[k]; !exists {
			call.Arguments[k] = v
		}
	}

	// A1 (Effect Boundary Defense): per-turn idempotency check.
	// If this exact tool call (same name + same args) has already
	// been executed in this turn, skip it. This prevents duplicate
	// execution when a provider returns the same call multiple times.
	// See docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.2.
	opKey := toolCallKey(call.Name, call.Arguments)
	if turnCtx.CheckAndMark(opKey) {
		observability.GetGlobalMetrics().Inc("orchestrator.a1.duplicate_skipped", 1)
		state.toolResults = append(state.toolResults, fmt.Sprintf(
			"[%s] Skipped: identical call already executed in this turn (A1 idempotency defense).",
			call.Name))
		continue
	}

	// #11 (eval §8.41): Identical-call dedup. Allow 2 identical
	// calls (same tool + same args); on the 3rd, require a _reason
	// argument. Legitimate retries (data refresh, waiting for async
	// result) pass by providing _reason. Mindless repetition is
	// blocked, saving turn budget.
	// Per-step scope: resets each step. Different steps may legitimately
	// make the same call (parallel searches, phased gathering).
	callKey := toolCallKey(call.Name, call.Arguments)
	callCount := state.toolCallCount[callKey]
	if callCount >= 2 {
		reason, hasReason := call.Arguments["_reason"].(string)
		if !hasReason || reason == "" {
			observability.GetGlobalMetrics().Inc("orchestrator.dedup.blocked", 1)
			state.toolResults = append(state.toolResults, fmt.Sprintf(
				"[%s] This is the %drd identical call with the same arguments. "+
					"Previous results were identical. If this is a legitimate retry "+
					"(e.g., waiting for data refresh), provide a `_reason` argument "+
					"explaining why. Otherwise, use the previous result.",
				call.Name, callCount+1))
			continue
		}
		// Legitimate retry with reason — allow and log.
		observability.GetGlobalMetrics().Inc("orchestrator.dedup.reasoned_retry", 1)
		s.logger.Log("EventDedupReasonedRetry", req.TaskID, req.StepID, map[string]any{
			"tool":   call.Name,
			"count":  callCount + 1,
			"reason": reason,
		})
	}
	state.toolCallCount[callKey]++

	// Core tools (write_file, read_file, execute_code) — handled directly
	if CoreToolNames[call.Name] {
		taskDir := filepath.Join(s.outputBase, req.TaskID)
		validator := NewSafePathValidator(taskDir, req.SessionRoot, s.allowedWorkspaces)
		res, status, interaction := s.handleCoreToolCall(ctx, call, req.TaskID, req.StepID, validator)
		// A2/A4 (Effect Boundary Defense): record the tool call's effect.
		s.recordEffect(req.TaskID, req.StepID, call.Name, call.Arguments, res, status)
		if status == "error" {
			state.toolFailCount[call.Name]++
			effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
			res = toolFailFeedback(call.Name, res, effCount)
		} else {
			state.toolFailCount[call.Name] = 0
		}
		state.trace = append(state.trace, interaction)
		env := s.processor.Wrap(call.Name, res, status, "orchestrator", "")
		state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
		continue
	}

	mcpID := s.ResolveToolMCP(call.Name, bindings, req.Hub)
	mcpDef := req.Hub.GetMCP(mcpID)
	// Copy arguments so _decision_id stays in the state.trace only and
	// doesn't leak into the actual MCP tool call payload (fix M2).
	interaction := store.ToolInteraction{
		ToolName:  call.Name,
		Arguments: copyToolCallArguments(call.Arguments, turnDecisionID),
	}

	reason := ToolCallReasonLLMInitiated // audit C-15: replaced mojibake string literal
	if r, ok := call.Arguments["_reason"].(string); ok && r != "" {
		reason = r
		delete(call.Arguments, "_reason")
	}

	// #19 (eval §8.42): Write reflection gate. MCP tools that modify
	// external state (cancel_*, update_*, create_*, etc.) require a
	// _reason argument. The purpose is to nudge the model to reflect
	// on the current situation before writing: "What is the state?
	// Should I change strategy? Is this call necessary?"
	// If _reason is provided, the call proceeds — the model reflected
	// and decided to act. If not, the reflection prompt is returned.
	// StagedWorkspace provides rollback for file operations; this gate
	// provides accountability for MCP operations that can't be rolled back.
	if IsWriteMCPTool(call.Name) && reason == ToolCallReasonLLMInitiated {
		// 🔴 audit DEAD-CODE (2026-10-04): the branch below could never
		// fire. Reason was already consumed at :1151-1154, which does
		// `delete(call.Arguments, "_reason")`; the type assertion here
		// re-reads the SAME (now deleted) key, so hasReason was always
		// false and EventWriteAuthAllowed was never emitted. The gate
		// actually worked via the `reason` copy on the outer condition.
		// Kept (not deleted) so the two paths can be distinguished again
		// once the reason is captured before deletion.
		if writeReason, hasReason := call.Arguments["_reason"].(string); hasReason && writeReason != "" {
			// Model reflected and provided a reason — allow the write.
			s.logger.Log("EventWriteAuthAllowed", req.TaskID, req.StepID, map[string]any{
				"tool":   call.Name,
				"mcp":    mcpID,
				"reason": writeReason,
			})
		} else {
			observability.GetGlobalMetrics().Inc("orchestrator.auth.write_blocked", 1)
			s.logger.Log("EventWriteAuthBlocked", req.TaskID, req.StepID, map[string]any{
				"tool": call.Name,
				"mcp":  mcpID,
			})
			// 2026-10-04 (eval §8.51.4-§8.51.7, four prompt rounds measured):
			//
			// R1 prose + fragmentary example — model complied 18 times, all
			//    rejected; it pasted the fragment into another argument's
			//    VALUE with full-width quotes: {"reservation_id": "XEHM4B””, “_reason": "}
			// R2 full example + two WRONG counter-examples — quotes fixed,
			//    but it copied a counter-example: {"reservation_id": "XEHM4B", "}
			// R3 ONE example, no counter-examples — full-width quotes came BACK,
			//    so the counter-examples were never the cause.
			//
			// R4 (this one) — prompt, don't gate. The block comment above
			// states the purpose is to NUDGE reflection ("What is the
			// state? Should I change strategy? Is this call necessary?").
			// It is not an authorization check — the engine has no way to
			// know whether the customer consented. Three prompt rounds
			// failed to make a 4B model satisfy the format, and the cost
			// of continuing to block is that the write NEVER happens
			// (airline task 7: Write Actions 0/3 across all four arms).
			// So: keep the nudge in the tool result, record the intent,
			// and let the call proceed. Guidance preserved, veto removed.
			// Set system.require_write_reason=true to restore hard blocking.
			nudge := fmt.Sprintf(
				"[%s] This call changes external state. Before you rely on it, note: "+
					"what is the current state, and is this write necessary? "+
					"Proceeding without a `_reason`.",
				call.Name)
			interaction.Result = nudge
			state.trace = append(state.trace, interaction)
			if s.registry != nil && s.registry.System.RequireWriteReason {
				nudge = fmt.Sprintf(
					"[%s] AUTHORIZATION REQUIRED — this call changes external state.\n"+
						"Add a top-level key named \"_reason\" whose value is one short sentence.\n"+
						"Call it again with this exact shape:\n"+
						"  {\"reservation_id\": \"<the id>\", \"_reason\": \"<why this write is correct>\"}",
					call.Name)
				interaction.Result = nudge
				env := s.processor.Wrap(call.Name, nudge, "error", "orchestrator", reason)
				state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
				continue
			}
			s.logger.Log("EventWriteAuthNudged", req.TaskID, req.StepID, map[string]any{
				"tool": call.Name,
				"mcp":  mcpID,
			})
		}
	}

	if mcpDef == nil {
		state.toolFailCount[call.Name]++
		effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
		res := toolFailFeedback(call.Name, fmt.Sprintf("MCP server %s not found for tool %s", mcpID, call.Name), effCount)
		interaction.Result = res
		state.trace = append(state.trace, interaction)

		env := s.processor.Wrap(call.Name, res, "error", "orchestrator", reason)
		state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
		continue
	}

	if isStateChangingTool(call.Name) {
		state.needsVerification = true
	}

	// #2-universal (eval §23): a state-changing (write) call invalidates
	// every cached read for this MCP server, keeping the read cache
	// coherent with external state. Generic: any write verb prefix
	// (cancel_/update_/create_/...) or known state-changing tool.
	if s.domainReadCache != nil && (IsWriteMCPTool(call.Name) || isStateChangingTool(call.Name)) {
		s.domainReadCache.Invalidate(mcpID)
	}

	// Bookmark_List Routing: Inject trusted domains into search-like tools
	if strings.Contains(strings.ToLower(call.Name), "search") && len(req.Hub.Registry.System.Bookmarks) > 0 {
		if call.Arguments == nil {
			call.Arguments = make(map[string]any)
		}
		call.Arguments["_bookmarks"] = req.Hub.Registry.System.Bookmarks
	}

	// #2-universal (eval §23): replay cached read results to break
	// repetition loops (execution amplification) without force-terminating
	// the conversation. Generic: any read-only domain tool (get_/list_/
	// search_/query_/read_/fetch_/check_/is_/has_*) is eligible; pollers
	// (progress/health/wait) and state-changing tools are excluded. The
	// model receives the identical previously-computed markdown, so the
	// repeated call costs zero extra tool execution.
	if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
		if cachedMD, ok := s.domainReadCache.Get(mcpID, call.Name, call.Arguments); ok {
			log.Printf("[spawner] read_cache HIT task=%s step=%s tool=%s mcp=%s", req.TaskID, req.StepID, call.Name, mcpID)
			observability.GetGlobalMetrics().Inc("orchestrator.read_cache.hit", 1)
			interaction.Result = cachedMD
			state.trace = append(state.trace, interaction)
			state.toolResults = append(state.toolResults, cachedMD)
			continue
		}
	}

	if mcpDef.Command != "" {
		var finalRes any
		var execErr error

		// Tool Execution Timeout (default 120s)
		toolCtx, cancelTool := context.WithTimeout(ctx, 120*time.Second)

		// Helper to perform the actual call (including SAV verification)
		doCall := func() (any, error) {
			cli, err := s.mcpMgr.ensureMCPClient(toolCtx, mcpDef, req.TaskID)
			if err != nil {
				return nil, err
			}

			verifierID := req.Hub.Registry.GetVerifierID(call.Name)
			if verifierID != "" {
				if v, ok := s.verifierRegistry[verifierID]; ok {
					state, sErr := v.Sense(toolCtx, mcpID, call.Name, s)
					if sErr == nil {
						res, err := cli.SendRequest(toolCtx, "tools/call", map[string]any{
							"name":      call.Name,
							"arguments": call.Arguments,
						})
						if err != nil {
							return nil, err
						}
						success, vErr := v.Verify(toolCtx, mcpID, call.Name, state, res, s)
						if !success {
							return v.Recover(toolCtx, mcpID, call.Name, call.Arguments, vErr, s)
						}
						return res, nil
					}
					return nil, fmt.Errorf("SAV Sense failed: %v", sErr)
				}
			}

			return cli.SendRequest(toolCtx, "tools/call", map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			})
		}

		finalRes, execErr = doCall()

		// --- MCP Stdio Robustness: Auto-Reconnect Retry ---
		// If the process crashed or the pipe broke, evict from cache and retry once.
		if execErr != nil && client.IsConnectionError(execErr) {
			s.logger.Log("EventMCPReconnectAttempt", req.TaskID, req.StepID, map[string]any{
				"mcp":   mcpDef.ID,
				"tool":  call.Name,
				"error": execErr.Error(),
			})
			s.mcpMgr.EvictClient(mcpDef.ID)

			finalRes, execErr = doCall()
		}
		cancelTool()

		if execErr != nil {
			state.toolFailCount[call.Name]++
			effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
			resErr := toolFailFeedback(call.Name, fmt.Sprintf("Error executing tool %s: %v", call.Name, execErr), effCount)
			interaction.Result = resErr
			env := s.processor.Wrap(call.Name, resErr, "error", "mcp:"+mcpDef.ID, reason)
			state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
		} else {
			// SERF: check MCP isError field before treating as success (ADDED 2026-10-03)
			isToolError, serf, errMsg := parseMCPError(finalRes)
			if isToolError {
				state.toolFailCount[call.Name]++
				effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
				recoveryMsg := buildSERFRecovery(call.Name, serf, errMsg, effCount)
				interaction.Result = recoveryMsg
				env := s.processor.Wrap(call.Name, recoveryMsg, "error", "mcp:"+mcpDef.ID, reason)
				state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
			} else {
				state.toolFailCount[call.Name] = 0
				interaction.Result = finalRes
				interaction.Meta = extractMeta(finalRes) // Phase 2: extract structured metadata
				if att, ok := finalRes.(schemas.SubagentOutput); ok {
					state.turnAttachments = append(state.turnAttachments, att.Attachments...)
				}
				status := "ok"
				if m, ok := finalRes.(map[string]any); ok {
					if s, ok := m["status"].(string); ok {
						status = s
					}
				}
				env := s.processor.Wrap(call.Name, finalRes, status, "mcp:"+mcpDef.ID, reason)
				md := env.ToMarkdown(call.Name)

				// Apply Sieve Pruning to reduce token noise for large tool outputs
				if len(md) > 2000 {
					md = s.sieve.PruneOutput(md, req.Task)
				}
				// #2-universal (eval §23): cache the successful read result
				// for replay on identical future calls within this server.
				// Cache the clean md before any repetition warning is prepended.
				if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
					s.domainReadCache.Put(mcpID, call.Name, call.Arguments, md)
				}
				// Egress redaction (I5): strip secrets before result enters LLM context.
				// Output bounding: adaptive truncation for oversized results.
				md = RedactEgress(md)
				if len(md) > 2000 {
					md = string(AdaptiveOutputResult([]byte(md), call.Name))
				}
				state.toolResults = append(state.toolResults, md)
			}
		}
	} else if mcpDef.URL != "" {
		toolCtx, cancelTool := context.WithTimeout(ctx, 60*time.Second)
		finalRes, execErr := s.mcpMgr.callRemoteMCPTool(toolCtx, mcpDef, call.Name, call.Arguments)
		cancelTool()

		if execErr != nil {
			state.toolFailCount[call.Name]++
			effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
			resErr := toolFailFeedback(call.Name, fmt.Sprintf("Error executing remote tool %s: %v", call.Name, execErr), effCount)
			interaction.Result = resErr
			env := s.processor.Wrap(call.Name, resErr, "error", "mcp:"+mcpDef.ID, reason)
			state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
		} else {
			// SERF: check MCP isError field before treating as success (ADDED 2026-10-03)
			isToolError, serf, errMsg := parseMCPError(finalRes)
			if isToolError {
				state.toolFailCount[call.Name]++
				effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
				recoveryMsg := buildSERFRecovery(call.Name, serf, errMsg, effCount)
				interaction.Result = recoveryMsg
				env := s.processor.Wrap(call.Name, recoveryMsg, "error", "mcp:"+mcpDef.ID, reason)
				state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
			} else {
				state.toolFailCount[call.Name] = 0
				interaction.Result = finalRes
				status := "ok"
				if m, ok := finalRes.(map[string]any); ok {
					if s, ok := m["status"].(string); ok {
						status = s
					}
				}
				env := s.processor.Wrap(call.Name, finalRes, status, "mcp:"+mcpDef.ID, reason)
				md := env.ToMarkdown(call.Name)

				// Apply Sieve Pruning to reduce token noise for large tool outputs,
				// mirroring the local (command-based) MCP branch above.
				if len(md) > 2000 {
					md = s.sieve.PruneOutput(md, req.Task)
				}
				// #2-universal (eval §23): cache the successful read result.
				// Cache the clean md before any repetition warning is prepended.
				if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
					s.domainReadCache.Put(mcpID, call.Name, call.Arguments, md)
				}
				// Egress redaction (I5): strip secrets before result enters LLM context.
				md = RedactEgress(md)
				if len(md) > 2000 {
					md = string(AdaptiveOutputResult([]byte(md), call.Name))
				}
				state.toolResults = append(state.toolResults, md)
			}
		}
	} else {
		state.toolFailCount[call.Name]++
		effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
		res := toolFailFeedback(call.Name, fmt.Sprintf("MCP %s has neither a local command nor a remote URL configured for tool %s", mcpDef.ID, call.Name), effCount)
		interaction.Result = res
		env := s.processor.Wrap(call.Name, res, "error", "orchestrator:cloud", reason)
		state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
	}
	// A2/A4 (Effect Boundary Defense): record the MCP tool call's effect.
	s.recordEffect(req.TaskID, req.StepID, call.Name, call.Arguments, fmt.Sprintf("%v", interaction.Result), "unknown")
	state.trace = append(state.trace, interaction)

	// ── PGPO: State Observation Loop (ADDED 2026-09-08) ──
	currentState := s.GenerateStateSignature(call.Name, interaction.Result)
	state.statesVisited = append(state.statesVisited, currentState)
	if alert, delta := s.EvaluatePotentialDrop(state.previousState, currentState); alert {
		intervention := s.TriggerProactiveIntervention(currentState, req.Task)
		state.toolResults = append(state.toolResults, intervention)
		s.logger.Log("EventPotentialDropAlert", req.TaskID, req.StepID, map[string]any{
			"previous_state": state.previousState,
			"current_state":  currentState,
			"delta":          delta,
		})
	}
	state.previousState = currentState
	}
}
