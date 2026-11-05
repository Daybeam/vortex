package config

import "strings"

// RedactedSecret is what both config read paths return in place of a secret.
// UpdateSystemHandler refuses it for system.api_keys, so a GET→POST round-trip
// cannot replace a live credential with the placeholder.
const RedactedSecret = "***configured***"

// RedactConfigSecrets masks secret-bearing values inside a JSON-shaped config
// map, in place. It accepts both shapes this repo produces: the map built by
// /api/config (mcps keyed by id) and the map from marshalling config.Config
// (mcps is an array).
//
// audit O24-3: /api/config masked only providers.*.api_key, and get_config did
// the same, so named API keys, MCP env credentials, OAuth client secrets and
// the systemone/reranker bearer tokens were served in plaintext — including to
// public-tier SSE callers, because tools/subsystems.go admits config.get_config.
//
// Not derivable, therefore not masked: credentials embedded in a URL's userinfo
// or query string, provider `extra` maps, and notification target URLs (which
// can themselves be secrets). Those need a dedicated field, not a heuristic.
func RedactConfigSecrets(cfg map[string]any) {
	if cfg == nil {
		return
	}
	if providers, ok := cfg["providers"].(map[string]any); ok {
		for _, raw := range providers {
			if p, ok := raw.(map[string]any); ok {
				redactProvider(p)
			}
		}
	}
	redactProviderList(cfg["global_fallback"])
	if sys, ok := cfg["system"].(map[string]any); ok {
		redactSystemSettings(sys)
	}
	redactMCPs(cfg["mcps"])
}

func redactProvider(p map[string]any) {
	maskString(p, "api_key")
	if keys, ok := p["api_keys"].([]any); ok {
		for i, raw := range keys {
			if s, ok := raw.(string); ok && s != "" {
				keys[i] = RedactedSecret
			}
		}
	}
	if instances, ok := p["instances"].([]any); ok {
		for _, raw := range instances {
			if inst, ok := raw.(map[string]any); ok {
				maskString(inst, "api_key")
			}
		}
	}
	redactProviderList(p["fallback_chain"])
}

func redactProviderList(raw any) {
	list, ok := raw.([]any)
	if !ok {
		return
	}
	for _, item := range list {
		if p, ok := item.(map[string]any); ok {
			redactProvider(p)
		}
	}
}

func redactSystemSettings(sys map[string]any) {
	if keys, ok := sys["api_keys"].([]any); ok {
		for _, raw := range keys {
			if nk, ok := raw.(map[string]any); ok {
				maskString(nk, "key")
			}
		}
	}
	for _, section := range []string{"reranker", "systemone"} {
		if s, ok := sys[section].(map[string]any); ok {
			maskString(s, "api_key")
		}
	}
}

func redactMCPs(raw any) {
	switch v := raw.(type) {
	case map[string]any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				redactMCP(m)
			}
		}
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				redactMCP(m)
			}
		}
	}
}

func redactMCP(m map[string]any) {
	if env, ok := m["env"].(map[string]any); ok {
		for name, raw := range env {
			if s, ok := raw.(string); ok && s != "" && looksSecretEnvName(name) {
				env[name] = RedactedSecret
			}
		}
	}
	if oc, ok := m["oauth_client"].(map[string]any); ok {
		maskString(oc, "client_secret")
	}
}

func maskString(m map[string]any, key string) {
	if s, ok := m[key].(string); ok && s != "" {
		m[key] = RedactedSecret
	}
}

// looksSecretEnvName is deliberately broad: hiding a harmless value costs a
// debug detail, missing one leaks a credential. Names without a marker (PATH,
// HOME, LANG, ...) pass through so the env block stays useful.
func looksSecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSW", "CREDENTIAL", "PRIVATE", "AUTH"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}
