package core

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/daybeam/vortex/config"
)

// routeBudget is shared by every binding in one Route call. The budget is a
// per-step context-economy ceiling, not a per-binding allowance.
type routeBudget struct {
	total int
	used  int
}

func newRouteBudget(requested int) *routeBudget {
	if requested <= 0 {
		requested = defaultToolSchemaBudgetTokens
	}
	return &routeBudget{total: requested}
}

// take applies the original selection rule: the first tool of the entire call
// is admitted even when oversized, but later tools must fit the remaining
// budget. Oversized entries are skipped rather than ending the scan.
func (b *routeBudget) take(candidates []string, estimate func(string) int) []string {
	remaining := b.total - b.used
	var selected []string
	var used int
	for _, tool := range candidates {
		tokens := estimate(tool)
		if (b.used > 0 || len(selected) > 0) && used+tokens > remaining {
			continue
		}
		used += tokens
		selected = append(selected, tool)
	}
	b.used += used
	return selected
}

// resolveRouteMCP drops stale/disabled MCPs and performs lazy discovery before
// the binding's authorized tool set is snapshotted.
func (r *ToolRouter) resolveRouteMCP(ctx context.Context, req RouteRequest, binding config.MCPBinding) (*config.MCPDef, bool) {
	mcpDef := r.Registry.GetMCP(binding.MCPID)
	if mcpDef == nil || !mcpActive(mcpDef) {
		return nil, false
	}
	if r.lazyDiscoveryHook != nil && len(mcpDef.AvailableTools) == 0 && mcpDef.Command != "" {
		// Surface discovery failure: swallowing it previously allowed a call to
		// proceed with an undiscovered, potentially restricted tool surface.
		if err := r.lazyDiscoveryHook(ctx, mcpDef.ID, req.TaskID); err != nil {
			slog.Warn("tool-router: lazy discovery failed", "mcp", mcpDef.ID, "error", err)
		}
		// Discovery may have populated AvailableTools. Preserve the original
		// behavior of re-fetching for existence, without a second active check.
		mcpDef = r.Registry.GetMCP(binding.MCPID)
		if mcpDef == nil {
			return nil, false
		}
	}
	return mcpDef, true
}

// routeToolEstimator builds the per-binding schema estimate cache once, rather
// than scanning FullToolDefinitions for every selected tool.
func routeToolEstimator(mcpDef *config.MCPDef) func(string) int {
	cache := make(map[string]int, len(mcpDef.FullToolDefinitions))
	for _, def := range mcpDef.FullToolDefinitions {
		rough := len(def.Name) + len(def.Description)
		if def.InputSchema != nil {
			rough += jsonSchemaApproxSize(def.InputSchema)
		}
		cache[def.Name] = rough/4 + 8
	}
	return func(toolName string) int {
		if tokens, ok := cache[toolName]; ok {
			return tokens
		}
		return len(toolName)/4 + 8
	}
}

type routeRanks struct {
	bm25       map[string]float64
	dense      map[string]float64
	string     map[string]float64
	systemOne  map[string]float64
	explicitly map[string]bool
}

func (ranks routeRanks) hasHybridSignal() bool {
	return len(ranks.bm25) > 0 || len(ranks.dense) > 0 || len(ranks.systemOne) > 0
}

// rankRouteTools evaluates the four channels in their established order. The
// string channel also records explicit relevance for disclosure/fallback.
func (r *ToolRouter) rankRouteTools(ctx context.Context, req RouteRequest, taskLower string, available []string) routeRanks {
	ranks := routeRanks{
		string:     make(map[string]float64, len(available)),
		explicitly: make(map[string]bool, len(available)),
	}

	// 1. Sparse channel: BM25. RefreshToolEmbeddings may swap the corpus, so
	// snapshot the pointer under the router lock.
	r.mu.RLock()
	corpus := r.BM25Corpus
	r.mu.RUnlock()
	if corpus != nil {
		ranks.bm25 = corpus.Score(req.Task)
		// The corpus covers all registered tools; never let it widen this
		// binding's authorized candidate set.
		if len(ranks.bm25) > 0 {
			authorized := make(map[string]bool, len(available))
			for _, tool := range available {
				authorized[tool] = true
			}
			for id := range ranks.bm25 {
				if !authorized[id] {
					delete(ranks.bm25, id)
				}
			}
		}
	}

	// 2. Dense channel: embedding.
	if r.EmbedClient != nil {
		if queryVec, err := r.EmbedClient.Embed(ctx, req.Task); err == nil {
			candidates := make(map[string][]float32)
			r.mu.RLock()
			for _, tool := range available {
				if vec, ok := r.embeddings[tool]; ok {
					candidates[tool] = vec
				}
			}
			r.mu.RUnlock()
			if len(candidates) > 0 {
				ranks.dense = CosineRank(queryVec, candidates)
			}
		}
	}

	// 3. String channel: legacy relevance.
	for _, tool := range available {
		if r.isRelevant(taskLower, tool) {
			ranks.string[tool] = 1.0
			ranks.explicitly[tool] = true
		}
	}

	// 4. System One channel: semantic scoring for small candidate pools.
	if r.systemOneProvider != nil && len(available) <= 20 {
		ranks.systemOne = r.scoreWithSystemOne(ctx, req.Task, available)
	}
	return ranks
}

func (r *ToolRouter) mergeRouteRanks(req RouteRequest, ranks routeRanks) map[string]float64 {
	merged := RRFMerge(ranks.bm25, ranks.dense, ranks.string, ranks.systemOne)
	if r.CaSKG != nil && req.PredecessorSkillID != "" {
		for toolID := range merged {
			merged[toolID] *= r.CaSKG.GetCausalBoost(req.PredecessorSkillID, toolID)
		}
	}
	return merged
}

// selectHybridTools preserves RRF score ordering and the deterministic id
// tie-break before applying the shared schema budget.
func (r *ToolRouter) selectHybridTools(req RouteRequest, ranks routeRanks, budget *routeBudget, estimate func(string) int) []string {
	merged := r.mergeRouteRanks(req, ranks)
	if len(merged) == 0 {
		return nil
	}
	type toolScore struct {
		id    string
		score float64
	}
	sorted := make([]toolScore, 0, len(merged))
	for id, score := range merged {
		sorted = append(sorted, toolScore{id: id, score: score})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].score != sorted[j].score {
			return sorted[i].score > sorted[j].score
		}
		return sorted[i].id < sorted[j].id
	})
	candidates := make([]string, len(sorted))
	for i, tool := range sorted {
		candidates[i] = tool.id
	}
	return budget.take(candidates, estimate)
}

// selectRelevantTools keeps legacy available-order selection when no hybrid
// channel has signal.
func selectRelevantTools(available []string, explicitly map[string]bool, budget *routeBudget, estimate func(string) int) []string {
	var relevant []string
	for _, tool := range available {
		if explicitly[tool] {
			relevant = append(relevant, tool)
		}
	}
	return budget.take(relevant, estimate)
}

// fallbackRouteTools selects exploration candidates only after the normal
// ranking path produced an empty surface.
func fallbackRouteTools(available []string, restricted bool, budget *routeBudget, estimate func(string) int) []string {
	var candidates []string
	if !restricted || len(available) <= 5 {
		candidates = available
	} else {
		// Large restricted lists with no relevance match expose discovery
		// tools, or at most the first three entries when none are discovery tools.
		for _, tool := range available {
			if isDiscoveryTool(strings.ToLower(tool)) {
				candidates = append(candidates, tool)
			}
		}
		if len(candidates) == 0 && len(available) > 0 {
			limit := 3
			if len(available) < limit {
				limit = len(available)
			}
			candidates = available[:limit]
		}
	}
	return budget.take(candidates, estimate)
}

func applyProgressiveDisclosure(req RouteRequest, filtered []string, explicitly map[string]bool, fallback bool) []string {
	if !req.ProgressiveMode || req.Turn != 0 || len(filtered) <= 3 {
		return filtered
	}
	var discovery []string
	var specialized []string
	for _, tool := range filtered {
		if isDiscoveryTool(tool) {
			discovery = append(discovery, tool)
		} else if explicitly[tool] {
			specialized = append(specialized, tool)
		}
	}
	if len(discovery) < 2 {
		return filtered
	}
	if fallback || len(specialized) == 0 {
		return discovery
	}
	return append(discovery, specialized...)
}

func prunePrivilegedTools(filtered []string) []string {
	var pruned []string
	for _, tool := range filtered {
		if !privilegedToolBlocklist[tool] {
			pruned = append(pruned, tool)
		}
	}
	return pruned
}

// appendAlwaysIncludeTools keeps mandatory tools after relevance filtering but
// never widens an allowlist, resurrects a privileged tool, or invents a tool
// absent from a non-empty discovered catalog.
func appendAlwaysIncludeTools(binding config.MCPBinding, mcpDef *config.MCPDef, filtered []string, restricted bool) []string {
	if len(binding.AlwaysInclude) == 0 {
		return filtered
	}
	inScope := make(map[string]bool, len(binding.AllowedTools))
	if restricted {
		for _, tool := range binding.AllowedTools {
			inScope[tool] = true
		}
	}
	known := make(map[string]bool, len(mcpDef.AvailableTools))
	for _, tool := range mcpDef.AvailableTools {
		known[tool] = true
	}
	seen := make(map[string]bool, len(filtered))
	for _, tool := range filtered {
		seen[tool] = true
	}
	var skipped, unknown []string
	for _, tool := range binding.AlwaysInclude {
		if seen[tool] {
			continue
		}
		if privilegedToolBlocklist[tool] {
			skipped = append(skipped, tool)
			continue
		}
		if restricted && !inScope[tool] {
			skipped = append(skipped, tool)
			continue
		}
		// An empty catalog means discovery has not completed; retain the
		// cold-start safety net rather than dropping every mandatory tool.
		if len(known) > 0 && !known[tool] {
			unknown = append(unknown, tool)
			continue
		}
		filtered = append(filtered, tool)
	}
	if len(skipped) > 0 {
		slog.Warn("always_include entry ignored by tool router",
			"mcp", binding.MCPID,
			"restricted", restricted,
			"skipped", strings.Join(skipped, ","),
			"reason", "privileged_blocklist_or_outside_allowlist")
	}
	if len(unknown) > 0 {
		slog.Warn("always_include entry names a tool the MCP does not serve",
			"mcp", binding.MCPID,
			"unknown", strings.Join(unknown, ","),
			"catalog_size", len(known),
			"reason", "not_in_discovered_catalog")
	}
	return filtered
}
