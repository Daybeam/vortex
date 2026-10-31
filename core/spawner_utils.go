package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
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

// MinResultBytesForDedup is the minimum joined result size to participate in
// output stagnation detection. Short results (dates, booleans, "OK") repeat
// legitimately and would cause false positives.
const MinResultBytesForDedup = 100

// computeTurnHash computes a hash of the turn's tool calls (name + args) AND
// results for stagnation detection (C3 fix). Returns shouldTrack=false if:
//   - results are too short (< MinResultBytesForDedup): short outputs like
//     dates or "OK" legitimately repeat (e.g., get_current_date, polling).
//   - all results are errors: errors are already tracked by toolFailCount;
//     counting them here double-penalizes.
//
// The hash includes tool call names + arguments so that different calls
// producing the same short result (e.g., two tools returning "OK") don't
// collide — only identical calls + identical results trigger stagnation.
// Control arguments are excluded (canonicalToolArgs) so a rephrased `_reason`
// cannot reset the consecutive-repeat counter.
func computeTurnHash(toolCalls []schemas.ToolCall, toolResults []string) (hash uint64, shouldTrack bool) {
	joined := strings.Join(toolResults, "\n")
	if len(joined) < MinResultBytesForDedup {
		return 0, false
	}
	// Skip if all results are errors — errors are tracked by toolFailCount.
	allErrors := true
	for _, r := range toolResults {
		if !strings.Contains(r, "❌") {
			allErrors = false
			break
		}
	}
	if allErrors {
		return 0, false
	}
	h := fnv.New64a()
	for _, tc := range toolCalls {
		h.Write([]byte(tc.Name))
		b, err := json.Marshal(canonicalToolArgs(tc.Arguments))
		if err != nil {
			b = []byte(fmt.Sprintf("%v", tc.Arguments))
		}
		h.Write(b)
		h.Write([]byte{0}) // separator
	}
	h.Write([]byte(joined))
	return h.Sum64(), true
}

// ── Output-stagnation decision (ADDED 2026-10-23, obs-layer A/B) ──────────
//
// Purely functional so the grace-window change can be proven equivalent to the
// historical behaviour at grace=0 by a unit test rather than by reading the
// control flow. `spawner_dospawn.go` only translates the result into logging
// and side effects.
//
// Historical behaviour (which grace=0 must reproduce exactly):
//
//	repeats <= threshold            → nothing
//	first time over threshold       → hint  (and remember repeats)
//	any later turn over threshold   → kill
//
// With grace=N, the kill is deferred until repeats exceed hintAt+N. The hint
// is never deferred and never re-injected.
type stagnationAction int

const (
	stagNone stagnationAction = iota
	stagHint
	stagGrace
	stagKill
)

func (a stagnationAction) String() string {
	switch a {
	case stagHint:
		return "hint"
	case stagGrace:
		return "grace"
	case stagKill:
		return "kill"
	default:
		return "none"
	}
}

// stagnationStep returns the action for one turn given the consecutive-repeat
// count. `hintAt` is the repeat count at which the hint fired (0 if not yet).
func stagnationStep(repeats, hintAt, grace, threshold int, hinted bool) stagnationAction {
	if repeats <= threshold {
		return stagNone
	}
	if !hinted {
		return stagHint
	}
	if repeats > hintAt+grace {
		return stagKill
	}
	return stagGrace
}

// partialStagnationOutput builds a SpawnResult with StatusPartial that
// preserves accumulated work (tool names, trace count) when the model is
// stuck in output stagnation. Unlike failedOutput, this does not discard
// all prior turn work — downstream steps can inspect what was accomplished.
func (s *Spawner) partialStagnationOutput(capability, reason string, trace []store.ToolInteraction) *SpawnResult {
	toolNames := make([]string, 0, len(trace))
	for _, t := range trace {
		toolNames = append(toolNames, t.ToolName)
	}
	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusPartial,
			Confidence: 0.3, // Low but non-zero: some work was done
			Result: map[string]any{
				"error":           reason,
				"tools_used":      toolNames,
				"tool_call_count": len(trace),
			},
			Assumptions: []string{"stagnation_detected: " + reason},
			Warnings:    []string{reason},
			Capability:  capability,
		},
	}
}

// MaxPartialOutputChars bounds the size of intercepted PartialOutput injected
// into a downstream step's prompt. Without this, a multi-MB intercepted output
// would bloat the prompt and risk context-window overflow.
const MaxPartialOutputChars = 2000

// securityInterceptionReasons are Sieve interception reasons that indicate the
// intercepted content is security-sensitive (injection, PII, secret leak). For
// these, the content must NOT be passed back to the model — doing so would
// propagate the attack. The Sieve currently only produces "repetition" and
// "schema violation" reasons, but this is defense-in-depth for future Sieve
// enhancements that add injection/PII detection.
var securityInterceptionReasons = []string{"injection", "pii", "security", "malicious", "secret_leak"}

// sanitizePartialOutputForPassthrough prepares an intercepted PartialOutput
// for injection into a downstream step's prompt (C2 fix). It:
//  1. Skips passthrough entirely for security-class interception reasons
//     (injection, PII) — sending blocked content back to the model would
//     propagate the attack.
//  2. Applies egress redaction to strip secrets/PII.
//  3. Truncates to MaxPartialOutputChars to bound prompt growth.
//  4. Wraps in untrusted-data delimiters so the model treats it as data,
//     not instructions (prompt injection defense).
//
// Returns empty string if passthrough should be skipped entirely.
func sanitizePartialOutputForPassthrough(partialOutput, interceptionReason, ownerID string) string {
	if partialOutput == "" {
		return ""
	}
	// Security-class reasons: don't pass intercepted content back.
	reasonLower := strings.ToLower(interceptionReason)
	for _, sec := range securityInterceptionReasons {
		if strings.Contains(reasonLower, sec) {
			return ""
		}
	}
	// Egress redaction: strip secrets before sending to model.
	redacted := RedactEgressWithOwner(partialOutput, ownerID)
	// Truncate to bound prompt growth.
	if len(redacted) > MaxPartialOutputChars {
		head := redacted[:MaxPartialOutputChars/2]
		tail := redacted[len(redacted)-MaxPartialOutputChars/2:]
		redacted = fmt.Sprintf("%s\n\n... [TRUNCATED %d chars] ...\n\n%s", head, len(redacted)-MaxPartialOutputChars, tail)
	}
	// Wrap in untrusted-data delimiters (prompt injection defense).
	return fmt.Sprintf("<untrusted_data from=\"sieve_interception\">\n%s\n</untrusted_data>", redacted)
}

// AuditorAllowedTools returns a copy of the parent step's AdditionalMCPs and
// AdditionalToolAllowlists, filtered to exclude write tools. The
// auditor should only have read-only tools — it's verifying, not acting.
// A deep copy is made to avoid mutation leak (shared map/slice reference
// would let auditor mutations affect the parent step and vice versa).
//
// Whitelist approach: MCPs are only included if they
// have an explicit allowlist entry that survives filtering with at least one
// read-only tool. This fixes two bugs:
//  1. MCPs without an allowlist entry were inherited entirely (all write tools).
//  2. MCPs whose allowlist filtered to empty were treated as "no restriction"
//     by validator.go (empty = unrestricted), granting full access.
//
// Now: no allowlist → skipped; empty after filtering → skipped.
func AuditorAllowedTools(parentMCPs []string, parentAllowlists map[string][]string) ([]string, map[string][]string) {
	allowlists := make(map[string][]string, len(parentAllowlists))
	var mcps []string

	for _, mcpID := range parentMCPs {
		tools, hasAllowlist := parentAllowlists[mcpID]
		if !hasAllowlist {
			// No explicit allowlist → can't guarantee read-only → skip (whitelist).
			continue
		}

		filtered := make([]string, 0, len(tools))
		for _, tool := range tools {
			if !IsWriteMCPTool(tool) {
				filtered = append(filtered, tool)
			}
		}
		if len(filtered) == 0 {
			// All tools were write tools → empty allowlist would mean
			// "no restriction" in validator.go → skip this MCP entirely.
			continue
		}
		mcps = append(mcps, mcpID)
		allowlists[mcpID] = filtered
	}
	return mcps, allowlists
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
		// Model-name fallback: escalate_model:MODEL_NAME (e.g.
		// "gpt-4o") passes the model name as providerID. If no
		// provider with that ID exists, search for a provider whose
		// Model field matches before falling back to the role default.
		// Without this, the escalation silently reverts to the role's
		// default provider (e.g. gemini) instead of the requested model.
		// Bug: P0-2 escalate_model silent fallback (2026-10-28).
		if pID != "" {
			if found := hub.Registry.FindProviderByModel(pID); found != nil {
				pc = found
				log.Printf("[Spawner] providerID %q not found, resolved by model name to provider %q (model %q)",
					pID, found.Provider, found.Model)
			}
		}
	}

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

// isContextLimitError detects provider errors caused by the prompt exceeding
// the model's maximum context window. Different providers phrase this differently:
//   - OpenAI:    "context_length_exceeded" / "This model's maximum context length is"
//   - Anthropic: "prompt is too long" / "context too long"
//   - Gemini:    "exceeds the maximum number of tokens"
//   - llama.cpp (new, b11624+): "request (26012 tokens) exceeds the available
//     context size (16384 tokens), try increasing it" + n_prompt_tokens/n_ctx fields
//   - llama.cpp (old, <=b11517 — Kaggle v36a3b batch evidence 2026-10-13): "request
//     (14441 token: exceeds n_ctx=12288" + n_prompt_token/n_ctx fields.
//     🔴 That variant message has no "context"/"tokens" substring, so the old pattern zero-matches →
//     #20 compression never triggers for old llama-server (v36a3b 20/46 tasks had 61 lines of 400
//     all passing straight through to Final failure). Local b11624 tested: the NEW format actually contains "context size".
//   - vLLM/Ollama/LM Studio: same OpenAI-compatible format as llama.cpp
//   - Generic:   "too many tokens" / "context length" / "context size"
//
// Used by #20 (eval §8.46): reactive context compression — when the provider
// returns this error, we compress the conversation history and retry once
// instead of failing the step or falling through to the next candidate.
//
// Regression: eval §8.48.7 — the original 8 cases all missed llama.cpp's real
// 400 text "exceeds the available context size", so #20 never triggered for
// any OpenAI-compatible local server. Added "context size" to fix this.
// Regression: eval §9.3 (2026-10-16) — that fix only covered the NEW llama.cpp
// wording; the OLD wording ( Kaggle v36a3b evidence, message "request (N token:
// exceeds n_ctx=M", fields n_prompt_token/n_ctx) still matched nothing, so the
// 400 cascade death chain survived the §8.48.7 fix. Added "n_ctx" /
// "n_prompt_tokens" (both formats always carry the n_ctx JSON field, and the
// engine's SanitizeError stars the message but keeps the fields).
func isContextLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "context_length_exceeded") ||
		strings.Contains(s, "context length") ||
		strings.Contains(s, "context size") ||
		strings.Contains(s, "n_ctx") ||
		strings.Contains(s, "n_prompt_tokens") ||
		strings.Contains(s, "maximum context") ||
		strings.Contains(s, "prompt is too long") ||
		strings.Contains(s, "context too long") ||
		strings.Contains(s, "too many tokens") ||
		strings.Contains(s, "exceeds the maximum number of tokens") ||
		strings.Contains(s, "max_tokens")
}

// contextCompressResult reports what compressUserBlocksToBudget did, so the
// caller can emit exactly one event whether or not anything was dropped.
// A silent no-op is indistinguishable from "the guard never ran" — that is
// precisely how three previous keyword fixes (eval §8.48.7 / §9.3) shipped
// without ever being exercised (铁律 51-3).
type contextCompressResult struct {
	Blocks    []schemas.ContentBlock
	Before    int
	After     int
	BeforeTok int
	AfterTok  int
	Dropped   int
	Changed   bool
	Reason    string // "" when Changed; otherwise why nothing was dropped
}

const (
	contextCompressKeepFirst = 2
	contextCompressKeepLast  = 6
	// truncatePasses bounds the truncate-the-biggest-block loop. Each pass
	// halves the block, so 6 passes is a 64x reduction — far beyond any real
	// prompt, and a hard stop against an oscillating estimator.
	truncatePasses = 6
)

// contextInputBudget returns the number of prompt tokens the history must fit
// into before the next provider call: window minus the reply reserve minus this
// call's max_tokens, with a safety margin.
//
// The margin is not decoration. providers.EstimateTokens is known to UNDERSTATE
// JSON/schema-heavy prompts by >=17.6% (铁律 15), so a request trimmed to fit
// "exactly" still 400s. 0.85 is the smallest margin that covers that measured
// gap; tightening it needs a fresh measurement, not a guess.
func (s *Spawner) contextInputBudget(pCfg *config.ProviderConfig, req *SpawnRequest) int {
	if pCfg == nil {
		return 0
	}
	win := pCfg.MaxContextWindow
	if win <= 0 {
		return 0
	}
	reserve := pCfg.GetReserveTokens()
	if out := s.spawnOutputBudget(req); reserve < out {
		reserve = out
	}
	budget := win - reserve
	if budget <= 0 {
		return 0
	}
	return budget * 85 / 100
}

// estimateBlocksTokens sums the provider-facing estimate for every block.
// Uses providers.EstimateTokens (CJK-aware) rather than len(text)/4 so the
// budget check shares one unit with the rest of the engine (铁律 15).
func estimateBlocksTokens(blocks []schemas.ContentBlock) int {
	total := 0
	for i := range blocks {
		total += providers.EstimateTokens(blocks[i].Text)
	}
	return total
}

// truncateBlockText keeps the head and the tail of text and replaces the
// middle with the compression marker, cutting on rune (not byte) boundaries so
// CJK payloads never produce invalid UTF-8.
func truncateBlockText(text string, keepChars int) string {
	r := []rune(text)
	if len(r) <= keepChars {
		return text
	}
	if keepChars < 0 {
		keepChars = 0
	}
	head := keepChars * 6 / 10
	tail := keepChars - head
	return string(r[:head]) + "\n" + MarkerHistoryCompressed + "\n" + string(r[len(r)-tail:])
}

// compressUserBlocksToBudget shrinks the conversation history so that its
// estimated token count fits budgetTokens. It always returns a usable slice.
//
// History (铁律 51, 2026-10-27): the previous compressUserBlocks keyed on BLOCK
// COUNT — `len(blocks) <= 10 ⇒ return nil` — and the caller treated nil as
// "nothing to do". Two failures followed, both observed on aw19:
//   - Squeeze collapsed userBlocks to a single block, so len == 1 and this
//     function returned nil on EVERY context-limit 400; the step then died
//     with no event at all (EventContextCompressed count = 0 for 51 turns);
//   - even when it did fire it dropped by position, not by size, so one huge
//     tool-result block could survive and the retry 400'd again.
//
// The budget version fixes both: it never returns nil, it drops middle blocks
// until the estimate fits, and if the retained set still does not fit it
// truncates the largest retained block.
func compressUserBlocksToBudget(blocks []schemas.ContentBlock, budgetTokens int) contextCompressResult {
	if blocks == nil {
		blocks = []schemas.ContentBlock{} // never hand the caller a nil slice
	}
	res := contextCompressResult{Blocks: blocks, Before: len(blocks), After: len(blocks)}
	res.BeforeTok = estimateBlocksTokens(blocks)
	res.AfterTok = res.BeforeTok

	if len(blocks) == 0 {
		res.Reason = "empty"
		return res
	}

	// No budget information (MaxContextWindow unset): fall back to the legacy
	// positional rule so behaviour only ever loosens, never silently disables.
	if budgetTokens <= 0 {
		if len(blocks) <= contextCompressKeepFirst+contextCompressKeepLast {
			res.Reason = "no_budget"
			return res
		}
		out := make([]schemas.ContentBlock, 0, contextCompressKeepFirst+contextCompressKeepLast+1)
		out = append(out, blocks[:contextCompressKeepFirst]...)
		out = append(out, schemas.ContentBlock{Text: MarkerHistoryCompressed})
		out = append(out, blocks[len(blocks)-contextCompressKeepLast:]...)
		res.Blocks = out
		res.After = len(out)
		res.Dropped = len(blocks) - len(out) + 1 // +1: the marker is not history
		res.AfterTok = estimateBlocksTokens(out)
		res.Changed = true
		return res
	}

	if res.BeforeTok <= budgetTokens {
		res.Reason = "under_budget"
		return res
	}

	// Drop the middle, keep the first (task framing) and last (recency) blocks.
	if len(blocks) > contextCompressKeepFirst+contextCompressKeepLast {
		out := make([]schemas.ContentBlock, 0, contextCompressKeepFirst+contextCompressKeepLast+1)
		out = append(out, blocks[:contextCompressKeepFirst]...)
		out = append(out, schemas.ContentBlock{Text: MarkerHistoryCompressed})
		out = append(out, blocks[len(blocks)-contextCompressKeepLast:]...)
		res.Dropped = len(blocks) - (contextCompressKeepFirst + contextCompressKeepLast)
		res.Blocks = out
		res.After = len(out)
		res.AfterTok = estimateBlocksTokens(out)
		res.Changed = true
	}

	// Still over budget: shrink the largest retained block. This is the case the
	// positional rule could never handle — one fat tool result.
	//
	// The keep-length is found by binary search on the rune length rather than a
	// single halving pass: truncateBlockText also inserts the marker (~18
	// tokens), so a proportional cut can land just over the budget and the
	// marker cost then makes "cut more" oscillate. Searching the largest length
	// whose estimate still fits is exact and terminates.
	for pass := 0; pass < truncatePasses && res.AfterTok > budgetTokens; pass++ {
		idx, biggest := -1, 0
		for i := range res.Blocks {
			if res.Blocks[i].Text == MarkerHistoryCompressed {
				continue
			}
			if n := providers.EstimateTokens(res.Blocks[i].Text); n > biggest {
				idx, biggest = i, n
			}
		}
		if idx < 0 || biggest == 0 {
			break
		}
		others := res.AfterTok - biggest
		allowed := budgetTokens - others // token budget left for this block
		if allowed < 0 {
			allowed = 0
		}
		orig := res.Blocks[idx].Text
		runes := []rune(orig)
		lo, hi := 0, len(runes)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if providers.EstimateTokens(truncateBlockText(orig, mid)) <= allowed {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		newText := truncateBlockText(orig, lo)
		if providers.EstimateTokens(newText) >= biggest {
			break // no progress — stop rather than spin
		}
		res.Blocks[idx].Text = newText
		res.Changed = true
		res.AfterTok = estimateBlocksTokens(res.Blocks)
	}

	if !res.Changed {
		res.Reason = "no_middle"
	}
	return res
}

// toolFailCircuitBreakerThreshold is the number of failures (step-level or
// task-level, whichever is higher) at which toolFailFeedback switches to the
// circuit-breaker message and the engine treats the failure as deterministic
// rather than generative (E2/E3 fix).
const toolFailCircuitBreakerThreshold = 3

// toolFailFeedback wraps a tool error with recovery guidance for the LLM.
// After 3 consecutive failures, it switches to a circuit-breaker message
// telling the LLM to stop using that tool. Mirrors chat_harness.go's pattern.
// Moved from spawner.go during god-class split.
func toolFailFeedback(toolName, errMsg string, failCount int) string {
	if failCount >= toolFailCircuitBreakerThreshold {
		return fmt.Sprintf(ToolFailRepeatedFmt, toolName, failCount)
	}
	return fmt.Sprintf(ToolFailOnceFmt, toolName, errMsg)
}

// buildSERFRecovery constructs a structured recovery message from SERF error
// metadata. Replaces the pure string concatenation of toolFailFeedback when
// MCP tools return isError:true with SERF-parseable content.
//
// If serf is nil (no SERF metadata found), falls back to the original
// toolFailFeedback behavior.
func buildSERFRecovery(toolName string, serf *SERFError, errMsg string, failCount int) string {
	if failCount >= toolFailCircuitBreakerThreshold {
		return fmt.Sprintf(ToolFailRepeatedFmt, toolName, failCount)
	}

	if serf == nil {
		return fmt.Sprintf(ToolFailOnceFmt, toolName, errMsg)
	}

	var advice string
	switch serf.Category {
	case "INVALID_INPUT":
		advice = "The arguments were invalid. Check the tool's input schema and retry with corrected parameters."
	case "RESOURCE_NOT_FOUND":
		advice = "The requested resource was not found. Verify the resource identifier (e.g. project_id, service_name) and retry, or ask the user to confirm the target."
	case "RESOURCE_EXHAUSTED":
		if serf.RetryAfterMs != nil {
			advice = fmt.Sprintf("Rate limit reached. Wait %dms before retrying. If persistent, switch to an alternative resource.", *serf.RetryAfterMs)
		} else {
			advice = "Rate limit reached. Wait before retrying, or switch to an alternative resource."
		}
	case "PERMISSION_DENIED":
		advice = "Permission denied. Do not retry with the same credentials. Escalate to the user to provide alternative access."
	case "UPSTREAM_FAILURE":
		if serf.Retryable {
			advice = "Upstream service is temporarily unavailable. Retry with backoff."
		} else {
			advice = "Upstream service failure. Switch to an alternative resource or escalate."
		}
	case "INTERNAL_ERROR":
		advice = "Internal server error. Do not retry — escalate to the user."
	default:
		advice = fmt.Sprintf("Tool error: %s. Consider fixing arguments, switching tools, or answering directly.", serf.RawMessage)
	}

	// M-7 (2026-10-04): Use strings.Builder instead of repeated += allocation.
	var sb strings.Builder
	sb.WriteString(advice)
	for i, action := range serf.SuggestedActions {
		switch action.Type {
		case "SWITCH_RESOURCE":
			field, _ := action.Params["field"].(string)
			if field != "" {
				fmt.Fprintf(&sb, "\n[OPTION %d]: Try switching %s to a different value.", i+1, field)
			}
		case "ESCALATE_TO_USER":
			fmt.Fprintf(&sb, "\n[OPTION %d]: %s", i+1, action.Message)
		case "RETRY":
			fmt.Fprintf(&sb, "\n[OPTION %d]: Retry the call with adjusted parameters.", i+1)
		}
	}

	return fmt.Sprintf(ToolFailSERFFmt, toolName, serf.Category, sb.String())
}

// RepeatedToolFailure returns the tool with the highest task-level failure
// count for the task, so callers can decide whether a step failure is a
// deterministic/environmental tool refusal rather than generative uncertainty.
// Returns ("", 0) when the task has no recorded failures.
func (s *Spawner) RepeatedToolFailure(taskID string) (string, int) {
	s.taskFailMu.Lock()
	defer s.taskFailMu.Unlock()
	tool, max := "", 0
	for name, n := range s.taskToolFailCount[taskID] {
		if n > max {
			tool, max = name, n
		}
	}
	return tool, max
}

// incrTaskToolFail increments the task-level tool failure count and returns
// the effective count = max(stepCount, taskCount). The step-level count
// (toolFailCount local var in doSpawn) resets per step; this task-level
// counter persists across steps within the same task, so a tool failing
// in step 1, 2, and 3 accumulates to 3 and triggers the circuit-breaker.
// E1 fix (2026-10-02): two-layer failure count.
const maxTaskToolFailEntries = 1000 // cap; clearing is safe (loses counts for abandoned tasks)

func (s *Spawner) incrTaskToolFail(taskID, toolName string, stepCount int) int {
	s.taskFailMu.Lock()
	defer s.taskFailMu.Unlock()
	if s.taskToolFailCount == nil {
		s.taskToolFailCount = make(map[string]map[string]int)
	}
	// Cap to prevent unbounded growth from abandoned tasks.
	// audit L-3: previously cleared the ENTIRE map, losing circuit-breaker
	// state for all active tasks. Now evicts ~25% of entries, preserving
	// most active tasks' failure counts. Random map iteration ≈ random eviction.
	if len(s.taskToolFailCount) > maxTaskToolFailEntries {
		target := maxTaskToolFailEntries * 3 / 4
		for k := range s.taskToolFailCount {
			if len(s.taskToolFailCount) <= target {
				break
			}
			if k != taskID {
				delete(s.taskToolFailCount, k)
			}
		}
	}
	taskCounts := s.taskToolFailCount[taskID]
	if taskCounts == nil {
		taskCounts = make(map[string]int)
		s.taskToolFailCount[taskID] = taskCounts
	}
	taskCounts[toolName]++
	if taskCounts[toolName] > stepCount {
		return taskCounts[toolName]
	}
	return stepCount
}

// persistTraceOnError persists the accumulated trace to the task store when
// doSpawn returns an error. Without this, the trace (tool interactions from
// turns before the error) is lost, making the engine DB incomplete — eval
// §8.48.6 observed 28.3% of steps missing from the DB, almost all from error
// paths. Best-effort: taskStoreSet is nil-safe and ignores write errors
// (we're already on the error path; a store failure is not actionable here).
func (s *Spawner) persistTraceOnError(ctx context.Context, req *SpawnRequest, trace []store.ToolInteraction, providerID, modelID, capability string) {
	s.taskStoreSet(ctx, req.TaskID, req.StepID, &store.StepResult{
		Capability: capability,
		ProviderID: providerID,
		ModelID:    modelID,
		Trace:      trace,
		CreatedAt:  time.Now(),
	})
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
	if s.taskStore == nil || len(req.ContextRefs) == 0 {
		return resolvedCtx
	}

	// M-1 (2026-10-04): Group refs by taskID for batch fetching via GetBatch,
	// replacing the N+1 pattern (one ts.Get per ref) with one GetBatch per task.
	type refEntry struct {
		name   string
		stepID string
	}
	byTask := make(map[string][]refEntry)
	for name, ref := range req.ContextRefs {
		if ref == "" {
			continue
		}
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) == 2 {
			byTask[parts[0]] = append(byTask[parts[0]], refEntry{name: name, stepID: parts[1]})
		}
	}

	for taskID, refs := range byTask {
		stepIDs := make([]string, len(refs))
		for i, r := range refs {
			stepIDs[i] = r.stepID
		}
		results, err := s.taskStore.GetBatch(ctx, taskID, stepIDs)
		if err != nil {
			// Fallback to individual gets on batch failure.
			for _, r := range refs {
				if result := s.taskStoreGet(ctx, taskID, r.stepID); result != nil {
					resolvedCtx[r.name] = result.Data
				}
			}
			continue
		}
		for _, r := range refs {
			if result, ok := results[r.stepID]; ok {
				resolvedCtx[r.name] = result.Data
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

	// Phase 2: If the role is Generatable, try findskills first (fast tier-1 path).
	// SECURITY FIX (2026-10-21): This branch was previously AFTER the AllowDynamic
	// early return, making it dead code for sop_generator (Generatable=true but
	// AllowDynamicSkills=true → early return). Moved BEFORE the early return so
	// falling back to dynamic expansion.
	//
	// TODO: none — (1)+(2) RESOLVED 2026-10-22 (affinity sort + experience-library
	// search via store.QuerySkillRecommendations, spawner_capability_resolve.go);
	// (3) RESOLVED 2026-10-22 (D3: Skill.MountPolicy × submitter tier gate).
	if role.Generatable && s.tryFindSkillsForMissing(ctx, req, missing) {
		return nil
	}

	// If the role allows dynamic expansion, downgrade the error to a warning.
	if role.AllowDynamicSkills || role.AllowDynamicMCPs {
		s.logger.LogCtx(ctx, "EventCapabilityWarning", req.TaskID, req.StepID, map[string]any{
			"role":    req.RoleID,
			"missing": missing,
			"message": fmt.Sprintf("Role %q is missing required capabilities [%s], but has dynamic expansion enabled. Proceeding with caution.", req.RoleID, missingStr),
		})
		return nil
	}

	// No dynamic expansion available.
	s.logger.LogCtx(ctx, EventTaskFailed, req.TaskID, req.StepID, map[string]any{
		"role":    req.RoleID,
		"missing": missing,
		"error":   "Missing required capabilities",
	})
	if role.Generatable {
		// findskills was tried (above) but didn't find anything, and the role
		// doesn't have dynamic expansion — escalate to RoleGenerator.
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
		// ★ Semantic compression before side-loading (ADDED 2026-10-28).
		// See docs/plan/UPSTREAM_CONTEXT_SUMMARIZER_DESIGN.md §2.5.
		summarizerEnabled := s.upstreamSummarizer != nil && s.upstreamSummarizerConfig.Enabled

		var summary string
		if summarizerEnabled {
			budget := s.upstreamSummarizerConfig.BudgetChars
			if budget <= 0 {
				budget = 2000
			}
			if sm, err := s.upstreamSummarizer.Summarize(ctx, ctxBytes, budget); err == nil && sm != "" {
				summary = sm
			}
			// Summarizer failure → summary stays "" → graceful degradation to placeholder.
		}

		file, err := s.assets.Handle(req.TaskID, req.StepID, ctxBytes, "json", "upstream_context")
		if err == nil && file != nil {
			if summarizerEnabled && summary != "" {
				// Semantic summary mode: inject key facts as a distinct block.
				refPointer := map[string]any{
					"ref":     file.Path,
					"size":    ctxLen,
					"summary": summary,
					"step":    req.StepID,
					"task":    req.TaskID,
				}
				ptrBytes, _ := json.Marshal(refPointer)
				userText = fmt.Sprintf("%s\n\n### Upstream Context (ref-based)\n%s\n\n%s", userText, summary, string(ptrBytes))
				s.logger.Log("EventUpstreamContextSemanticSummary", req.TaskID, req.StepID, map[string]any{
					"path":          file.Path,
					"bytes":         ctxLen,
					"threshold":     effectiveThreshold,
					"summary_chars": len(summary),
					"summary_mode":  s.upstreamSummarizerConfig.Mode,
					"model_max": func() int {
						if injectionPCfg != nil {
							return injectionPCfg.MaxContextWindow
						}
						return 0
					}(),
				})
			} else {
				// Original behavior: placeholder summary, no summary block.
				// Used when summarizer is disabled, nil, or failed (graceful degradation).
				refPointer := map[string]any{
					"ref":     file.Path,
					"size":    ctxLen,
					"summary": "Upstream context side-loaded (adaptive model threshold); use read_file to inspect.",
					"step":    req.StepID,
					"task":    req.TaskID,
				}
				ptrBytes, _ := json.Marshal(refPointer)
				userText = fmt.Sprintf("%s\n\n### Upstream Context (ref-based)\n%s", userText, string(ptrBytes))
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
			}
		} else {
			userText = fmt.Sprintf("%s\n\n### Upstream Context\n%s", userText, string(ctxBytes))
		}
	} else {
		userText = fmt.Sprintf("%s\n\n### Upstream Context\n%s", userText, string(ctxBytes))
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
	systemBlocks, err := s.buildSystemPrompt(ctx, req.Hub, role, prunedSkills, role.BaseCapability, pCfg, toolConstraints, delegationCtx, toolFewShots, req.Isolation, delegationPrecedents, req.Task, "", req.TaskID, nil)
	if err != nil {
		return nil, false
	}
	s.logger.Log(EventStepStarted, req.TaskID, req.StepID, map[string]any{
		"action": "delegation_triggered",
		"reason": "system_delegation_mode_enabled",
	})
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
				"workspace":         delegationWorkspaceBlock(s.outputBase, req.TaskID, req.SessionRoot),
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
