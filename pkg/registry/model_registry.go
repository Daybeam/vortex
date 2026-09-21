package registry

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed models_embedded.json
var embeddedModelsJSON []byte

// ExternalModelProvider is a pluggable interface for fetching model metadata
// from an external source (e.g., enterprise gateway, LiteLLM sync service).
type ExternalModelProvider interface {
	FetchLatestMetadata(modelName string) (*ModelCapabilities, error)
}

// ModelRegistry resolves model capabilities via a three-tier fallback:
// 1. User overrides (config.json) — highest priority, never auto-overwritten
// 2. Runtime cache (dynamic/plugin-fetched)
// 3. Embedded static table (go:embed JSON) — offline fallback
// 4. Hardcoded safe defaults
//
// See MODEL_REGISTRY_PLUGGABLE_ARCHITECTURE.md §2.
type ModelRegistry struct {
	mu               sync.RWMutex
	embedded         map[string]ModelCapabilities
	runtimeCache     map[string]ModelCapabilities
	userOverrides    map[string]ModelCapabilities
	externalProvider ExternalModelProvider
}

// NewModelRegistry creates a registry with the embedded dataset pre-loaded.
func NewModelRegistry() *ModelRegistry {
	r := &ModelRegistry{
		embedded:      make(map[string]ModelCapabilities),
		runtimeCache:  make(map[string]ModelCapabilities),
		userOverrides: make(map[string]ModelCapabilities),
	}
	r.loadEmbedded()
	return r
}

func (r *ModelRegistry) loadEmbedded() {
	var list []ModelCapabilities
	if err := json.Unmarshal(embeddedModelsJSON, &list); err == nil {
		for _, m := range list {
			r.embedded[strings.ToLower(m.ModelName)] = m
		}
	}
}

// GetCapabilities resolves model capabilities following the strict 3-tier
// precedence: user override → runtime cache → embedded → external provider
// → safe defaults.
func (r *ModelRegistry) GetCapabilities(modelName string) ModelCapabilities {
	key := strings.ToLower(modelName)

	r.mu.RLock()
	if cap, ok := r.userOverrides[key]; ok {
		r.mu.RUnlock()
		return cap
	}
	if cap, ok := r.runtimeCache[key]; ok {
		r.mu.RUnlock()
		return cap
	}
	if cap, ok := r.embedded[key]; ok {
		r.mu.RUnlock()
		return cap
	}
	r.mu.RUnlock()

	r.mu.RLock()
	provider := r.externalProvider
	r.mu.RUnlock()

	if provider != nil {
		if remoteCap, err := provider.FetchLatestMetadata(modelName); err == nil && remoteCap != nil {
			r.mu.Lock()
			r.runtimeCache[key] = *remoteCap
			r.mu.Unlock()
			return *remoteCap
		}
	}

	return ModelCapabilities{
		ModelName:        modelName,
		MaxContextWindow: 8192,
		MaxOutputTokens:  4096,
		SupportsToolUse:  true,
	}
}

// SetUserOverride registers a human-explicit capability override.
// This always takes precedence over embedded/runtime data.
func (r *ModelRegistry) SetUserOverride(caps ModelCapabilities) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userOverrides[strings.ToLower(caps.ModelName)] = caps
}

// SetExternalProvider mounts a pluggable external metadata source.
func (r *ModelRegistry) SetExternalProvider(provider ExternalModelProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.externalProvider = provider
}

// ListEmbedded returns all models in the embedded static table.
func (r *ModelRegistry) ListEmbedded() []ModelCapabilities {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]ModelCapabilities, 0, len(r.embedded))
	for _, m := range r.embedded {
		result = append(result, m)
	}
	return result
}
