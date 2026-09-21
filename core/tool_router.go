package core

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ToolRouter decides which tools should be exposed to the subagent based on the task description.
type ToolRouter struct {
	Registry    *config.Registry
	BM25Corpus  *BM25Corpus // Unified Hybrid Search (ADDED 2026-09-01)
	EmbedClient EmbeddingClient
	CaSKG       *CaSKGManager
	mu          sync.RWMutex
	embeddings  map[string][]float32 // tool ID -> vec
}

func NewToolRouter(reg *config.Registry, caskg *CaSKGManager) *ToolRouter {
	return &ToolRouter{
		Registry:   reg,
		CaSKG:      caskg,
		embeddings: make(map[string][]float32),
	}
}

// RouteRequest defines parameters for tool routing.
type RouteRequest struct {
	Task               string
	Bindings           []config.MCPBinding
	Turn               int
	ProgressiveMode    bool
	MaxTokenBudget     int    // max token budget for tool schemas (0 = disabled)
	PredecessorSkillID string // Added for CaSKG causal boost
}

// privilegedToolBlocklist contains tools that must never be exposed to
// subagents via routing, as they bypass orchestration-level safety gates.
var privilegedToolBlocklist = map[string]bool{
	"orchestrator_submit_decision":    true,
	"orchestrator_debug_dump":         true,
	"orchestrator_reload":             true,
	"orchestrator_admin_deploy":       true,
	"orchestrator_cancel_task":        true,
	"orchestrator_fork_task":          true,
	"orchestrator_run_command":        true,
	"orchestrator_invoke":             true,
	"orchestrator_core_replace_batch": true,
}

// Route selects a subset of tools from the provided MCP bindings based on task relevance.
func (r *ToolRouter) Route(req RouteRequest) []config.MCPBinding {
	taskLower := strings.ToLower(req.Task)
	var routed []config.MCPBinding

	for _, b := range req.Bindings {
		mcpDef := r.Registry.GetMCP(b.MCPID)
		if mcpDef == nil {
			// JIT §5.1: stale jit_ IDs (expired/evicted) are filtered here
			// before prompt assembly, preventing ghost-tool exposure.
			continue
		}

		wasRestricted := b.IsRestricted()

		var available []string
		if wasRestricted {
			available = b.AllowedTools
		} else {
			available = mcpDef.AvailableTools
		}

		var filtered []string
		explicitlyRelevant := make(map[string]bool)

		// --- RRF Hybrid Ranking (SCOUT-style) ---
		var bm25Rank map[string]float64
		var denseRank map[string]float64
		stringRank := make(map[string]float64)

		// 1. Sparse Channel: BM25
		if r.BM25Corpus != nil {
			bm25Rank = r.BM25Corpus.Score(req.Task)
		}

		// 2. Dense Channel: Embedding
		if r.EmbedClient != nil {
			if qVec, err := r.EmbedClient.Embed(context.Background(), req.Task); err == nil {
				candidates := make(map[string][]float32)
				r.mu.RLock()
				for _, tool := range available {
					if v, ok := r.embeddings[tool]; ok {
						candidates[tool] = v
					}
				}
				r.mu.RUnlock()
				if len(candidates) > 0 {
					denseRank = CosineRank(qVec, candidates)
				}
			}
		}

		// 3. String Channel: Legacy isRelevant
		for _, tool := range available {
			if r.isRelevant(taskLower, tool) {
				stringRank[tool] = 1.0
				explicitlyRelevant[tool] = true
			}
		}

		// Fuse all channels
		if len(bm25Rank) > 0 || len(denseRank) > 0 {
			merged := RRFMerge(bm25Rank, denseRank, stringRank)

			// Apply Causal Boost
			if r.CaSKG != nil && req.PredecessorSkillID != "" {
				for toolID := range merged {
					boost := r.CaSKG.GetCausalBoost(req.PredecessorSkillID, toolID)
					merged[toolID] *= boost
				}
			}

			if len(merged) > 0 {
				type toolScore struct {
					id    string
					score float64
				}
				var sorted []toolScore
				for id, score := range merged {
					sorted = append(sorted, toolScore{id: id, score: score})
				}
				sort.Slice(sorted, func(i, j int) bool { return sorted[i].score > sorted[j].score })

				// Threshold/Limit for filtered tools with token budget + embedding dedup
				limit := 5
				if len(sorted) < 5 {
					limit = len(sorted)
				}
				var usedBudget int
				// Collect the RRF-ranked tool IDs to build an embedding similarity matrix
				rankedList := make([]string, 0, len(sorted))
				for _, ts := range sorted {
					rankedList = append(rankedList, ts.id)
				}
				// Embedding dedup: mark tools redundant (>0.85 cosine) with an already-selected tool
				redundant := make(map[string]bool)
				for i := 0; i < limit && !redundant[sorted[i].id]; i++ {
					selected := sorted[i].id
					// Check token budget
					tokens := r.estimateToolTokens(selected, mcpDef)
					if req.MaxTokenBudget > 0 && usedBudget+tokens > req.MaxTokenBudget && i > 0 {
						break
					}
					usedBudget += tokens
					filtered = append(filtered, selected)
					// Mark future tools redundant with this selection
					if vec := r.embeddings[selected]; len(vec) > 0 {
						for _, cand := range rankedList {
							if cand == selected || redundant[cand] {
								continue
							}
							candVec := r.embeddings[cand]
							if len(candVec) == 0 {
								continue
							}
							if cosineSimF32(vec, candVec) > 0.85 {
								redundant[cand] = true
							}
						}
					}
				}
			}
		} else {
			// No hybrid (BM25/embedding) signal available -- keep the legacy,
			// uncapped, deterministic behavior: every string-matched tool passes
			// through, in available's original order.
			for _, tool := range available {
				if explicitlyRelevant[tool] {
					filtered = append(filtered, tool)
				}
			}
		}

		isFallback := false
		if len(filtered) == 0 {
			isFallback = true
			if !wasRestricted {
				filtered = available
			} else if len(available) <= 5 {
				filtered = available
			} else {
				// Large restricted list with no relevance match: fall back to exploration
				for _, tool := range available {
					tLower := strings.ToLower(tool)
					if isDiscoveryTool(tLower) {
						filtered = append(filtered, tool)
					}
				}
				if len(filtered) == 0 && len(available) > 0 {
					limit := 3
					if len(available) < 3 {
						limit = len(available)
					}
					filtered = available[:limit]
				}
			}
		}

		// Priority 4: Progressive Disclosure (Discovery First)
		// Only prune if we fell back to a large set, OR if the task is very general.
		if req.ProgressiveMode && req.Turn == 0 && len(filtered) > 3 {
			var discovery []string
			var specializedRelevant []string

			for _, tool := range filtered {
				if isDiscoveryTool(tool) {
					discovery = append(discovery, tool)
				} else if explicitlyRelevant[tool] {
					specializedRelevant = append(specializedRelevant, tool)
				}
			}

			// If we have discovery tools, and either we are in fallback mode OR
			// we have many specialized tools that weren't explicitly mentioned
			if len(discovery) >= 2 {
				if isFallback || len(specializedRelevant) == 0 {
					filtered = discovery
				} else {
					// Keep discovery + explicitly relevant specialized tools
					filtered = append(discovery, specializedRelevant...)
				}
			}
		}

		// Prune privileged tools that must never be exposed to subagents
		var pruned []string
		for _, tool := range filtered {
			if !privilegedToolBlocklist[tool] {
				pruned = append(pruned, tool)
			}
		}
		filtered = pruned

		routed = append(routed, config.MCPBinding{
			MCPID:        b.MCPID,
			AllowedTools: filtered,
		})
	}

	return routed
}

// containsWholeWord reports whether word appears in s as a whole word (surrounded
// by non-alphanumeric characters or string boundaries). This intentionally uses
// only ASCII alphanumeric detection for boundaries, which works correctly for
// mixed Chinese/English text: any non-ASCII UTF-8 byte is not alphanumeric, so
// English words embedded in Chinese text are correctly detected as whole words.
func containsWholeWord(s, word string) bool {
	for start := 0; start <= len(s)-len(word); {
		idx := strings.Index(s[start:], word)
		if idx < 0 {
			break
		}
		abs := start + idx
		before := abs == 0 || !isAlphaNum(s[abs-1])
		after := abs+len(word) == len(s) || !isAlphaNum(s[abs+len(word)])
		if before && after {
			return true
		}
		start = abs + 1
	}
	return false
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// estimateToolTokens returns a rough token count for a tool schema.
// When FullToolDefinitions is available, count chars of name+desc+params / 4.
// Fallback: name length / 4 + 8 (minimum tool schema cost).
func (r *ToolRouter) estimateToolTokens(toolName string, mcpDef *config.MCPDef) int {
	if mcpDef != nil {
		for _, dt := range mcpDef.FullToolDefinitions {
			if dt.Name == toolName {
				rough := len(dt.Name) + len(dt.Description)
				if dt.InputSchema != nil {
					rough += len(fmt.Sprintf("%v", dt.InputSchema))
				}
				return rough/4 + 8 // ~4 chars/token + 8 overhead
			}
		}
	}
	return len(toolName)/4 + 8
}

// cosineSimF32 returns the cosine similarity between two float32 vectors.
func cosineSimF32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func isDiscoveryTool(name string) bool {
	lower := strings.ToLower(name)
	discoveryKeywords := []string{"list", "search", "status", "analyze", "grep", "read", "find", "inspect", "get", "ls", "query"}
	for _, kw := range discoveryKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func (r *ToolRouter) isRelevant(task, tool string) bool {
	toolLower := strings.ToLower(tool)

	// 1. Direct name match
	if strings.Contains(task, toolLower) {
		return true
	}

	// 2. Token-level match (High signal)
	toolTokens := strings.Split(toolLower, "_")
	for _, tok := range toolTokens {
		if len(tok) >= 4 && containsWholeWord(task, tok) {
			return true
		}
	}

	// 3. Keyword affinity mapping.
	affinity := map[string][]string{
		"file":    {"read", "write", "list", "fs", "path", "mkdir", "rm", "cp", "mv"},
		"browser": {"navigate", "click", "type", "screenshot", "scroll", "back", "forward"},
		"code":    {"analyze", "intel", "lint", "search", "grep", "ast", "find", "patch", "refactor"},
		"git":     {"clone", "commit", "push", "pull", "log", "checkout", "branch"},
		"network": {"get", "post", "request", "fetch", "curl", "http"},
		"process": {"run", "exec", "kill", "ps", "top"},
		"shell":   {"command", "sh", "bash", "cmd"},
	}

	for key, patterns := range affinity {
		if containsWholeWord(task, key) {
			for _, p := range patterns {
				for _, tok := range toolTokens {
					if tok == p {
						return true
					}
				}
			}
		}
	}

	return false
}

// SearchResult is the return type of SearchTools.
type SearchResult struct {
	Tools []schemas.ToolDefinition
}

// SearchTools searches all registered MCP tools by keyword with pagination.
// Returns tools whose Name or Description contains the keyword (case-insensitive).
func (r *ToolRouter) SearchTools(ctx context.Context, keyword, mcpID string, page, pageSize int) (*SearchResult, error) {
	if pageSize <= 0 {
		pageSize = 10
	}
	if page <= 0 {
		page = 1
	}

	keywordLower := strings.ToLower(keyword)
	var matched []schemas.ToolDefinition

	r.Registry.Mu.RLock()
	defer r.Registry.Mu.RUnlock()

	for _, mcp := range r.Registry.MCPs {
		if mcpID != "" && mcp.ID != mcpID {
			continue
		}
		for _, tool := range mcp.FullToolDefinitions {
			if keywordLower == "" ||
				strings.Contains(strings.ToLower(tool.Name), keywordLower) ||
				strings.Contains(strings.ToLower(tool.Description), keywordLower) {
				matched = append(matched, tool)
			}
		}
	}

	// Sort for deterministic pagination
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Name < matched[j].Name
	})

	start := (page - 1) * pageSize
	if start >= len(matched) {
		return &SearchResult{Tools: nil}, nil
	}
	end := start + pageSize
	if end > len(matched) {
		end = len(matched)
	}

	return &SearchResult{Tools: matched[start:end]}, nil
}
