package core

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/skeleton"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// dedupAttachments appends incoming attachments to existing, skipping any whose
// Path is already present. Screenshots from the same tool often overwrite the
// same file path across turns; re-sending the identical base64 blob wastes
// tokens. See docs/plan/VLM_IMAGE_CONTEXT_OPTIMIZATION_DESIGN.md §1a.
func dedupAttachments(existing, incoming []schemas.Attachment) []schemas.Attachment {
	for _, a := range incoming {
		dup := false
		for _, e := range existing {
			if e.Path == a.Path {
				dup = true
				break
			}
		}
		if !dup {
			existing = append(existing, a)
		}
	}
	return existing
}

// toolExecState holds the mutable state shared between the tool execution
// loop in doSpawn and the extracted processToolCalls method.
type toolExecState struct {
	toolResults   []string
	stickyResults []string // auth/login results kept near top of context (AppWorld eval: prevent burial at pos 5/51)
	trace         []store.ToolInteraction
	toolCallCount map[string]int
	toolFailCount map[string]int
	// resultStreak counts, per canonical args key, how many consecutive calls
	// returned byte-identical output. This — not the raw call count — is what
	// the reason-blind hard cap blocks on. See noteToolResult.
	resultStreak      map[string]int
	resultLastHash    map[string]uint64
	needsVerification bool
	turnAttachments   []schemas.Attachment
	statesVisited     []string
	previousState     string
}

// stickyToolPatterns matches tool names that produce persistent context (auth,
// login, tokens). Results from these tools are kept near the top of the context
// window to prevent burial at position N/51 (AppWorld eval finding).
var stickyToolPatterns = []string{
	"login", "auth", "get_token", "get_session", "get_user",
	"authenticate", "signin", "connect", "get_api_key", "get_credentials",
}

// isStickyResult returns true if the tool name matches a sticky pattern.
// These results are promoted to a "Persistent Context" block prepended to
// the conversation so the model always has auth/session info near the top.
func isStickyResult(toolName string) bool {
	lower := strings.ToLower(toolName)
	for _, pat := range stickyToolPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// Result-repetition escalation thresholds. The second identical payload earns a
// nudge; the fourth is withheld, because by then the payload is already sitting
// in context two or three times and each extra copy displaces something the
// model actually needs.
const (
	resultRepetitionSteerAt = 2
	resultRepetitionStubAt  = 4
)

// steerRepeatedResult is the content-side counterpart to checkSieveGuardian.
//
// checkSieveGuardian hashes tool + arguments, so it only catches a literal
// repeat. A model that keeps the tool and permutes its arguments — query=Rent,
// query=Utilities, query=rent, page_limit=20 then 50 — mints a fresh hash on
// every call and sails through, even when the server returns byte-identical
// data. That is exactly what 4fab96f_3 did on 2026-10-19: 91 of 102 tool calls
// were re-reads of two payloads (7597 and 7380 chars) that never changed, and
// no guard fired once before MAX_TURNS_EXHAUSTED.
//
// Content is the signal the model cannot permute away. Identical bytes in means
// zero information out, whatever the arguments said.
//
// Scoped per (task, step, tool): two steps may legitimately read the same data,
// and calling that a loop would be wrong. Pollers are exempt — for them the
// same payload is not evidence of being stuck.
func (s *Spawner) steerRepeatedResult(taskID, stepID, tool, md string) string {
	if s.sieve == nil || md == "" || !isReadTool(tool) || isPollerTool(tool) {
		return md
	}
	n := s.sieve.ObserveResult(taskID, stepID, tool, md)
	switch {
	case n >= resultRepetitionStubAt:
		observability.GetGlobalMetrics().Inc("orchestrator.result_repetition.withheld", 1)
		s.logger.Log("EventResultRepetition", taskID, stepID, map[string]any{
			"tool":     tool,
			"count":    n,
			"chars":    len(md),
			"action":   "withheld",
			"released": len(md),
		})
		// Deliberately does NOT offer "or stop" as an alternative. An earlier
		// version did, and on 4fab96f_3 the model took it: 15 turns, 10 calls,
		// status=partial, with venmo__remind_payment_request — the only write the
		// task needed — sitting in its tool surface unused. A guard that removes
		// information has to point at the next action, not at the exit.
		return fmt.Sprintf("[%s] Identical result withheld: call #%d of this step returned the same %d characters as before, so nothing changed and re-reading cannot help. You already have this data in context. Do not call %s again in this step — take the next action the task requires with the tools you have.",
			tool, n, len(md), tool)
	case n >= resultRepetitionSteerAt:
		observability.GetGlobalMetrics().Inc("orchestrator.result_repetition.steer", 1)
		s.logger.Log("EventResultRepetition", taskID, stepID, map[string]any{
			"tool":  tool,
			"count": n,
			"chars": len(md),
		})
		return fmt.Sprintf("[RESULT REPETITION] %s returned the same %d characters as an earlier call in this step (occurrence #%d). Different arguments produced no different data — the source is not changing, so re-querying it cannot help. Work with what you already have and move on to the next action the task requires.\n\n%s",
			tool, len(md), n, md)
	}
	return md
}

// noteToolResult records the outcome of a completed call so the reason-blind
// hard cap can count "same call, same answer" instead of just "same call".
//
// Identical arguments are not by themselves evidence of a loop. `venmo__login`
// is called with the same username and password every time its access token
// expires (AppWorld expires them every 10–30 minutes at random), and each call
// returns a *different* token — a legitimate retry. Counting arguments alone
// blocked exactly that: in trial 2 of passk_20261021_074251 the model reasoned
// "refresh the access token, previous calls failed with authorization errors"
// and the args counter had already reached 4. The third element of the tuple —
// the observation — is what distinguishes a loop from a retry, which is also
// how OpenHands (action↔observation pairs) and Gemini CLI (identical content
// chunks) draw the line.
func noteToolResult(state *toolExecState, callKey, md string) {
	if state == nil || md == "" || callKey == "" {
		return
	}
	h := fnv.New64a()
	h.Write([]byte(md))
	sum := h.Sum64()
	if state.resultLastHash[callKey] == sum {
		state.resultStreak[callKey]++
		return
	}
	state.resultLastHash[callKey] = sum
	state.resultStreak[callKey] = 1
}

// prependSticky prepends a "Persistent Context" block to the conversation
// blocks if stickyContext is non-empty. This keeps auth/login results near
// the top of the context window instead of buried at position N/51.
func prependSticky(blocks []schemas.ContentBlock, stickyContext string) []schemas.ContentBlock {
	if stickyContext == "" {
		return blocks
	}
	return append([]schemas.ContentBlock{{
		Type: "text",
		Text: "## Persistent Context\n" + stickyContext,
	}}, blocks...)
}

// accumulateSticky appends new sticky results to the current sticky context,
// skipping empty strings and duplicates (by substring containment). Returns
// the updated context string.
func accumulateSticky(current string, newResults []string) string {
	// audit fix P4: use strings.Builder instead of `+=` which was O(L²) due to
	// copying the entire accumulated string on every append. The Builder
	// amortizes growth; b.String() for the dedup check is O(current) per
	// result, far cheaper than the quadratic copy chain.
	var b strings.Builder
	b.WriteString(current)
	for _, sr := range newResults {
		if sr != "" && !strings.Contains(b.String(), sr) {
			b.WriteString(sr)
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

// logSERFFailureClass maps a SERF category to FailureClass and emits an
// event + metric. Bridges SERF metadata into scheduler retry/decision
// observability. Wired at both local and remote MCP error sites.
func (s *Spawner) logSERFFailureClass(taskID, stepID, toolName string, serf *SERFError) {
	if serf == nil {
		return
	}
	fc := SERFCategoryToFailureClass(serf.Category)
	s.logger.Log("EventSERFFailureClassified", taskID, stepID, map[string]any{
		"tool":          toolName,
		"serf_category": serf.Category,
		"failure_class": string(fc),
		"retryable":     serf.Retryable,
	})
	observability.GetGlobalMetrics().Inc("orchestrator.serf.failure."+string(fc), 1)
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

		// #11b: reason-blind hard ceiling. The reflection gate below is
		// guidance — it asks the model to explain itself before repeating,
		// which works often enough to keep. But guidance must not be able to
		// delete the gate: once a model learns the `_reason` key it passes
		// unconditionally, forever, and that is what happened on 4fab96f_3
		// (24 identical reads in one step, no check fired). This branch looks
		// at the observed answer instead, which the model cannot rephrase.
		// See docs/54 §5.18 and §5.20.
		// Counted on (canonical args, result) — not on args alone. Identical
		// arguments with a changing answer is a retry (token refresh, re-read
		// after a write); identical arguments with an identical answer is the
		// degenerate case worth stopping.
		if hardCap := s.dedupHardCap(); state.resultStreak[callKey] >= hardCap {
			observability.GetGlobalMetrics().Inc("orchestrator.dedup.hard_cap", 1)
			s.logger.Log("EventDedupHardCap", req.TaskID, req.StepID, map[string]any{
				"tool":        call.Name,
				"call_n":      callCount + 1,
				"same_result": state.resultStreak[callKey],
				"hard_cap":    hardCap,
				"args_hash":   callKey,
			})
			state.toolResults = append(state.toolResults, fmt.Sprintf(
				"[%s] Blocked: this exact call — same tool, same arguments — has now run %d times "+
					"in this step and returned the same data every time. A `_reason` does not lift "+
					"this limit; re-asking cannot produce new information.\n"+
					"Do one of these instead:\n"+
					"  1. Change the query. A different filter, id, or page index that will actually "+
					"return different rows — not a rephrasing of the same request.\n"+
					"  2. Act on what you already have. The data is in the results above; take the "+
					"next step the task requires.\n"+
					"  3. If the call keeps failing, stop calling it and report the failure.",
				call.Name, callCount+1))
			continue
		}

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

		// Role-scoped core tools (ADDED 2026-10-19). A role may declare
		// core_tools to narrow the built-in surface; task_planner declares [] so it
		// cannot execute the task it was asked to plan.
		//
		// This check is NOT redundant with the schema filter in buildMCPServers:
		// the model can emit a call for a tool it was never shown (it knows the
		// names from its instruction or from an earlier turn), and the dispatch
		// below keys only on CoreToolNames — so without this, hiding a tool would
		// be cosmetic.
		if CoreToolNames[call.Name] && !s.coreToolPolicyFor(req.RoleID).allows(call.Name) {
			observability.GetGlobalMetrics().Inc("orchestrator.calls.core_tool_refused", 1)
			state.toolResults = append(state.toolResults, fmt.Sprintf(
				"[%s] Refused: role %q is not permitted to use built-in tools in this step. "+
					"Use the role's own MCP tools, or state that the step cannot be completed "+
					"with the capabilities you have.",
				call.Name, req.RoleID))
			continue
		}

		// SharedContext tools (ADDED 2026-10-27): shared_context_write/read
		// are handled directly, like core tools, but require a SharedContext
		// instance from the SpawnRequest. See docs/plan/TYPED_SHARED_CONTEXT_DESIGN.md.
		if SharedContextToolNames[call.Name] && req.SharedContext != nil {
			res, status := handleSharedContextTool(call, req.SharedContext, req.TaskID, req.StepID)
			interaction := store.ToolInteraction{
				ToolName:  call.Name,
				Arguments: call.Arguments,
				Result:    res,
			}
			state.trace = append(state.trace, interaction)
			env := s.processor.Wrap(call.Name, res, status, "orchestrator", "")
			md := env.ToMarkdown(call.Name)
			if status != "error" {
				noteToolResult(state, callKey, md)
			}
			state.toolResults = append(state.toolResults, md)
			s.logger.Log("EventSharedContextToolCall", req.TaskID, req.StepID, map[string]any{
				"tool":   call.Name,
				"status": status,
			})
			continue
		}

		// Core tools (write_file, read_file, execute_code) — handled directly
		if CoreToolNames[call.Name] {
			taskDir := filepath.Join(s.outputBase, req.TaskID)
			validator := NewSafePathValidator(taskDir, req.SessionRoot, s.allowedWorkspaces)
			res, status, interaction := s.handleCoreToolCall(ctx, call, req.TaskID, req.StepID, validator, req.OwnerID)
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
			md := env.ToMarkdown(call.Name)
			if status != "error" {
				noteToolResult(state, callKey, md)
				md = s.steerRepeatedResult(req.TaskID, req.StepID, call.Name, md)
			}
			state.toolResults = append(state.toolResults, md)
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
				// Set system.require_write_reason=true to restore hard blocking (that path emits EventWriteAuthBlocked).
				//
				// A3 (eval FINDINGS.md §A3, 2026-10-22): the R4 comment promised
				// "keep the nudge in the tool result", but the append lived only in
				// the hard-block branch — with require_write_reason=false the nudge
				// went to state.trace (never model-visible) and the model saw only
				// the bare result; v20 logged 15 EventWriteAuthNudged firing into
				// the void. The advisory nudge is now appended to toolResults ahead
				// of the real result (the execution paths below append theirs after).
				// The "do NOT repeat" wording is load-bearing: the call has already
				// been dispatched, and a small model that reads the warning as a
				// complaint would otherwise re-issue the same write (double
				// cancel_/update_). The early trace append also moved into the
				// hard-block branch: it double-booked the advisory path (one call,
				// two trace entries) and recorded the advisory text on the
				// hard-block path, where the model actually saw AUTHORIZATION REQUIRED.
				nudge := fmt.Sprintf(
					"[%s] This call changes external state. Before you rely on the result below, note: "+
						"what is the current state, and is this write necessary? The call is being "+
						"executed without a `_reason`; once the result below arrives, do NOT repeat "+
						"this call — only reflect on whether further writes are needed.",
					call.Name)
				if s.registry != nil && s.registry.System.RequireWriteReason {
					observability.GetGlobalMetrics().Inc("orchestrator.auth.write_blocked", 1)
					s.logger.Log("EventWriteAuthBlocked", req.TaskID, req.StepID, map[string]any{
						"tool": call.Name,
						"mcp":  mcpID,
					})
					nudge = fmt.Sprintf(
						"[%s] AUTHORIZATION REQUIRED — this call changes external state.\n"+
							"Add a top-level key named \"_reason\" whose value is one short sentence.\n"+
							"Call it again with this exact shape:\n"+
							"  {\"reservation_id\": \"<the id>\", \"_reason\": \"<why this write is correct>\"}",
						call.Name)
					interaction.Result = nudge
					state.trace = append(state.trace, interaction)
					env := s.processor.Wrap(call.Name, nudge, "error", "orchestrator", reason)
					state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
					continue
				}
				interaction.Result = nudge
				env := s.processor.Wrap(call.Name, nudge, "ok", "orchestrator", reason)
				state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
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

		// Pre-flight tool-name validation. A name the target MCP does not serve is
		// forwarded by ResolveToolMCP's Tier 3 and comes back as an empty body; the
		// model then has no way to distinguish "this tool does not exist" from "the
		// service is down". Say which it is, and name the near misses.
		if msg := s.unknownToolFeedback(call.Name, mcpID, mcpDef); msg != "" {
			observability.GetGlobalMetrics().Inc("orchestrator.calls.unknown_tool", 1)
			s.logger.Log("EventUnknownTool", req.TaskID, req.StepID, map[string]any{
				"tool": call.Name,
				"mcp":  mcpID,
			})
			state.toolFailCount[call.Name]++
			effCount := s.incrTaskToolFail(req.TaskID, call.Name, state.toolFailCount[call.Name])
			res := toolFailFeedback(call.Name, msg, effCount)
			interaction.Result = res
			state.trace = append(state.trace, interaction)
			env := s.processor.Wrap(call.Name, res, "error", "orchestrator", reason)
			state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
			continue
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
				noteToolResult(state, callKey, cachedMD)
				cachedMD = s.steerRepeatedResult(req.TaskID, req.StepID, call.Name, cachedMD)
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
					s.logSERFFailureClass(req.TaskID, req.StepID, call.Name, serf)
					interaction.Result = recoveryMsg
					env := s.processor.Wrap(call.Name, recoveryMsg, "error", "mcp:"+mcpDef.ID, reason)
					state.toolResults = append(state.toolResults, env.ToMarkdown(call.Name))
				} else {
					state.toolFailCount[call.Name] = 0
					interaction.Result = finalRes
					interaction.Meta = extractMeta(finalRes) // Phase 2: extract structured metadata
					if att, ok := finalRes.(schemas.SubagentOutput); ok {
						state.turnAttachments = dedupAttachments(state.turnAttachments, att.Attachments)
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
					if len(md) > s.processor.MaxBodyChars {
						md = s.sieve.PruneOutput(md, req.Task)
					}
					// #2-universal (eval §23): cache the successful read result
					// for replay on identical future calls within this server.
					// Cache the clean md before any repetition warning is prepended.
					if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
						s.domainReadCache.Put(mcpID, call.Name, call.Arguments, md)
					}
					// Egress redaction (I5) + L1 injection marking (input_normalization.go).
					// Output bounding: adaptive truncation for oversized results.
					md = redactAndMarkUntrusted(md, req.OwnerID)
					if len(md) > s.processor.MaxBodyChars {
						md = string(skeleton.AdaptiveOutputResult([]byte(md), call.Name, env.Source))
					}
					noteToolResult(state, callKey, md)
					md = s.steerRepeatedResult(req.TaskID, req.StepID, call.Name, md)
					state.toolResults = append(state.toolResults, md)
					if isStickyResult(call.Name) {
						state.stickyResults = append(state.stickyResults, md)
					}
				}
			}
		} else if mcpDef.URL != "" {
			toolCtx, cancelTool := context.WithTimeout(ctx, 60*time.Second)
			finalRes, execErr := s.mcpMgr.callRemoteMCPTool(toolCtx, mcpDef, call.Name, call.Arguments, req.OwnerID)
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
					s.logSERFFailureClass(req.TaskID, req.StepID, call.Name, serf)
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
					if len(md) > s.processor.MaxBodyChars {
						md = s.sieve.PruneOutput(md, req.Task)
					}
					// #2-universal (eval §23): cache the successful read result.
					// Cache the clean md before any repetition warning is prepended.
					if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
						s.domainReadCache.Put(mcpID, call.Name, call.Arguments, md)
					}
					// Egress redaction (I5) + L1 injection marking (mirrors the local branch).
					md = redactAndMarkUntrusted(md, req.OwnerID)
					if len(md) > s.processor.MaxBodyChars {
						md = string(skeleton.AdaptiveOutputResult([]byte(md), call.Name, env.Source))
					}
					noteToolResult(state, callKey, md)
					md = s.steerRepeatedResult(req.TaskID, req.StepID, call.Name, md)
					state.toolResults = append(state.toolResults, md)
					if isStickyResult(call.Name) {
						state.stickyResults = append(state.stickyResults, md)
					}
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
