package core

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// defaultToolSchemaBudgetTokens is the fallback ceiling on the total size of the
// tool schemas the router is willing to expose in one step, used when the
// deployment leaves RouteRequest.MaxTokenBudget at 0.
//
// It is a context-economy budget, not a tool count: the point of the router is
// to keep tool schemas from crowding out the actual work, and what crowds the
// context window is bytes of schema, not number of entries. The value is a
// small fraction of a modern context window (a few percent) so that a large
// tool surface -- hundreds of schemas -- stays affordable after routing, and it
// scales automatically with schema size: verbose schemas yield fewer tools,
// terse ones yield more, for the same cost.
const defaultToolSchemaBudgetTokens = 8000

// ToolRouter decides which tools should be exposed to the subagent based on the task description.
type ToolRouter struct {
	Registry       *config.Registry
	BM25Corpus     *BM25Corpus // Unified Hybrid Search (ADDED 2026-09-01)
	EmbedClient    EmbeddingClient
	CaSKG          *CaSKGManager
	mu             sync.RWMutex
	embeddings     map[string][]float32 // tool ID -> vec
	toolVersions   map[string]string    // tool ID -> version key
	lastRefreshSig string               // audit FIX 2026-11-22: skip rebuild when registry unchanged

	systemOneProvider *providers.SystemOneProvider
	systemOneModel    string

	// lazyDiscoveryHook (ADDED 2026-10-20): called by Route when
	// LazyDiscovery is enabled and an MCP's AvailableTools is empty.
	// The hook triggers tools/list discovery for that MCP.
	// Set by Spawner during initialization; nil when LazyDiscovery is off.
	lazyDiscoveryHook func(ctx context.Context, mcpID, taskID string) error
}

func NewToolRouter(reg *config.Registry, caskg *CaSKGManager) *ToolRouter {
	return &ToolRouter{
		Registry:     reg,
		CaSKG:        caskg,
		embeddings:   make(map[string][]float32),
		toolVersions: make(map[string]string),
	}
}

// SetEmbedClient wires the embedding client for semantic tool routing.
func (r *ToolRouter) SetEmbedClient(c EmbeddingClient) {
	r.EmbedClient = c
}

// SetLazyDiscoveryHook wires the lazy discovery callback.
// When set, Route will call it for any MCP with empty AvailableTools
// instead of expecting pre-populated discovery.
func (r *ToolRouter) SetLazyDiscoveryHook(fn func(ctx context.Context, mcpID, taskID string) error) {
	r.lazyDiscoveryHook = fn
}

// RefreshToolEmbeddings builds or updates BM25 + embedding caches for all
// registered MCP tools. Skips unchanged tools (versionKey unchanged).
// Embedding I/O happens outside locks, mirroring IntentRouter.RefreshEmbeddings.
// computeRefreshSig returns a deterministic signature of the MCP registry state
// that captures all changes relevant to tool routing: MCP IDs, their available
// tool counts, and their full tool definition counts. If the signature is
// unchanged since the last RefreshToolEmbeddings call, the rebuild can be skipped
// entirely (audit FIX 2026-11-22).
func computeRefreshSig(mcpCopy map[string]*config.MCPDef) string {
	ids := make([]string, 0, len(mcpCopy))
	for id := range mcpCopy {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	for _, id := range ids {
		mcp := mcpCopy[id]
		if mcp == nil {
			continue
		}
		sb.WriteString(id)
		sb.WriteByte(':')
		sb.WriteString(strconv.Itoa(len(mcp.AvailableTools)))
		sb.WriteByte(':')
		sb.WriteString(strconv.Itoa(len(mcp.FullToolDefinitions)))
		sb.WriteByte(';')
	}
	return sb.String()
}

func (r *ToolRouter) RefreshToolEmbeddings(ctx context.Context, reg *config.Registry) {
	if reg == nil {
		return
	}

	// Snapshot MCP definitions under registry read lock.
	reg.Mu.RLock()
	mcpCopy := make(map[string]*config.MCPDef, len(reg.MCPs))
	for k, v := range reg.MCPs {
		mcpCopy[k] = v
	}
	reg.Mu.RUnlock()

	// Audit FIX 2026-11-22: compute a registry signature and skip the entire
	// rebuild (BM25 corpus + doc building + embedding) when nothing has changed
	// since the last call. This avoids O(N×M) work on every config reload when
	// the registry is unchanged.
	sig := computeRefreshSig(mcpCopy)
	r.mu.RLock()
	if sig == r.lastRefreshSig && r.BM25Corpus != nil {
		r.mu.RUnlock()
		return
	}
	r.mu.RUnlock()

	// Build rich embedding text and version key per tool.
	type toolDoc struct {
		id         string
		versionKey string
		text       string
	}
	var docs []toolDoc

	for _, mcp := range mcpCopy {
		if mcp == nil {
			continue
		}
		// audit L11 (2026-11-05): formerly O(N×M) linear scan of
		// FullToolDefinitions for each toolName in AvailableTools.
		// Pre-build a name→description index for O(1) lookup.
		toolDescIndex := make(map[string]string, len(mcp.FullToolDefinitions))
		for _, def := range mcp.FullToolDefinitions {
			if def.Description != "" {
				toolDescIndex[def.Name] = def.Description
			}
		}
		for _, toolName := range mcp.AvailableTools {
			// M-7 (2026-10-04): Use strings.Builder instead of repeated += allocation.
			var sb strings.Builder
			sb.WriteString("[Tool:")
			sb.WriteString(toolName)
			sb.WriteString("] ")
			if mcp.Domain != "" {
				sb.WriteString(mcp.Domain)
				sb.WriteByte(' ')
			}
			sb.WriteString(mcp.ID)
			sb.WriteByte(' ')
			for _, p := range mcp.Provides {
				sb.WriteString(p)
				sb.WriteByte(' ')
			}
			// Pull description from the pre-built index.
			if desc, ok := toolDescIndex[toolName]; ok {
				sb.WriteString(desc)
				sb.WriteByte(' ')
			}
			versionKey := mcp.Domain + "_" + strconv.Itoa(len(mcp.AvailableTools))
			docs = append(docs, toolDoc{id: toolName, versionKey: versionKey, text: strings.TrimSpace(sb.String())})
		}
	}

	if len(docs) == 0 {
		return
	}

	// Build BM25 corpus from all tool docs.
	// NOTE (2026-10-18): RefreshToolEmbeddings is now also called from
	// buildMCPServers after MCP discovery, i.e. concurrently with sibling DAG
	// steps that may be inside Route() reading BM25Corpus. Guard the swap.
	bm25Docs := make(map[string]string, len(docs))
	for _, d := range docs {
		bm25Docs[d.id] = d.text
	}
	newCorpus := NewBM25Corpus(1.2, 0.75, bm25Docs)
	r.mu.Lock()
	r.BM25Corpus = newCorpus
	r.lastRefreshSig = sig
	r.mu.Unlock()

	if r.EmbedClient == nil {
		return // BM25-only mode; no embedding to compute.
	}

	// Incremental: skip tools whose versionKey hasn't changed.
	type pendingEmbed struct {
		id         string
		versionKey string
		text       string
	}
	var pending []pendingEmbed

	r.mu.RLock()
	for _, d := range docs {
		if r.toolVersions[d.id] == d.versionKey && r.embeddings[d.id] != nil {
			continue
		}
		pending = append(pending, pendingEmbed{id: d.id, versionKey: d.versionKey, text: d.text})
	}
	r.mu.RUnlock()

	if len(pending) == 0 {
		return
	}

	// Compute embeddings outside any lock (network I/O).
	type embedResult struct {
		id         string
		versionKey string
		vec        []float32
	}
	var results []embedResult

	for _, p := range pending {
		embedCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if vec, err := r.EmbedClient.Embed(embedCtx, p.text); err == nil && len(vec) > 0 {
			results = append(results, embedResult{id: p.id, versionKey: p.versionKey, vec: vec})
		}
		cancel()
	}

	// Batch-update maps under a short write lock.
	if len(results) > 0 {
		r.mu.Lock()
		for _, res := range results {
			r.embeddings[res.id] = res.vec
			r.toolVersions[res.id] = res.versionKey
		}
		r.mu.Unlock()
	}
}

// SetSystemOneProvider injects the System One decision model for semantic
// tool scoring. Nil-safe: when nil, the router uses string-only matching.
func (r *ToolRouter) SetSystemOneProvider(p *providers.SystemOneProvider, model string) {
	r.systemOneProvider = p
	r.systemOneModel = model
}

// scoreWithSystemOne asks the System One model to score tool relevance
// semantically. Returns a rank map (toolID → probability). On any error
// or timeout, returns nil (caller falls back to string-only matching).
func (r *ToolRouter) scoreWithSystemOne(ctx context.Context, task string, tools []string) map[string]float64 {
	questions := map[string]any{
		"q1": map[string]any{
			"type":     "choice",
			"question": "Which tool is most relevant to this task?",
			"options":  tools,
		},
	}
	req := providers.CompleteRequest{
		User:        "Task: " + task,
		Model:       r.systemOneModel,
		MaxTokens:   256,
		Constraints: map[string]any{"questions": questions},
	}
	// L-2.1: derive from propagated ctx so cancellation propagates
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := r.systemOneProvider.Complete(ctx, req)
	if err != nil {
		slog.Warn("tool_router: System One reranker failed, returning nil rank (graceful degradation)", "err", err)
		return nil
	}
	rank := make(map[string]float64)
	for _, tc := range resp.ToolCalls {
		if tc.Name != "q1" {
			continue
		}
		if probs, ok := tc.Arguments["probabilities"].(map[string]any); ok {
			for tool, p := range probs {
				if score, ok := p.(float64); ok {
					rank[tool] = score
				}
			}
		}
		if choice, ok := tc.Arguments["choice"].(string); ok {
			if _, exists := rank[choice]; !exists {
				rank[choice] = 1.0
			}
		}
	}
	if len(rank) == 0 {
		return nil
	}
	return rank
}

// RouteRequest defines parameters for tool routing.
type RouteRequest struct {
	Task               string
	Bindings           []config.MCPBinding
	Turn               int
	ProgressiveMode    bool
	MaxTokenBudget     int    // max token budget for tool schemas (0 = disabled)
	PredecessorSkillID string // Added for CaSKG causal boost
	TaskID             string // Added for lazy discovery hook (2026-10-20)
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
// L-2.1: ctx propagates for embedding/system-one cancellation.
func (r *ToolRouter) Route(ctx context.Context, req RouteRequest) []config.MCPBinding {
	taskLower := strings.ToLower(req.Task)
	var routed []config.MCPBinding

	// One token ceiling across all bindings in this routing call.
	budget := newRouteBudget(req.MaxTokenBudget)

	for _, b := range req.Bindings {
		mcpDef, ok := r.resolveRouteMCP(ctx, req, b)
		if !ok {
			continue
		}

		wasRestricted := b.IsRestricted()

		var available []string
		if wasRestricted {
			available = b.AllowedTools
		} else {
			available = mcpDef.AvailableTools
		}

		tokenEstimate := routeToolEstimator(mcpDef)

		ranks := r.rankRouteTools(ctx, req, taskLower, available)

		var filtered []string
		if ranks.hasHybridSignal() {
			filtered = r.selectHybridTools(req, ranks, budget, tokenEstimate)
		} else {
			filtered = selectRelevantTools(available, ranks.explicitly, budget, tokenEstimate)
		}
		isFallback := false
		if len(filtered) == 0 {
			filtered = fallbackRouteTools(available, wasRestricted, budget, tokenEstimate)
			isFallback = true
		}

		filtered = applyProgressiveDisclosure(req, filtered, ranks.explicitly, isFallback)
		filtered = prunePrivilegedTools(filtered)

		// always_include (C1-②): tools that must survive routing. Appended AFTER
		// relevance filtering so they are kept even when the task text never
		// mentions them (e.g. supervisor__complete_task in AppWorld).
		//
		// Two hard constraints (2026-10-18):
		//  1. always_include must NOT widen a restricted binding. A restricted
		//     binding is an explicit allowlist; letting always_include bypass it
		//     would silently escalate privileges (the two tests in
		//     tool_router_always_include_test.go assert the opposite for the
		//     unrestricted case only — see note there).
		//  2. always_include must NOT resurrect privileged tools that the
		//     blocklist just removed.
		//  3. always_include must NOT manufacture tools the MCP does not have
		//     (see the `known` comment below).
		if len(b.AlwaysInclude) > 0 {
			// Catalog membership. A name in always_include is a claim that the tool
			// exists; appending it unchecked produces a phantom: the tool shows up in
			// the model's surface, the model calls it, and every call comes back
			// empty, because the MCP server never had it.
			//
			// Measured 2026-10-19 (AppWorld 4fab96f_3, run u): the role's
			// always_include named phone__get_current_date_and_time while
			// APPWORLD_MCP_APPS loaded only supervisor+venmo. The routed surface had
			// 60 names, one of which was the phantom. The agent called it 5 times,
			// got an empty body each time, never learned the current date, and so
			// could never evaluate "not approved or denied for 50 or more days".
			//
			// Guarded on a non-empty catalog: an empty AvailableTools means discovery
			// has not populated it yet (cold start, see ensureMCPDiscovery). Dropping
			// every entry then would remove the safety net precisely when it is
			// needed.
			filtered = appendAlwaysIncludeTools(b, mcpDef, filtered, wasRestricted)
		}

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
					rough += jsonSchemaApproxSize(dt.InputSchema)
				}
				return rough/4 + 8
			}
		}
	}
	return len(toolName)/4 + 8
}

// jsonSchemaApproxSize estimates the byte size of a JSON schema map without
// allocating a formatted string (audit H9: replaces fmt.Sprintf("%v", ...) which
// was called per-tool per-Route invocation).
func jsonSchemaApproxSize(schema any) int {
	switch v := schema.(type) {
	case map[string]any:
		n := 2 // {}
		for k, val := range v {
			n += len(k) + 3 // "key":
			n += jsonSchemaApproxSize(val)
		}
		return n
	case []any:
		n := 2 // []
		for _, val := range v {
			n += jsonSchemaApproxSize(val)
		}
		return n
	case string:
		return len(v) + 2
	case nil:
		return 4
	default:
		return 8
	}
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

// toolAffinity maps task-domain keywords to tool-name patterns for relevance
// matching. Package-level to avoid per-call allocation in isRelevant (audit H5).
var toolAffinity = map[string][]string{
	"file":    {"read", "write", "list", "fs", "path", "mkdir", "rm", "cp", "mv"},
	"browser": {"navigate", "click", "type", "screenshot", "scroll", "back", "forward"},
	"code":    {"analyze", "intel", "lint", "search", "grep", "ast", "find", "patch", "refactor"},
	"git":     {"clone", "commit", "push", "pull", "log", "checkout", "branch"},
	"network": {"get", "post", "request", "fetch", "curl", "http"},
	"process": {"run", "exec", "kill", "ps", "top"},
	"shell":   {"command", "sh", "bash", "cmd"},
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

	// 3. Keyword affinity mapping (uses package-level toolAffinity).
	for key, patterns := range toolAffinity {
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
// SearchTools searches tool definitions by keyword. When roleID is non-empty,
// the search is scoped to that role's bound MCPs only (D3 fix, 2026-10-20:
// prevents privilege escalation — a role must not discover tools from MCPs it
// is not bound to). When roleID is empty, searches all MCPs (admin/debug use).
// When mcpID is non-empty, further narrows to a single MCP within the scope.
func (r *ToolRouter) SearchTools(ctx context.Context, keyword, mcpID, roleID string, page, pageSize int) (*SearchResult, error) {
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

	// D3 fix: scope to role's bound MCPs when roleID is provided.
	scopeMCPs := r.Registry.MCPs
	if roleID != "" {
		role, ok := r.Registry.Roles[roleID]
		if !ok || role == nil || len(role.BoundMCPBindings) == 0 {
			// Role not found or has no bindings → empty result.
			return &SearchResult{Tools: nil}, nil
		}
		scopeMCPs = make(map[string]*config.MCPDef, len(role.BoundMCPBindings))
		for _, b := range role.BoundMCPBindings {
			if mcp, ok := r.Registry.MCPs[b.MCPID]; ok {
				scopeMCPs[b.MCPID] = mcp
			}
		}
	}

	for _, mcp := range scopeMCPs {
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
