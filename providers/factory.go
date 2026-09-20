package providers

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/pkg/ratelimit"
)

var (
	cache = make(map[string]interfaces.Provider)
	mu    sync.Mutex
)

func ClearCache() {
	mu.Lock()
	defer mu.Unlock()
	cache = make(map[string]interfaces.Provider)
}

// providerCacheKey computes the cache/router-slot key for a ProviderConfig.
// Extracted as a shared helper (2026-07-11) after Get() and
// ReloadScriptProvider previously duplicated this formula and drifted out of
// sync when APIKey was added to the key -- ReloadScriptProvider kept using
// the old 3-part formula, which would have silently failed to find any
// script provider whose cfg had a non-empty direct APIKey set. Any future
// change to what distinguishes a provider instance should only need to touch
// this one function.
func providerCacheKey(cfg *config.ProviderConfig) string {
	// NOTE (2026-07-11): key includes cfg.APIKey (not just APIKeyEnv) so that
	// two multi-instance ProviderConfig.Instances entries that both set a
	// direct api_key (rather than api_key_env) don't collide into the same
	// cache/router slot. Harmless for the common case where APIKey is empty
	// and instances are distinguished by APIKeyEnv instead.
	return fmt.Sprintf("%s::%s::%s::%s", cfg.Provider, cfg.BaseURL, cfg.APIKeyEnv, cfg.APIKey)
}

// PoolIDFor returns the GlobalRouter failover pool identifier for cfg.
// ADDED (2026-07-19) alongside the ProviderRouter.Next pool-matching fix:
// prefers cfg.PoolID (set by Registry.Load()/register_provider/
// RegisterConfiguredInstances -- see PoolID's doc comment in config.go),
// falling back to providerCacheKey for any ProviderConfig constructed
// outside those paths (e.g. in tests) that never got a PoolID assigned.
// Callers resolving a provider for a task/step should use this instead of
// the bare cfg.Provider type string when calling GlobalRouter.Next.
func PoolIDFor(cfg *config.ProviderConfig) string {
	if cfg.PoolID != "" {
		return cfg.PoolID
	}
	return providerCacheKey(cfg)
}

func Get(cfg *config.ProviderConfig, runtimes config.ExternalRuntimes) (interfaces.Provider, error) {
	key := providerCacheKey(cfg)
	mu.Lock()
	p, ok := cache[key]
	mu.Unlock()
	if ok {
		return p, nil
	}
	// Check GlobalRouter for a pre-registered provider (e.g., test mocks)
	if p := GlobalRouter.LookupByConfig(key); p != nil {
		mu.Lock()
		cache[key] = p
		mu.Unlock()
		return p, nil
	}
	pNew, err := newProvider(cfg, runtimes)
	if err != nil {
		return nil, err
	}
	// Wrap with exponential backoff retry (3 attempts by default)
	rp := &RetryingProvider{Base: pNew, MaxRetries: 3, SlotID: key}

	// Wrap with proactive rate limiting if configured (ADDED 2026-08-26)
	var finalProvider interfaces.Provider = rp
	if cfg.RateLimit != nil && cfg.RateLimit.RequestsPerMinute > 0 {
		limiter := ratelimit.NewLeakyBucket(cfg.RateLimit.RequestsPerMinute)
		finalProvider = &RateLimitedProvider{Base: rp, Limiter: limiter}
	}

	mu.Lock()
	if existing, ok := cache[key]; ok {
		mu.Unlock()
		return existing, nil
	}
	cache[key] = finalProvider
	mu.Unlock()

	// Register in the GlobalRouter for load balancing
	GlobalRouter.Register(key, cfg, finalProvider)

	// Wrap with FallbackProvider if an explicit fallback chain is configured.
	// This is complementary to the router's automatic pool-based failover:
	// FallbackProvider does ordered sequential failover (try A, then B, then C)
	// while the router does round-robin + health detection within a pool.
	if len(cfg.FallbackChain) > 0 {
		chain := make([]Provider, 0, len(cfg.FallbackChain)+1)
		chain = append(chain, finalProvider)
		for _, fbCfg := range cfg.FallbackChain {
			fbProvider, fbErr := Get(&fbCfg, runtimes)
			if fbErr != nil {
				continue // skip misconfigured fallbacks
			}
			chain = append(chain, fbProvider)
		}
		if len(chain) > 1 {
			finalProvider = &FallbackProvider{Providers: chain}
		}
	}

	return finalProvider, nil
}

func newProvider(cfg *config.ProviderConfig, runtimes config.ExternalRuntimes) (Provider, error) {
	proto := cfg.Protocol
	if proto == "" {
		proto = cfg.Provider
	}
	brand := cfg.Provider

	switch proto {
	case "anthropic":
		return &AnthropicProvider{cfg: cfg, name: brand, client: &http.Client{Timeout: 120 * time.Second}}, nil
	case "openai", "deepseek", "sensenova", "sentimes", "together", "groq":
		return &OpenAIProvider{cfg: cfg, name: brand, client: &http.Client{Timeout: 300 * time.Second}}, nil
	case "gemini", "gemma":
		return &GeminiProvider{cfg: cfg, name: brand, client: &http.Client{Timeout: 120 * time.Second}}, nil
	case "ollama":
		return &OllamaProvider{cfg: cfg, name: brand, client: &http.Client{Timeout: 300 * time.Second}}, nil
	case "chinamobile":
		return &ChinaMobileProvider{cfg: cfg, name: brand, client: &http.Client{Timeout: 120 * time.Second}}, nil
	case "script":
		scriptPath, _ := cfg.Extra["script_path"].(string)
		if scriptPath == "" {
			return nil, fmt.Errorf("script provider requires extra.script_path")
		}
		return NewScriptProvider(cfg, scriptPath)
	case "external":
		scriptPath, _ := cfg.Extra["script_path"].(string)
		if scriptPath == "" {
			return nil, fmt.Errorf("external provider requires extra.script_path")
		}
		return NewExternalScriptProvider(cfg, scriptPath, runtimes)
	case "swarm":
		return nil, fmt.Errorf("swarm provider is not available")
	case "host":
		return &HostProvider{name: brand}, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (supported: anthropic, openai, gemini, ollama, chinamobile, script, external, host)", cfg.Provider)
	}
}

// ReloadScriptProvider reloads a script provider by its cache key name.
// Returns an error if the provider is not found or is not a script provider.
func ReloadScriptProvider(name string, cfg *config.ProviderConfig) error {
	key := providerCacheKey(cfg)
	mu.Lock()
	defer mu.Unlock()
	p, ok := cache[key]
	if !ok {
		return fmt.Errorf("provider %q not loaded", name)
	}
	sp, ok := p.(*ScriptProvider)
	if !ok {
		return fmt.Errorf("provider %q is not a script provider", name)
	}
	// Remove from cache so next Get() rebuilds
	delete(cache, key)
	return sp.Reload()
}

// GetScriptProviders returns metadata about all currently loaded script providers.
func GetScriptProviders() []map[string]any {
	var result []map[string]any
	mu.Lock()
	defer mu.Unlock()
	for key, p := range cache {
		if sp, ok := p.(*ScriptProvider); ok {
			result = append(result, map[string]any{
				"cache_key":   key,
				"name":        sp.Name(),
				"script_path": sp.ScriptPath(),
				"last_loaded": sp.LastLoaded().Format(time.RFC3339),
			})
		}
	}
	return result
}
