package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// AnalyzeHandler handles POST /api/analyze — returns a complexity hint for a task.
func (h *APIHandler) AnalyzeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Ported complexity hint logic
	hint := "single_step"
	if strings.Contains(strings.ToLower(req.Task), " and ") || strings.Contains(req.Task, ",") {
		hint = "multi_step_dag"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"complexity_hint": hint,
		"task_preview":    req.Task,
	})
}

// ContextProfileHandler handles GET /api/context/profile — returns the token-
// proportion breakdown of the system prompt for the observability "context
// budget" panel.
//
// source is "live" when a real system prompt has been assembled during a spawn,
// or "sample" when no task has run yet (a representative profile is assembled
// on demand from the default role + default provider). The profile covers the
// inner System Prompt breakdown — the user-visible ask of visualizing what
// % of the context is skills / mcp / rules / precedents / ….
//
// ADDED (2026-10-12): the profile now also carries the outer ring —
// tools_tokens / history_tokens (estimated per spawn turn) plus the
// provider-reported prompt_tokens / completion_tokens. "outer_ring.captured"
// tells the UI whether those figures come from a real turn (live) or read
// zero (sample).
func (h *APIHandler) ContextProfileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.Scheduler == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}

	spawner := h.App.Scheduler.GetSpawner()
	profile := spawner.LastSystemPromptProfile()
	source := "live"
	if profile == nil {
		sample, err := spawner.SampleSystemPromptProfile()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"source": "none",
				"error":  err.Error(),
				"outer_ring": map[string]any{
					"captured": false,
					"note":     "No profile available; the outer ring is captured live from spawn turns.",
				},
			})
			return
		}
		profile = sample
		source = "sample"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"source":  source,
		"profile": profile,
		"outer_ring": map[string]any{
			// ADDED (2026-10-12): the outer ring used to be a deferred
			// increment; it is now captured live per spawn turn
			// (tools_tokens / history_tokens / prompt_tokens /
			// completion_tokens on the profile).
			"captured": source == "live",
			"note":     "tools/history/prompt/completion figures are captured live from each spawn turn; a sample has no live turn, so they read zero.",
		},
	})
}

// UsageHandler serves GET /api/usage — the global token-consumption ledger
// (ADDED 2026-10-12). The snapshot maps each chat model AND each provider to
// prompt/completion/cache/tool-call totals, plus a separate "_embedding"
// block with per-embedding-model input tokens and call counts. This is the
// raw material the observability panel renders as consumption tables, and
// what future cost accounting / consumption benchmarking will build on.
func (h *APIHandler) UsageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.Scheduler == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"usage": h.App.Scheduler.GetSpawner().GetUsageSnapshot(),
	})
}
