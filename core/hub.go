package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"unicode"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ContextHub provides a unified interface to resolve roles and skills,
// abstracting the tiered lookup between session-scoped ephemeral data
// and the global registry.
type ContextHub struct {
	Registry *config.Registry
	Graph    *schemas.TaskGraph
	Exp      store.IExperienceStore // ADDED (2026-08-16): Support JIT/Generated skills

	// OnUnknownMCP is called when ResolveFinalMCPs encounters an MCP ID not
	// in the static registry. If the callback returns a non-nil MCPDef, it
	// is registered in DynamicMCPs and the binding proceeds. If nil, the
	// original error is returned. This fixes the ghost binding defect
	// (DELEGATION_AND_SUBMIT_DEFECTS.md §2.2): allows runtime dynamic MCP
	// injection without pre-registration in config.json.
	OnUnknownMCP func(mcpID string) *config.MCPDef

	// Circuit breaker for embedding providers: track non-retryable failures per provider
	// and skip them entirely for the rest of the process lifetime (reset on restart or
	// via orchestrator_discover). 429/rate-limit errors are EXCLUDED — they auto-recover.
	circuitMu      sync.Mutex
	circuitBroken  map[string]int // poolID → consecutive non-429 failure count
	circuitMaxFail int            // threshold before a provider is dead
}

func NewContextHub(reg *config.Registry, graph *schemas.TaskGraph, exp store.IExperienceStore) *ContextHub {
	return &ContextHub{
		Registry:       reg,
		Graph:          graph,
		Exp:            exp,
		circuitBroken:  make(map[string]int),
		circuitMaxFail: 3,
	}
}

// resetCircuitBreaker clears all embedding circuit breaker state.
// Called by orchestrator_discover or on explicit refresh.
func (h *ContextHub) ResetEmbeddingCircuitBreaker() {
	h.circuitMu.Lock()
	defer h.circuitMu.Unlock()
	h.circuitBroken = make(map[string]int)
}

// isEmbeddingCircuitOpen checks if a provider has been tripped.
func (h *ContextHub) isEmbeddingCircuitOpen(poolID string) bool {
	h.circuitMu.Lock()
	defer h.circuitMu.Unlock()
	return h.circuitBroken[poolID] >= h.circuitMaxFail
}

// recordEmbeddingFailure records one non-retryable embedding failure for a provider.
// Returns true if the circuit just tripped (reached threshold).
func (h *ContextHub) recordEmbeddingFailure(poolID string, err error) bool {
	h.circuitMu.Lock()
	defer h.circuitMu.Unlock()

	// Skip 429 / rate-limit — they auto-recover and should not trip the breaker
	errMsg := err.Error()
	if containsRateLimit(errMsg) {
		return false
	}

	h.circuitBroken[poolID]++
	if h.circuitBroken[poolID] >= h.circuitMaxFail {
		return true // just tripped
	}
	return false
}

func containsRateLimit(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "http 429") ||
		strings.Contains(lower, "429") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "rate_limited") ||
		strings.Contains(lower, "resource_exhausted")
}

func (h *ContextHub) GetRole(roleID string) *config.Role {
	if roleID == "" {
		return nil
	}

	// 1. Check Session Roles (Task-specific IR).
	// FIX (2026-07-20): this used to index h.Graph.SessionRoles directly, including
	// a cache-write-back with no synchronization at all. DirectedEngine.executeStep
	// runs independent steps of the same graph as concurrent goroutines (see
	// sync.WaitGroup usage there), so two parallel steps resolving the same
	// still-raw (map[string]any, e.g. from a resumed manifest) session role could
	// race on that write -- a real "concurrent map writes" panic risk, not just a
	// theoretical one. Route through TaskGraph's own mutex-guarded accessors
	// instead of touching the maps directly.
	if h.Graph != nil {
		if r, ok := h.Graph.GetSessionRole(roleID); ok {
			// If it's already a *config.Role, return it
			if role, ok := r.(*config.Role); ok {
				return role
			}
			// If it's a map[string]any (likely from manifest.json load), unmarshal it
			if m, ok := r.(map[string]any); ok {
				data, _ := json.Marshal(m)
				var role config.Role
				if err := json.Unmarshal(data, &role); err == nil {
					// Cache it back as the typed object to avoid re-unmarshaling
					h.Graph.SetSessionRole(roleID, &role)
					return &role
				} else {
					// FIX (2026-07-20): previously fell through silently to the global
					// registry lookup below on unmarshal failure -- if a session role was
					// meant to shadow a same-ID global role and its stored data was
					// corrupt, callers would silently get the (possibly quite different)
					// global role with no signal anything went wrong. Log it.
					log.Printf("[ContextHub] session role %q failed to unmarshal, falling back to global registry: %v", roleID, err)
				}
			}
		}
	}

	// 2. Check Global Registry
	h.Registry.Mu.RLock()
	defer h.Registry.Mu.RUnlock()
	return h.Registry.Roles[roleID]
}

func (h *ContextHub) GetSkill(skillID string) *config.Skill {
	if skillID == "" {
		return nil
	}

	// 1. Check Session Skills -- see GetRole above for why this goes through
	// TaskGraph's mutex-guarded accessors instead of indexing the maps directly.
	if h.Graph != nil {
		if s, ok := h.Graph.GetSessionSkill(skillID); ok {
			if skill, ok := s.(*config.Skill); ok {
				return skill
			}
			if m, ok := s.(map[string]any); ok {
				data, _ := json.Marshal(m)
				var skill config.Skill
				if err := json.Unmarshal(data, &skill); err == nil {
					// Skill filters are in-memory only and must be re-initialized
					skill.InitFilter()
					h.Graph.SetSessionSkill(skillID, &skill)
					return &skill
				} else {
					log.Printf("[ContextHub] session skill %q failed to unmarshal, falling back to global registry: %v", skillID, err)
				}
			}
		}
	}

	// 2. Check Global Registry
	h.Registry.Mu.RLock()
	skill, ok := h.Registry.Skills[skillID]
	h.Registry.Mu.RUnlock()
	if ok {
		return skill
	}

	// 3. Check Experience Store (Generated/JIT Skills)
	if h.Exp != nil {
		if gs, ok := h.Exp.GetGeneratedSkill(skillID); ok {
			return generatedToConfigSkill(gs)
		}
	}

	return nil
}

// generatedToConfigSkill converts an experience-store GeneratedSkill into a
// registry-compatible config.Skill. ADDED (2026-08-16).
func generatedToConfigSkill(gs *store.GeneratedSkill) *config.Skill {
	if gs == nil {
		return nil
	}
	s := &config.Skill{
		ID:          gs.ID,
		Name:        gs.ID,
		Capability:  gs.Capability,
		Description: gs.Description,
		Implementations: map[string]config.SkillImplementation{
			"default": {
				SystemPrompt: fmt.Sprintf("You have access to a specialized tool `%s` that was generated to solve: %s. Use it when appropriate.", gs.ID, gs.Description),
			},
		},
	}
	s.InitFilter()
	return s
}

// FindSkillByCapability attempts to find a registered skill whose Capability or ID
// matches the provided natural language description. This is used to map LLM's
// "capability_required"/retry_with_skill feedback (which is often a loose phrase
// like "fetch market data" rather than the exact registered ID) to a concrete
// skill object.
//
// FIX (2026-08-06): the original version of this function did a raw three-way
// strings.Contains (capability-in-desc, id-in-desc, desc-in-capability) over
// h.Registry.Skills, a Go map -- whose iteration order is randomized by the
// runtime. Two problems followed from that: (1) short/common substrings could
// cross-match an unrelated skill whose ID or Capability happened to share a
// few characters (the same false-positive class that caused three real
// regressions in tool_router.go -- see the 2026-06-27/06-30 addenda), and (2)
// because the first Contains-hit encountered during map iteration won, which
// skill "won" among multiple partial matches was NOT DETERMINISTIC -- the same
// input could resolve to a different skill on different calls, purely by luck
// of map iteration order. That is a materially worse failure mode than a
// simple false positive: a task could work on one retry and silently pick the
// wrong skill on the next, with the exact same input.
//
// This version: (a) checks for an exact, case-insensitive ID/Capability match
// first (the common case when the LLM happens to echo back the precise
// value) and returns immediately if so; (b) otherwise tokenizes the
// description and each candidate's ID+Capability into whole words (splitting
// on "_" too, so "skill_fetch_market_data" tokenizes the same as "fetch
// market data"), filters common English stopwords (the exact failure mode
// that motivated store/experience.go's tokenizeStopwords, 2026-07-10 addendum
// -- "the"/"of" alone are not discriminating), and scores by token overlap;
// (c) requires the overlap to cover at least half of the description's
// meaningful tokens (so a single incidental shared word on a multi-word
// description can't win, while a short precise description like "search"
// still can); (d) among candidates that clear the threshold, deterministically
// picks the highest score, tie-broken by ID -- never by map iteration order.
func (h *ContextHub) FindSkillByCapability(desc string) *config.Skill {
	if desc == "" {
		return nil
	}
	descLower := strings.ToLower(strings.TrimSpace(desc))
	descTokens := tokenizeCapabilityText(desc)
	minRequired := (len(descTokens) + 1) / 2
	if minRequired < 1 {
		minRequired = 1
	}

	exactMatch := func(s *config.Skill) bool {
		return strings.ToLower(s.ID) == descLower || strings.ToLower(s.Capability) == descLower
	}

	best := func(candidates []*config.Skill) *config.Skill {
		for _, s := range candidates {
			if exactMatch(s) {
				return s
			}
		}
		var winner *config.Skill
		winnerScore := 0
		for _, s := range candidates {
			score := skillCapabilityScore(descTokens, s)
			if score < minRequired {
				continue
			}
			if winner == nil || score > winnerScore || (score == winnerScore && s.ID < winner.ID) {
				winner = s
				winnerScore = score
			}
		}
		return winner
	}

	// 1. Check Session Skills (take priority over the global registry).
	if h.Graph != nil {
		var sessionCandidates []*config.Skill
		h.Graph.ForEachSessionSkill(func(id string, v any) {
			var skill *config.Skill
			if sk, ok := v.(*config.Skill); ok {
				skill = sk
			} else if m, ok := v.(map[string]any); ok {
				data, _ := json.Marshal(m)
				var sk config.Skill
				if err := json.Unmarshal(data, &sk); err == nil {
					sk.InitFilter()
					skill = &sk
				}
			}
			if skill != nil {
				sessionCandidates = append(sessionCandidates, skill)
			}
		})
		if match := best(sessionCandidates); match != nil {
			return match
		}
	}

	// 2. Check Global Registry.
	h.Registry.Mu.RLock()
	var globalCandidates []*config.Skill
	for _, s := range h.Registry.Skills {
		globalCandidates = append(globalCandidates, s)
	}
	h.Registry.Mu.RUnlock()

	if match := best(globalCandidates); match != nil {
		return match
	}

	// 3. Check Experience Store (Generated Skills).
	if h.Exp != nil {
		// GetGeneratedSkillsSnapshot handles its own locking internally.
		genCandidates := make([]*config.Skill, 0)
		for _, gs := range h.Exp.GetGeneratedSkillsSnapshot() {
			genCandidates = append(genCandidates, generatedToConfigSkill(gs))
		}
		if match := best(genCandidates); match != nil {
			return match
		}
	}

	return nil
}

// skillCapabilityStopwords filters common English words that would otherwise
// let a single incidental shared word win a token-overlap match between
// unrelated capability descriptions. Mirrors store/experience.go's
// tokenizeStopwords (2026-07-10 addendum), which was added after exactly
// this failure mode was caught by a test ("the"/"of" alone matching two
// otherwise-unrelated task descriptions).
var skillCapabilityStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "and": true, "or": true, "are": true, "is": true,
	"was": true, "were": true, "be": true, "this": true, "that": true,
	"with": true, "as": true, "at": true, "by": true, "it": true, "if": true,
	"please": true, "me": true, "my": true, "your": true, "you": true,
	"skill": true, // every registered skill ID is prefixed "skill_", not discriminating
}

// tokenizeCapabilityText splits s into lowercase, whole-word tokens on any
// non-alphanumeric boundary (so "_" in a snake_case skill ID splits the same
// way as a space would in a natural-language description), drops tokens
// under 2 characters and stopwords.
func tokenizeCapabilityText(s string) map[string]bool {
	tokens := make(map[string]bool)
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			w := strings.ToLower(string(cur))
			if !skillCapabilityStopwords[w] {
				tokens[w] = true
			}
		}
		cur = cur[:0]
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

// skillCapabilityScore counts how many of descTokens appear among the
// skill's own ID+Capability token set.
func skillCapabilityScore(descTokens map[string]bool, skill *config.Skill) int {
	skillTokens := tokenizeCapabilityText(skill.ID)
	for t := range tokenizeCapabilityText(skill.Capability) {
		skillTokens[t] = true
	}
	score := 0
	for t := range descTokens {
		if skillTokens[t] {
			score++
		}
	}
	return score
}

func (h *ContextHub) GetProviderConfig(providerID string) *config.ProviderConfig {
	if providerID == "" {
		return nil
	}

	// 1. Check Session Providers
	if h.Graph != nil {
		if p, ok := h.Graph.GetSessionProvider(providerID); ok {
			if pc, ok := p.(*config.ProviderConfig); ok {
				return pc
			}
			if m, ok := p.(map[string]any); ok {
				data, _ := json.Marshal(m)
				var pc config.ProviderConfig
				if err := json.Unmarshal(data, &pc); err == nil {
					h.Graph.SetSessionProvider(providerID, &pc)
					return &pc
				} else {
					log.Printf("[ContextHub] session provider %q failed to unmarshal, falling back to global registry: %v", providerID, err)
				}
			}
		}
	}

	// 2. Check Global Registry
	h.Registry.Mu.RLock()
	defer h.Registry.Mu.RUnlock()
	return h.Registry.Providers[providerID]
}

func (h *ContextHub) GetMCP(mcpID string) *config.MCPDef {
	h.Registry.Mu.RLock()
	defer h.Registry.Mu.RUnlock()

	// Dynamic JIT tools take precedence
	if m, ok := h.Registry.DynamicMCPs[mcpID]; ok {
		return m
	}
	return h.Registry.MCPs[mcpID]
}

// RegisterDynamicMCP adds a temporary MCP definition to the runtime registry.
// This allows zero-config dynamic extension: MCPs can be registered at submit
// time without pre-declaring them in config.json.
func (h *ContextHub) RegisterDynamicMCP(mcp *config.MCPDef) {
	h.Registry.Mu.Lock()
	defer h.Registry.Mu.Unlock()
	h.Registry.DynamicMCPs[mcp.ID] = mcp
}

// GetEmbeddingProvider resolves the system-wide embedding engine using tiered fallback.
func (h *ContextHub) GetEmbeddingProvider() (interfaces.Provider, string, error) {
	h.Registry.Mu.RLock()
	pID := h.Registry.DefaultEmbeddingProvider
	h.Registry.Mu.RUnlock()

	// 1. Try Primary (Global Default)
	if pID != "" {
		cfg := h.GetProviderConfig(pID)
		if cfg != nil && cfg.EmbeddingModel != "" {
			p, err := providers.Get(cfg, h.Registry.ExternalRuntimes)
			if err == nil {
				return p, cfg.EmbeddingModel, nil
			}
		}
	}

	// 2. Try Secondary (First available provider with embedding_model)
	h.Registry.Mu.RLock()
	defer h.Registry.Mu.RUnlock()

	// Iterate through global providers
	for _, cfg := range h.Registry.Providers {
		if cfg.EmbeddingModel != "" {
			p, err := providers.Get(cfg, h.Registry.ExternalRuntimes)
			if err == nil {
				return p, cfg.EmbeddingModel, nil
			}
		}
	}

	// 3. Last Resort: Check session-scoped providers (if not already checked via pID)
	if h.Graph != nil {
		for _, id := range h.Graph.ListSessionProviderIDs() {
			// GetProviderConfig handles unmarshaling/caching and locking
			cfg := h.GetProviderConfig(id)
			if cfg != nil && cfg.EmbeddingModel != "" {
				p, err := providers.Get(cfg, h.Registry.ExternalRuntimes)
				if err == nil {
					return p, cfg.EmbeddingModel, nil
				}
			}
		}
	}

	return nil, "", fmt.Errorf("no healthy embedding provider found")
}

// EmbedWithFallback performs an embedding operation using the tiered fallback logic.
// It attempts each available embedding provider until one succeeds or all fail.
func (h *ContextHub) EmbedWithFallback(ctx context.Context, text string) ([]float32, string, error) {
	h.Registry.Mu.RLock()
	pID := h.Registry.DefaultEmbeddingProvider
	h.Registry.Mu.RUnlock()

	var errs []string

	// 1. Try Primary (skip if circuit is open)
	if pID != "" {
		if h.isEmbeddingCircuitOpen(pID) {
			errs = append(errs, fmt.Sprintf("%s: circuit OPEN (tripped after %d non-retryable failures)", pID, h.circuitMaxFail))
		} else {
			cfg := h.GetProviderConfig(pID)
			if cfg != nil && cfg.EmbeddingModel != "" {
				p, err := providers.Get(cfg, h.Registry.ExternalRuntimes)
				if err == nil {
					emb, err := p.Embed(ctx, text)
					if err == nil {
						return emb, cfg.EmbeddingModel, nil
					}
					if tripped := h.recordEmbeddingFailure(pID, err); tripped {
						errs = append(errs, fmt.Sprintf("%s (%s): CIRCUIT TRIPPED — %v", pID, cfg.EmbeddingModel, err))
					} else {
						errs = append(errs, fmt.Sprintf("%s (%s): %v", pID, cfg.EmbeddingModel, err))
					}
				}
			}
		}
	} else {
		cfg := h.GetProviderConfig(pID)
		if cfg != nil && cfg.EmbeddingModel != "" {
			p, err := providers.Get(cfg, h.Registry.ExternalRuntimes)
			if err == nil {
				emb, err := p.Embed(ctx, text)
				if err == nil {
					return emb, cfg.EmbeddingModel, nil
				}
				errs = append(errs, fmt.Sprintf("%s (%s): %v", pID, cfg.EmbeddingModel, err))
			}
		}
	}

	// 2. Try Others from Registry
	h.Registry.Mu.RLock()
	var configs []*config.ProviderConfig
	for _, c := range h.Registry.Providers {
		if c.PoolID != pID && c.EmbeddingModel != "" {
			configs = append(configs, c)
		}
	}
	h.Registry.Mu.RUnlock()

	for _, cfg := range configs {
		if h.isEmbeddingCircuitOpen(cfg.PoolID) {
			errs = append(errs, fmt.Sprintf("%s: circuit OPEN", cfg.PoolID))
			continue
		}
		p, err := providers.Get(cfg, h.Registry.ExternalRuntimes)
		if err == nil {
			emb, err := p.Embed(ctx, text)
			if err == nil {
				return emb, cfg.EmbeddingModel, nil
			}
			if tripped := h.recordEmbeddingFailure(cfg.PoolID, err); tripped {
				errs = append(errs, fmt.Sprintf("%s (%s): CIRCUIT TRIPPED — %v", cfg.PoolID, cfg.EmbeddingModel, err))
			} else {
				errs = append(errs, fmt.Sprintf("%s (%s): %v", cfg.PoolID, cfg.EmbeddingModel, err))
			}
		}
	}

	// 3. Try Session Providers
	if h.Graph != nil {
		for _, id := range h.Graph.ListSessionProviderIDs() {
			if id == pID {
				continue
			}
			if h.isEmbeddingCircuitOpen(id) {
				continue
			}
			cfg := h.GetProviderConfig(id)
			if cfg != nil && cfg.EmbeddingModel != "" {
				p, err := providers.Get(cfg, h.Registry.ExternalRuntimes)
				if err == nil {
					emb, err := p.Embed(ctx, text)
					if err == nil {
						return emb, cfg.EmbeddingModel, nil
					}
					if tripped := h.recordEmbeddingFailure(id, err); tripped {
						errs = append(errs, fmt.Sprintf("%s (%s, session): CIRCUIT TRIPPED — %v", id, cfg.EmbeddingModel, err))
					} else {
						errs = append(errs, fmt.Sprintf("%s (%s, session): %v", id, cfg.EmbeddingModel, err))
					}
				}
			}
		}
	}

	errStr := strings.Join(errs, "; ")
	if errStr != "" {
		return nil, "", fmt.Errorf("all embedding providers failed: %s", errStr)
	}
	return nil, "", fmt.Errorf("no embedding providers configured")
}

// HubEmbeddingClient wraps ContextHub to satisfy the core.EmbeddingClient
// interface used by IntentRouter. It delegates to EmbedWithFallback, which
// provides tiered fallback so semantic SOP routing degrades gracefully.
// ADDED (2026-08-24): wiring for IntentRouter semantic routing activation.
type HubEmbeddingClient struct {
	hub *ContextHub
}

var _ EmbeddingClient = (*HubEmbeddingClient)(nil)

func NewHubEmbeddingClient(hub *ContextHub) *HubEmbeddingClient {
	return &HubEmbeddingClient{hub: hub}
}

func (c *HubEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	emb, _, err := c.hub.EmbedWithFallback(ctx, text)
	return emb, err
}

func (c *HubEmbeddingClient) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	return c.hub.EmbedWithFallback(ctx, text)
}

func (h *ContextHub) GetEmbeddingClient() store.IEmbeddingClient {
	return NewHubEmbeddingClient(h)
}
