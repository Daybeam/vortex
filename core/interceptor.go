package core

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

// ── Context Provider ────────────────────────────────────────────────────────

// ContextProvider injects real-time environmental data into the System Prompt.
type ContextProvider interface {
	Name() string
	FetchContext(ctx context.Context) (map[string]any, error)
}

// ── Runtime Governance Interceptor API ──────────────────────────────────────

type InterceptorAPI struct {
	available map[string]bool
	mu        sync.RWMutex
}

func NewInterceptorAPI(names []string) *InterceptorAPI {
	api := &InterceptorAPI{available: make(map[string]bool)}
	for _, name := range names {
		api.available[name] = true
	}
	return api
}

func (api *InterceptorAPI) HandleList(w http.ResponseWriter, r *http.Request) {
	api.mu.RLock()
	defer api.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(api.available)
}

func (api *InterceptorAPI) HandleToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if _, ok := api.available[req.Name]; ok {
		api.available[req.Name] = req.Enabled
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	} else {
		http.Error(w, "interceptor not found", http.StatusNotFound)
	}
}

// ── Runtime Governance Interceptors ─────────────────────────────────────────

// SpawnRequest encapsulates all parameters required to spawn a subagent.
type SpawnRequest struct {
	TaskID                   string
	StepID                   string
	RoleID                   string
	Task                     string
	AdditionalSkills         []string
	AdditionalMCPs           []string
	AdditionalToolAllowlists map[string][]string
	ContextRefs              map[string]string
	// DegradedMCPs: MCPs that failed health check and were skipped.
	// Populated by HealthCheckInterceptor. Downstream spawner should not attempt to start them.
	DegradedMCPs []string
	// TurnsBudgetBonus: extra tool-turn budget granted on top of maxTurns,
	// propagated from Step.TurnsBudgetBonus by the scheduler. Lets a resumed
	// step actually get more room instead of re-hitting the same cap.
	// ADDED (2026-09-07).
	TurnsBudgetBonus int
	Temperature      *float32
	FrequencyPenalty *float32
	Metadata         map[string]any
	// ProviderOverride: if set, overrides the role-level provider for this step.
	// Must match a named provider in the registry (config.json "providers" keys).
	ProviderOverride string
	RoutingMode      string
	CompressionHint  string

	// SessionRoot is the session-level authorized workspace root directory.
	// When non-empty, the spawner's SafePathValidator uses it as a Level 2
	// trust root, allowing file operations outside the task output directory.
	// See docs/completed/2026-09-19/WORKSPACE_AND_SANDBOX_REDESIGN.md §2.2.
	SessionRoot string

	// Hub provides access to tiered role/skill resolution (Session IR + Global Registry).
	// ADDED (2026-07-20) for Session Role support.
	Hub *ContextHub

	// EvoX-inspired Zero-Loss Coordination (ADDED 2026-08-17)
	InputMapping map[string]any // Resolved values to inject into tool calls
	Isolation    bool           // If true, skip historical summaries

	// Observability & Tracing (ADDED 2026-08-28)
	TraceID string
	SpanID  string

	// ODFTP-native recovery context (ADDED 2026-08-30)
	AdditionalPromptContext string
}

// SpawnerHandler is the core function signature for spawning a subagent.
type SpawnerHandler func(ctx context.Context, req *SpawnRequest) (*SpawnResult, error)

// Interceptor is a middleware that can wrap the SpawnerHandler.
type Interceptor func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error)

// BuildInterceptorChain wraps a handler with a chain of interceptors (onion model).
func BuildInterceptorChain(interceptors []Interceptor, handler SpawnerHandler) SpawnerHandler {
	for i := len(interceptors) - 1; i >= 0; i-- {
		interceptor := interceptors[i]
		next := handler // capture loop variable
		handler = func(ctx context.Context, req *SpawnRequest) (*SpawnResult, error) {
			return interceptor(ctx, req, next)
		}
	}
	return handler
}
