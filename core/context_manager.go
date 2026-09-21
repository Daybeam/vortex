package core

import (
	"context"
	"regexp"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// CompressionLevel defines the intensity of context reduction.
type CompressionLevel int

// Package-level regexes for applyRTK — compiled once, reused on every call (audit: was recompiled per call).
var (
	rtkReTimestamp = regexp.MustCompile(`\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\]`)
	rtkReProgress  = regexp.MustCompile(`(?m)^.*(Progress|Processing|Updating).*(\d+%|...).*$`)
)

const (
	LevelNone       CompressionLevel = iota
	LevelLight                       // Session-Dedup + RTK
	LevelAggressive                  // LLMLingua-2 / Rule-based pruning
	LevelCritical                    // Force task splitting (Fork)
)

// AuditResult provides the decision on how to handle the current context.
type AuditResult struct {
	Decision         CompressionLevel
	OriginalTokens   int
	CompressedTokens int
	CompressionRatio float64
	ActionTaken      string
	ReserveTokens    int // reply reserve subtracted from maxCtx (0 if skipped)
	EffectiveMaxCtx  int // maxCtx - ReserveTokens (the effective input capacity)
}

// ContextManager handles the quantification and reduction of prompt tokens.
type ContextManager struct {
	registry *config.Registry
	anchors  []string // Critical semantic markers that MUST be preserved
	sieve    *Sieve   // ADDED 2026-08-30 for Fidelity Audit
}

func NewContextManager(reg *config.Registry) *ContextManager {
	return &ContextManager{
		registry: reg,
		anchors:  []string{"S2T", "DecisionID", "Goal:", "Final Result:", "$artifact", "$contract"},
		sieve:    NewSieve(reg.System.MaxContextKeep),
	}
}

// applyCompressionHint scales the soft effCtx threshold used by Audit to
// decide between LevelLight/LevelAggressive, based on a per-step opt-in
// hint. "terse"/"aggressive" halves the threshold (compression engages
// earlier -- appropriate for steps expected to produce verbose raw tool
// output, e.g. log dumps or web scraping). "verbose" raises it by 50%
// (capped at maxCtx, never exceeding the hard ceiling) so more raw detail
// is preserved before compression kicks in. Any other value, including
// the empty string, leaves effCtx unchanged -- this is a strictly
// additive, opt-in mechanism with no effect on existing steps.
func applyCompressionHint(effCtx, maxCtx int, hint string) int {
	switch hint {
	case "terse", "aggressive":
		return effCtx / 2
	case "verbose":
		scaled := effCtx * 3 / 2
		if scaled > maxCtx {
			scaled = maxCtx
		}
		return scaled
	default:
		return effCtx
	}
}

// Audit evaluates the prompt size against model limits and decides on compression.
// Anti-Cliff (ADDED 2026-08-27, arXiv:2608.22752): content is split into
// Protected Zone (SOP rules, system prompt, contracts — NEVER compressed,
// verbatim) and Volatile Zone (tool outputs, raw logs, upstream history —
// compressible). The compression tier decision uses only the Volatile Zone's
// remaining budget (maxCtx minus Protected Zone tokens), so a bloated tool
// log cannot silently steal compression headroom from the rules layer.
func (cm *ContextManager) Audit(ctx context.Context, provider providers.Provider, req *schemas.CompleteRequest) (*AuditResult, error) {
	// 1a. Quantify Protected Zone (never compressed)
	protectedTokens, err := cm.countZone(ctx, provider, req, zoneProtected)
	if err != nil {
		return nil, err
	}
	// 1b. Quantify Volatile Zone (compressible)
	volatileTokens, err := cm.countZone(ctx, provider, req, zoneVolatile)
	if err != nil {
		return nil, err
	}
	totalTokens := protectedTokens + volatileTokens

	// 2. Resolve limits from config
	var pCfg *config.ProviderConfig
	cm.registry.Mu.RLock()
	for _, cfg := range cm.registry.Providers {
		if cfg.Provider == provider.Name() {
			pCfg = cfg
			break
		}
	}
	cm.registry.Mu.RUnlock()

	if pCfg == nil {
		return &AuditResult{Decision: LevelNone, OriginalTokens: totalTokens}, nil
	}

	maxCtx := pCfg.MaxContextWindow
	effCtx := pCfg.EffectiveContextWindow
	tokenLimit := pCfg.TokenLimit
	if maxCtx <= 0 {
		return &AuditResult{Decision: LevelNone, OriginalTokens: totalTokens, ActionTaken: "NO_LIMIT_CONFIGURED"}, nil
	}

	// 2.1 Reserve: subtract reply reserve from the context window to get the
	// effective input capacity. This prevents the context from being filled
	// to 100% of maxCtx, leaving no space for the model to generate a reply.
	// Design ref: docs/architecture/CONTEXT_WINDOW_RESERVE_DESIGN.md §3.1.
	reserveTokens := pCfg.GetReserveTokens()
	effectiveMaxCtx := maxCtx - reserveTokens
	if effectiveMaxCtx <= 0 {
		// Reserve exceeds or equals the window — this happens for small test
		// windows (e.g. maxCtx=200 with default reserve=4096) or misconfigured
		// production. Skip reserve to preserve existing behavior.
		reserveTokens = 0
		effectiveMaxCtx = maxCtx
	}

	if tokenLimit == 0 {
		tokenLimit = effectiveMaxCtx
	}

	// 2.5 Anti-Cliff uncompressible-floor check: if Protected Zone alone
	// occupies more than 75% of maxCtx, NO compression tier can help — must
	// Fork. Squeeze cannot touch protected content.
	if maxCtx > 0 && float64(protectedTokens)/float64(maxCtx) > 0.75 {
		return &AuditResult{
			Decision:        LevelCritical,
			OriginalTokens:  totalTokens,
			ActionTaken:     "PROTECTED_ZONE_OVERRUN_FORK",
			ReserveTokens:   reserveTokens,
			EffectiveMaxCtx: effectiveMaxCtx,
		}, nil
	}

	effCtx = applyCompressionHint(effCtx, effectiveMaxCtx, req.CompressionHint)

	// 3. Decision Logic — all comparisons use effectiveMaxCtx (maxCtx minus
	// reserve) so compression triggers before the model loses reply space.
	if totalTokens > tokenLimit {
		return &AuditResult{Decision: LevelCritical, OriginalTokens: totalTokens, ActionTaken: "HARD_LIMIT_BREACH_FORK", ReserveTokens: reserveTokens, EffectiveMaxCtx: effectiveMaxCtx}, nil
	}
	if totalTokens > effectiveMaxCtx {
		return &AuditResult{Decision: LevelCritical, OriginalTokens: totalTokens, ActionTaken: "FORCE_FORK", ReserveTokens: reserveTokens, EffectiveMaxCtx: effectiveMaxCtx}, nil
	}

	// Anti-Cliff: compressible headroom is (effCtx - protectedTokens - reserve).
	// This prevents long tool logs from stealing the rules layer's budget AND
	// ensures the reserve is respected in the soft threshold as well.
	compressibleBudget := effCtx - protectedTokens - reserveTokens
	if compressibleBudget < 0 {
		compressibleBudget = 0
	}

	if volatileTokens > compressibleBudget {
		return &AuditResult{Decision: LevelAggressive, OriginalTokens: totalTokens, ActionTaken: "SEMANTIC_PRUNING", ReserveTokens: reserveTokens, EffectiveMaxCtx: effectiveMaxCtx}, nil
	}
	if volatileTokens > (compressibleBudget / 2) {
		return &AuditResult{Decision: LevelLight, OriginalTokens: totalTokens, ActionTaken: "STRUCTURAL_DEDUP", ReserveTokens: reserveTokens, EffectiveMaxCtx: effectiveMaxCtx}, nil
	}

	return &AuditResult{Decision: LevelNone, OriginalTokens: totalTokens, ActionTaken: "PASS", ReserveTokens: reserveTokens, EffectiveMaxCtx: effectiveMaxCtx}, nil
}

// zoneKind discriminates which zone the counter should tally.
type zoneKind int

const (
	zoneProtected zoneKind = iota
	zoneVolatile
)

// countZone returns the total token count of the requested zone.
// zoneProtected = ProtectedSystemBlocks + ProtectedUserBlocks.
// zoneVolatile  = SystemBlocks + UserBlocks (legacy) + System/User fallback
//   - VolatileSystemBlocks + VolatileUserBlocks.
func (cm *ContextManager) countZone(ctx context.Context, provider providers.Provider, req *schemas.CompleteRequest, zone zoneKind) (int, error) {
	if zone == zoneProtected {
		var sb strings.Builder
		for _, b := range req.ProtectedSystemBlocks {
			sb.WriteString(b.Text)
		}
		for _, b := range req.ProtectedUserBlocks {
			sb.WriteString(b.Text)
		}
		return provider.CountTokens(ctx, sb.String())
	}

	// Volatile: legacy fields + legacy blocks + new Volatile* blocks
	var sb strings.Builder
	if len(req.SystemBlocks) > 0 {
		for _, b := range req.SystemBlocks {
			sb.WriteString(b.Text)
		}
	} else if req.System != "" {
		sb.WriteString(req.System)
	}
	sb.WriteString("\n\n")
	if len(req.UserBlocks) > 0 {
		for _, b := range req.UserBlocks {
			sb.WriteString(b.Text)
		}
	} else if req.User != "" {
		sb.WriteString(req.User)
	}
	for _, b := range req.VolatileSystemBlocks {
		sb.WriteString(b.Text)
	}
	for _, b := range req.VolatileUserBlocks {
		sb.WriteString(b.Text)
	}
	return provider.CountTokens(ctx, sb.String())
}

// VerifyAnchors ensures that critical semantic signals survived compression.
func (cm *ContextManager) VerifyAnchors(original, compressed string) (bool, string) {
	if cm.sieve != nil {
		return cm.sieve.VerifyFidelity(original, compressed)
	}
	for _, anchor := range cm.anchors {
		if strings.Contains(original, anchor) && !strings.Contains(compressed, anchor) {
			return false, anchor
		}
	}
	return true, ""
}

// Squeeze applies the decided compression level to the request.
// Anti-Cliff (ADDED 2026-08-27, arXiv:2608.22752): ONLY the Volatile Zone
// is compressed. ProtectedSystemBlocks / ProtectedUserBlocks flow through
// untouched at every compression tier (Light / Aggressive / Critical).
// This is the mechanical guarantee that SOP rules, system prompts, and
// artifact contracts survive long-running tasks verbatim.
func (cm *ContextManager) Squeeze(ctx context.Context, provider providers.Provider, req *schemas.CompleteRequest, level CompressionLevel) (*schemas.CompleteRequest, *AuditResult, error) {
	if level == LevelNone {
		return req, &AuditResult{Decision: LevelNone, OriginalTokens: 0, CompressedTokens: 0, CompressionRatio: 1.0}, nil
	}

	// Extract Volatile-only content (legacy fields/blocks + new Volatile* blocks).
	volatileSystem := req.System
	volatileUser := req.User
	if len(req.SystemBlocks) > 0 {
		var sb strings.Builder
		for _, b := range req.SystemBlocks {
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		}
		volatileSystem = sb.String()
	}
	if len(req.UserBlocks) > 0 {
		var sb strings.Builder
		for _, b := range req.UserBlocks {
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		}
		volatileUser = sb.String()
	}
	for _, b := range req.VolatileSystemBlocks {
		volatileSystem += b.Text + "\n"
	}
	for _, b := range req.VolatileUserBlocks {
		volatileUser += b.Text + "\n"
	}

	originalVolatile := volatileSystem + "\n\n" + volatileUser
	origTokens, _ := provider.CountTokens(ctx, originalVolatile)

	// Protected zone is carried through verbatim — never touched by Squeeze.
	protectedBlocks := make([]string, 0, len(req.ProtectedSystemBlocks)+len(req.ProtectedUserBlocks))
	for _, b := range req.ProtectedSystemBlocks {
		protectedBlocks = append(protectedBlocks, b.Text)
	}
	for _, b := range req.ProtectedUserBlocks {
		protectedBlocks = append(protectedBlocks, b.Text)
	}

	// L1: Light Compression — only on Volatile
	if level >= LevelLight {
		volatileUser = applyRTK(volatileUser)
		volatileSystem = applySessionDedup(volatileSystem)
	}
	// L2: Aggressive Compression — only on Volatile
	if level >= LevelAggressive {
		volatileUser = applySemanticPruning(volatileUser)
	}

	// Preserve protected verbatim inside System (concatenated, prepended so it
	// stays at the top of the model's system view). If a provider honors
	// ContentBlock caching, consumers can also reconstruct ProtectedSystemBlocks
	// from newReq.ProtectedSystemBlocks; the string form below is a fallback.
	protectedConcat := strings.Join(protectedBlocks, "\n\n")
	if protectedConcat != "" {
		volatileSystem = protectedConcat + "\n\n" + volatileSystem
	}

	newReq := &schemas.CompleteRequest{
		System:                volatileSystem,
		User:                  volatileUser,
		ProtectedSystemBlocks: req.ProtectedSystemBlocks,
		ProtectedUserBlocks:   req.ProtectedUserBlocks,
		VolatileSystemBlocks:  req.VolatileSystemBlocks,
		VolatileUserBlocks:    req.VolatileUserBlocks,
		Model:                 req.Model,
		MaxTokens:             req.MaxTokens,
		Temperature:           req.Temperature,
		FrequencyPenalty:      req.FrequencyPenalty,
		MCPServers:            req.MCPServers,
		Attachments:           req.Attachments,
		Secrets:               req.Secrets,
		ForceToolCall:         req.ForceToolCall,
		Constraints:           req.Constraints,
		CompressionHint:       req.CompressionHint,
	}

	compressedText := newReq.System + "\n\n" + newReq.User

	// Anchor guardian now runs over the FULL compressed text (including
	// protected content we've merged in), so an anchor embedded in a
	// Protected block still triggers Fork if the consumer only inspects the
	// flat System string.
	if level >= LevelAggressive {
		if faithful, lost := cm.VerifyAnchors(originalVolatile+"\n\n"+protectedConcat, compressedText); !faithful {
			return newReq, &AuditResult{
				Decision:       LevelCritical,
				OriginalTokens: origTokens,
				ActionTaken:    "ANCHOR_LOSS_FORCE_FORK: " + lost,
			}, nil
		}
	}

	compTokens, _ := provider.CountTokens(ctx, compressedText)
	ratio := 1.0
	if origTokens > 0 {
		ratio = float64(compTokens) / float64(origTokens)
	}

	return newReq, &AuditResult{
		Decision:         level,
		OriginalTokens:   origTokens,
		CompressedTokens: compTokens,
		CompressionRatio: ratio,
	}, nil
}

// --- Internal Compression Implementations (Stubs for now) ---

func applySessionDedup(text string) string {
	// Identify and remove redundant system-state snapshots or repeated prompt fragments.
	// Pattern: Look for repeated blocks of text separated by typical turn markers.
	lines := strings.Split(text, "\n")
	var result []string
	seen := make(map[string]bool)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			result = append(result, line)
			continue
		}
		// Only dedup very long lines (potential snapshots/logs) to avoid breaking natural dialogue
		if len(trimmed) > 100 {
			if seen[trimmed] {
				continue
			}
			seen[trimmed] = true
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func applyRTK(text string) string {
	// RTK-Filter: Remove tool output noise
	// 1. Remove redundant timestamps [YYYY-MM-DD HH:mm:ss]
	text = rtkReTimestamp.ReplaceAllString(text, "")

	// 2. Remove repetitive "Progress: X%" or "Processing..." logs
	text = rtkReProgress.ReplaceAllString(text, "")

	// 3. Trim excessive whitespace created by pruning
	return strings.TrimSpace(text)
}

func applySemanticPruning(text string) string {
	// Priority-Window: Keep the head (goal) and the tail (recent context), compress the middle.
	// Since we treat the context as a flat string, we attempt to split by turn markers.
	markers := []string{"User:", "Assistant:", "System:", "Tool:", "### Upstream Context"}
	var turns []string
	var currentTurn strings.Builder

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		isMarker := false
		for _, m := range markers {
			if strings.HasPrefix(line, m) {
				isMarker = true
				break
			}
		}
		if isMarker && currentTurn.Len() > 0 {
			turns = append(turns, strings.TrimSpace(currentTurn.String()))
			currentTurn.Reset()
		}
		if currentTurn.Len() > 0 {
			currentTurn.WriteString("\n")
		}
		currentTurn.WriteString(line)
	}
	if currentTurn.Len() > 0 {
		turns = append(turns, strings.TrimSpace(currentTurn.String()))
	}

	if len(turns) <= 5 {
		return text // Too short to prune
	}

	// Keep: first turn (goal) + last 3 turns (immediate context)
	// Prune: the middle
	var pruned []string
	pruned = append(pruned, turns[0]) // The Head

	// Middle turns are summarized (simulated here by taking only first 2 lines of each turn)
	for i := 1; i < len(turns)-3; i++ {
		lines := strings.Split(turns[i], "\n")
		if len(lines) > 2 {
			pruned = append(pruned, lines[0]+"\n"+lines[1]+"\n... [compressed]")
		} else {
			pruned = append(pruned, turns[i])
		}
	}

	// The Tail
	pruned = append(pruned, turns[len(turns)-3:]...)

	return strings.Join(pruned, "\n")
}
