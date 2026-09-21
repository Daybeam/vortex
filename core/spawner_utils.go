package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// formatForLog renders a tool result for diagnostic logging. Tool results
// are commonly map[string]any (JSON-decoded responses), for which
// fmt.Sprintf("%v", ...) produces Go's default map-printing format
// ("map[key:val key2:val2]") -- readable for small maps but loses nesting
// structure and quoting for anything non-trivial. Prefer json.Marshal for
// map/slice-shaped values so diagnostic logs (e.g. InfiniteLoopDetected's
// loop_detail) are actually inspectable; fall back to %v for everything
// else (strings, errors, scalars) where %v is already the natural format.

// formatForLog renders a tool result for diagnostic logging. Tool results
// are commonly map[string]any (JSON-decoded responses), for which
// fmt.Sprintf("%v", ...) produces Go's default map-printing format
// ("map[key:val key2:val2]") -- readable for small maps but loses nesting
// structure and quoting for anything non-trivial. Prefer json.Marshal for
// map/slice-shaped values so diagnostic logs (e.g. InfiniteLoopDetected's
// loop_detail) are actually inspectable; fall back to %v for everything
// else (strings, errors, scalars) where %v is already the natural format.
func formatForLog(v any) string {
	switch v.(type) {
	case map[string]any, []any:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return fmt.Sprintf("%v", v)
}

func (s *Spawner) failedOutput(capability, reason string) *SpawnResult {
	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:      schemas.StatusFailed,
			Confidence:  0.1, // Non-zero to distinguish from uninitialized
			Result:      map[string]any{"error": reason},
			Assumptions: []string{"infrastructure_failure: " + reason},
			Warnings:    []string{reason},
			Capability:  capability,
		},
	}
}

func (s *Spawner) resolveCandidateConfig(hub *ContextHub, role *config.Role, providerID, model string) *config.ProviderConfig {
	pID := providerID
	if pID == "" {
		if role != nil {
			pID = role.Provider
		}
		if pID == "" {
			// Tiered Default: Session Main -> Global Default
			if hub.Graph != nil && hub.Graph.MainProviderID != "" {
				pID = hub.Graph.MainProviderID
			} else {
				pID = hub.Registry.DefaultProvider
			}
		}
	}

	pc := hub.GetProviderConfig(pID)

	if pc == nil {
		// If Registry is also empty, we return a virtual "host" provider
		// to allow delegation back to the caller. ADDED (2026-07-25).
		hub.Registry.Mu.RLock()
		provCount := len(hub.Registry.Providers)
		hub.Registry.Mu.RUnlock()
		if provCount == 0 {
			return &config.ProviderConfig{
				Provider: "host",
				Model:    "delegated-host-model",
			}
		}
		// Final Fallback: use global registry's hardwired defaults if pID resolve failed
		return hub.Registry.ResolveProviderConfig(role)
	}

	if model != "" && model != pc.Model {
		clone := *pc
		clone.Model = model
		return &clone
	}
	return pc
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var rlErr *providers.RateLimitError
	if errors.As(err, &rlErr) {
		return true
	}
	var pErr *providers.ProviderError
	if errors.As(err, &pErr) {
		// 5xx errors are retryable
		if strings.Contains(pErr.Msg, "HTTP 5") {
			return true
		}
	}
	// Timeouts and connection errors
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "connection refused") {
		return true
	}
	// Multimodal fallback trigger: if model doesn't support images/vision, try next candidate.
	// ADDED (2026-07-24).
	if strings.Contains(errStr, "not support images") || strings.Contains(errStr, "images are not supported") || strings.Contains(errStr, "multimodal") {
		return true
	}
	// General fallback trigger: if provider is misconfigured or down, try next candidate.
	// ADDED (2026-07-25).
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "403") || strings.Contains(errStr, "api key") || strings.Contains(errStr, "not found") {
		return true
	}
	return false
}

func isStateChangingTool(name string) bool {
	stateTools := map[string]bool{
		"click":          true,
		"type":           true,
		"navigate":       true,
		"press_key":      true,
		"scroll":         true,
		"set_cookie":     true,
		"execute_script": true,
	}
	return stateTools[name]
}

func containsTool(tools []string, target string) bool {
	for _, t := range tools {
		if t == target {
			return true
		}
	}
	return false
}

// flattenContentBlocks joins the Text of each ContentBlock with double newlines,
// producing a plain string for providers that do not support block-based
// prompt caching (Gemini, OpenAI-compatible, Ollama, etc). Anthropic-style
// providers should prefer the SystemBlocks/UserBlocks fields directly; this
// is a compatibility fallback so CompleteRequest.System/User are never left
// empty now that buildSystemPrompt returns []schemas.ContentBlock.
func flattenContentBlocks(blocks []schemas.ContentBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// copyToolCallArguments creates a shallow copy of src and optionally stamps a
// _decision_id into the copy (not the original). This isolates the trace-only
// metadata from the arguments that get sent to the MCP server, so _decision_id
// never leaks into an actual tool call payload (fix M2).
func copyToolCallArguments(src map[string]any, decisionID string) map[string]any {
	dst := make(map[string]any, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	if decisionID != "" {
		dst["_decision_id"] = decisionID
	}
	return dst
}

// toolFailFeedback wraps a tool error with recovery guidance for the LLM.
// After 3 consecutive failures, it switches to a circuit-breaker message
// telling the LLM to stop using that tool. Mirrors chat_harness.go's pattern.
// Moved from spawner.go during god-class split.
func toolFailFeedback(toolName, errMsg string, failCount int) string {
	if failCount >= 3 {
		return fmt.Sprintf("[TOOL %s HAS FAILED %d TIMES] Stop using this tool. Use an alternative approach or answer directly without tools.", toolName, failCount)
	}
	return fmt.Sprintf("[TOOL FAILED: %s] %s\nDo not abort. Either fix the arguments and retry, use a different tool, or answer directly.", toolName, errMsg)
}

// recordCapabilityOutcome feeds execution telemetry to CapabilityProfileStore
// for Pareto-aware routing and IRT theta estimation. Best-effort: errors are
// logged but never propagate to the caller.
// Moved from spawner.go during god-class split.
func (s *Spawner) recordCapabilityOutcome(ctx context.Context, req *SpawnRequest, result *SpawnResult, latency time.Duration) {
	if s.capProfileStore == nil || result.ModelID == "" {
		return
	}
	capability := result.Output.Capability
	if capability == "" {
		return
	}
	success := result.Output.Status == schemas.StatusOK || result.Output.Status == schemas.StatusPartial
	tokenCost := float64(result.Output.TokenUsed)
	if err := s.capProfileStore.RecordOutcome(ctx, result.ModelID, capability, success, float64(result.TurnsUsed), float64(latency.Milliseconds()), tokenCost); err != nil {
		s.logger.Log("cap_profile_record_error", req.TaskID, req.StepID, map[string]any{"err": err.Error()})
	}
}

// gatherToolFewShots collects Few-Shot examples from the routed MCP bindings.
// For each binding, it checks the MCP definition's FewShots map for examples
// matching the binding's allowed tools (or all available tools if unrestricted).
// Extracted from spawner.go doSpawn during god-class split.
func gatherToolFewShots(hub *ContextHub, bindings []config.MCPBinding) []string {
	var toolFewShots []string
	for _, b := range bindings {
		mcp := hub.GetMCP(b.MCPID)
		if mcp == nil || mcp.FewShots == nil {
			continue
		}

		// Use binding list if restricted, otherwise use all available tools from MCP definition
		available := b.AllowedTools
		if !b.IsRestricted() {
			available = mcp.AvailableTools
		}

		for _, tool := range available {
			if examples, ok := mcp.FewShots[tool]; ok {
				for _, ex := range examples {
					argsJSON, _ := json.Marshal(ex.ToolCall)
					toolFewShots = append(toolFewShots, fmt.Sprintf("User Intent: %s\nTool Call: %s", ex.UserIntent, string(argsJSON)))
				}
			}
		}
	}
	return toolFewShots
}

// resolveContextRefs resolves context references from the task store KV.
// Each ref is "taskID:stepID" — the stored result's Data is injected into
// the context map under the ref's name key.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) resolveContextRefs(ctx context.Context, req *SpawnRequest) map[string]any {
	resolvedCtx := make(map[string]any)
	for name, ref := range req.ContextRefs {
		if ref == "" {
			continue
		}
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) == 2 {
			if result := s.taskStoreGet(ctx, parts[0], parts[1]); result != nil {
				resolvedCtx[name] = result.Data
			}
		}
	}
	return resolvedCtx
}

// fetchDynamicContext collects real-time context from all registered
// ContextProviders. Errors are logged but never abort the spawn — a
// failing provider simply contributes no data to the merged context.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) fetchDynamicContext(ctx context.Context, taskID, stepID string) map[string]any {
	envContext := make(map[string]any)
	for _, cp := range s.contextProviders {
		if c, err := cp.FetchContext(ctx); err == nil {
			envContext[cp.Name()] = c
		} else {
			s.logger.Log(EventStepFailed, taskID, stepID, map[string]any{
				"error":   fmt.Sprintf("Failed to fetch context from %s", cp.Name()),
				"details": err.Error(),
			})
		}
	}
	return envContext
}

// validateCapabilities performs the Phase 2 capability manifest pre-flight
// validation. Returns nil if the role's required capabilities are satisfied
// (or if dynamic expansion is enabled). Returns an error if the role is
// blocked by missing capabilities that cannot be dynamically generated.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) validateCapabilities(ctx context.Context, req *SpawnRequest, role *config.Role) error {
	if len(role.Requires) == 0 {
		return nil
	}
	missing := req.Hub.Registry.CheckCapabilities(role.Requires)
	if len(missing) == 0 {
		return nil
	}
	missingStr := strings.Join(missing, ", ")

	// FIX (2026-08-06): If the role allows dynamic expansion, downgrade the error to a warning.
	// This allows the agent to attempt the task using existing tools or to generate/find
	// the required capabilities during execution, avoiding premature blocking.
	if role.AllowDynamicSkills || role.AllowDynamicMCPs {
		s.logger.LogCtx(ctx, "EventCapabilityWarning", req.TaskID, req.StepID, map[string]any{
			"role":    req.RoleID,
			"missing": missing,
			"message": fmt.Sprintf("Role %q is missing required capabilities [%s], but has dynamic expansion enabled. Proceeding with caution.", req.RoleID, missingStr),
		})
		return nil
	}

	s.logger.LogCtx(ctx, EventTaskFailed, req.TaskID, req.StepID, map[string]any{
		"role":    req.RoleID,
		"missing": missing,
		"error":   "Missing required capabilities",
	})
	if role.Generatable {
		return fmt.Errorf("DYNAMIC_ESCALATION_REQUIRED: missing required capabilities [%s]", missingStr)
	}
	return fmt.Errorf("Blocked: role %q requires capabilities [%s] which are not available in the current environment", req.RoleID, missingStr)
}

// retrieveDynamicSkills performs Phase 0 skill retrieval (RAG for Skills).
// If the role allows dynamic skills, it queries the experience store for
// relevant autogenerated skills and appends them to the request's
// AdditionalSkills. Returns the combined skill list.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) retrieveDynamicSkills(ctx context.Context, req *SpawnRequest, role *config.Role) []string {
	additionalSkills := req.AdditionalSkills
	// FIX (2026-08-16/17, playbook addendum): s.expStore is declared as the
	// interface store.IExperienceStore, not a concrete pointer type -- a
	// nil-receiver guard inside the method itself (added in
	// store/experience.go) does NOT protect against a genuinely nil
	// interface value here, since Go panics at the call site trying to look
	// up the method on a nil interface, before the method body ever runs.
	// Several existing tests (and possibly real callers) construct a
	// Spawner via NewSpawner(..., nil, ...), leaving expStore as a true nil
	// interface -- reproduced live via TestSpawner_MultimodalFallbackDetection.
	if role.AllowDynamicSkills && s.expStore != nil {
		relevant := s.expStore.QueryRelevantSkills(ctx, req.Task, 2)
		if len(relevant) > 0 {
			additionalSkills = append(additionalSkills, relevant...)
			s.logger.Log(EventStepStarted, req.TaskID, req.StepID, map[string]any{
				"action": "auto_retrieved_skills",
				"skills": relevant,
			})
		}
	}
	return additionalSkills
}

// collectToolConstraints gathers the flat list of allowed tool names from
// restricted MCP bindings for prompt injection.
// Extracted from spawner.go doSpawn during god-class split.
func collectToolConstraints(bindings []config.MCPBinding) []string {
	var toolConstraints []string
	for _, b := range bindings {
		if b.IsRestricted() {
			toolConstraints = append(toolConstraints, b.AllowedTools...)
		}
	}
	return toolConstraints
}

// applyRefBasedHandoff implements the Anti-Cliff ref-based handoff strategy
// (arXiv:2608.22752). If the resolved upstream context exceeds the model's
// adaptive threshold, it side-loads the context into an artifact file and
// injects only a pointer + summary (never the raw payload), preserving
// subagent lazy-load access. Otherwise, it injects the raw context inline.
// Returns the (possibly modified) userText.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) applyRefBasedHandoff(ctx context.Context, req *SpawnRequest, role *config.Role, userText string, resolvedCtx map[string]any) string {
	if len(resolvedCtx) == 0 {
		return userText
	}
	ctxBytes, _ := json.Marshal(resolvedCtx)
	ctxLen := len(ctxBytes)

	// Resolve the model's MaxContextWindow for dynamic threshold (ADDED 2026-08-27):
	injectionPCfg := s.resolveCandidateConfig(req.Hub, role, req.ProviderOverride, "")

	// Dynamic adaptive threshold based on model's MaxContextWindow:
	effectiveThreshold := s.GetEffectiveHandoffThreshold(injectionPCfg)

	if s.assets != nil && effectiveThreshold > 0 && ctxLen > effectiveThreshold {
		file, err := s.assets.Handle(req.TaskID, req.StepID, ctxBytes, "json", "upstream_context")
		if err == nil && file != nil {
			refPointer := map[string]any{
				"ref":     file.Path,
				"size":    ctxLen,
				"summary": "Upstream context side-loaded (adaptive model threshold); use read_file to inspect.",
				"step":    req.StepID,
				"task":    req.TaskID,
			}
			ptrBytes, _ := json.Marshal(refPointer)
			userText = fmt.Sprintf("%s\n\n### Upstream Context (ref-based)\n%s", req.Task, string(ptrBytes))
			s.logger.Log("EventUpstreamContextSideLoaded", req.TaskID, req.StepID, map[string]any{
				"path":      file.Path,
				"bytes":     ctxLen,
				"threshold": effectiveThreshold,
				"model_max": func() int {
					if injectionPCfg != nil {
						return injectionPCfg.MaxContextWindow
					}
					return 0
				}(),
			})
		} else {
			userText = fmt.Sprintf("%s\n\n### Upstream Context\n%s", req.Task, string(ctxBytes))
		}
	} else {
		userText = fmt.Sprintf("%s\n\n### Upstream Context\n%s", req.Task, string(ctxBytes))
	}
	return userText
}

// tryDelegationMode handles the Subagent Delegation Mode (Zero-Config Cold
// Start, ADDED 2026-08-19). If delegation mode is enabled in the system
// config, it builds a delegation-required SpawnResult and returns it with
// handled=true. Otherwise returns (nil, false) and the caller continues
// with the normal tool execution loop.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) tryDelegationMode(ctx context.Context, req *SpawnRequest, role *config.Role, prunedSkills []string, toolConstraints []string, mergedContext map[string]any, toolFewShots []string, userText string, bindings []config.MCPBinding) (*SpawnResult, bool) {
	if !s.registry.System.DelegationMode {
		return nil, false
	}
	pCfg := s.resolveCandidateConfig(req.Hub, role, req.ProviderOverride, "")
	// Defect 3 fix: redact sensitive context in delegation mode
	delegationCtx := redactContextForDelegation(mergedContext)
	delegationPrecedents := []*schemas.DecisionNode{}
	systemBlocks, err := s.buildSystemPrompt(req.Hub, role, prunedSkills, role.BaseCapability, pCfg, toolConstraints, delegationCtx, toolFewShots, req.Isolation, delegationPrecedents, req.Task, "")
	if err != nil {
		return nil, false
	}
	s.logger.Log(EventStepStarted, req.TaskID, req.StepID, map[string]any{
		"action": "delegation_triggered",
		"reason": "system_delegation_mode_enabled",
	})
	// Defect 1 fix: include tool definitions in delegation return
	toolDefs := s.collectDelegationToolDefs(req.Hub, bindings)
	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusDelegationRequired,
			Confidence: 1.0,
			Result: map[string]any{
				"system_prompt":     flattenContentBlocks(systemBlocks),
				"user_prompt":       userText,
				"target_capability": role.BaseCapability,
				"role_id":           req.RoleID,
				"tool_definitions":  toolDefs,
			},
			Capability: role.BaseCapability,
		},
	}, true
}

// spawnCandidate represents a provider+model candidate for the spawn's
// provider fallback chain. Moved from a local type in doSpawn during
// god-class split.
type spawnCandidate struct {
	providerID string
	model      string
}

// buildCandidateConfigs constructs the provider fallback chain for a spawn
// request. It starts with the primary provider, adds a multimodal fallback
// if the primary doesn't support vision but the default provider does, adds
// role-configured fallbacks, and finally ensures a session-main fallback
// is always present as a last resort.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) buildCandidateConfigs(ctx context.Context, req *SpawnRequest, role *config.Role) []spawnCandidate {
	var candidateConfigs []spawnCandidate
	// Add primary
	candidateConfigs = append(candidateConfigs, spawnCandidate{providerID: req.ProviderOverride, model: ""})

	// ─── Phase 0.3: Multimodal Fallback Check (Graphify inspired) ─────────
	// If the request is multimodal (has attachments) but the primary model
	// is not known to support vision, automatically insert the DefaultProvider
	// (Session Main) if it supports vision. ADDED (2026-07-24).
	attachments := s.resolveContextAttachments(ctx, req.TaskID, req.StepID)
	primaryPCfg := s.resolveCandidateConfig(req.Hub, role, req.ProviderOverride, "")
	if len(attachments) > 0 && !primaryPCfg.SupportsVisionHeuristic() {
		// audit H1: use locked accessors — the Providers map is mutated in
		// place by the config file-watcher reload, so a direct unlocked
		// r.Providers[r.DefaultProvider] read races and can fatal-panic.
		defaultProviderID := req.Hub.Registry.DefaultProviderName()
		mainPCfg := req.Hub.Registry.GetProvider(defaultProviderID)
		if mainPCfg != nil && mainPCfg.SupportsVisionHeuristic() && req.ProviderOverride != defaultProviderID {
			candidateConfigs = append(candidateConfigs, spawnCandidate{providerID: defaultProviderID, model: ""})
			s.logger.Log("EventMultimodalFallbackPlanned", req.TaskID, req.StepID, map[string]any{
				"primary_model": primaryPCfg.Model,
				"fallback_main": mainPCfg.Model,
				"reason":        "multimodal_input_detected_primary_not_capable",
			})
		}
	}

	// Add fallbacks
	if !role.DisableFallback {
		for _, fb := range role.Fallbacks {
			candidateConfigs = append(candidateConfigs, spawnCandidate{providerID: fb.Provider, model: fb.Model})
		}
	}

	// ─── Phase 0.4: Session Main Fallback (Universal) ───────────────────
	// Ensure there's always a final fallback to the "Session Main" or
	// "Global Default". ADDED (2026-07-25).
	mainProviderID := req.Hub.Registry.DefaultProvider
	if req.Hub.Graph != nil && req.Hub.Graph.MainProviderID != "" {
		mainProviderID = req.Hub.Graph.MainProviderID
	}

	alreadyPresent := false
	for _, c := range candidateConfigs {
		if c.providerID == mainProviderID {
			alreadyPresent = true
			break
		}
		if c.providerID == "" && req.RoleID != "" {
			role := req.Hub.GetRole(req.RoleID)
			if role != nil && role.Provider == mainProviderID {
				alreadyPresent = true
				break
			}
		}
	}
	if !alreadyPresent {
		candidateConfigs = append(candidateConfigs, spawnCandidate{providerID: mainProviderID, model: ""})
		s.logger.Log("EventSessionMainFallbackAdded", req.TaskID, req.StepID, map[string]any{
			"provider_id": mainProviderID,
		})
	}
	return candidateConfigs
}
