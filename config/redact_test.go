package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// O24-3 regression: /api/config and get_config masked exactly one field
// (providers.*.api_key) and served MCP env credentials, named API keys, OAuth
// client secrets and the system bearer tokens in plaintext. The input is built
// from the typed structs on purpose: if a JSON tag is renamed, this test masks
// nothing for that field and fails with the leaked value in the message.
func TestRedactConfigSecrets_MasksEverySecretField(t *testing.T) {
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"primary": {
				Provider:  "openai",
				Model:     "gpt-4",
				BaseURL:   "https://api.example.test",
				APIKey:    "secret-provider",
				APIKeys:   []string{"secret-pool-1", "secret-pool-2"},
				Instances: []ProviderInstance{{ID: "i1", APIKey: "secret-instance"}},
				FallbackChain: []ProviderConfig{{
					Provider: "anthropic", Model: "claude", APIKey: "secret-fallback",
				}},
			},
		},
		GlobalFallback: []ProviderConfig{{Provider: "gemini", Model: "flash", APIKey: "secret-global"}},
		MCPs: []MCPDef{{
			ID: "svc",
			Env: map[string]string{
				"GITHUB_TOKEN":          "secret-github",
				"AWS_SECRET_ACCESS_KEY": "secret-aws",
				"DB_PASSWORD":           "secret-db",
				"PATH":                  "/usr/bin",
				"REGION":                "us-east-1",
			},
			OAuthClient: &OAuthClientConfig{
				ClientID:      "client-id-visible",
				ClientSecret:  "secret-oauth",
				TokenEndpoint: "https://oauth.example.test/token",
			},
		}},
		System: SystemSettings{
			APIKeys:   []NamedAPIKey{{Key: "secret-named", OwnerID: "alice", Tier: "admin"}},
			Reranker:  RerankerConfig{BaseURL: "http://127.0.0.1:8000", Model: "laya", APIKey: "secret-rerank"},
			SystemOne: SystemOneConfig{BaseURL: "http://127.0.0.1:8000", Model: "jev", APIKey: "secret-systemone"},
		},
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var shaped map[string]any
	if err := json.Unmarshal(raw, &shaped); err != nil {
		t.Fatal(err)
	}
	RedactConfigSecrets(shaped)
	out, err := json.Marshal(shaped)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)

	secrets := []string{
		"secret-provider", "secret-pool-1", "secret-pool-2", "secret-instance",
		"secret-fallback", "secret-global", "secret-github", "secret-aws",
		"secret-db", "secret-oauth", "secret-named", "secret-rerank",
		"secret-systemone",
	}
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("secret %q still present after redaction: %s", s, text)
		}
	}
	if got, want := strings.Count(text, RedactedSecret), len(secrets); got != want {
		t.Errorf("masked %d values, want %d: %s", got, want, text)
	}
	for _, keep := range []string{"client-id-visible", "/usr/bin", "us-east-1", "alice", "https://api.example.test"} {
		if !strings.Contains(text, keep) {
			t.Errorf("non-secret value %q was masked away: %s", keep, text)
		}
	}
}

// /api/config marshals Registry, where mcps is keyed by id; config.Config
// marshals it as an array. The redactor has to accept both.
func TestRedactConfigSecrets_RegistryShapeMasksMCPMap(t *testing.T) {
	m := map[string]any{
		"providers": map[string]any{"p": map[string]any{"api_key": "reg-provider"}},
		"mcps": map[string]any{
			"svc": map[string]any{
				"env":          map[string]any{"API_TOKEN": "reg-token", "MODE": "fast"},
				"oauth_client": map[string]any{"client_secret": "reg-oauth"},
			},
		},
		"system": map[string]any{
			"api_keys":  []any{map[string]any{"key": "reg-named", "owner_id": "bob", "tier": "public"}},
			"reranker":  map[string]any{"api_key": "reg-rerank"},
			"systemone": map[string]any{"api_key": "reg-systemone"},
		},
	}
	RedactConfigSecrets(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, s := range []string{"reg-provider", "reg-token", "reg-oauth", "reg-named", "reg-rerank", "reg-systemone"} {
		if strings.Contains(text, s) {
			t.Errorf("secret %q still present after redaction: %s", s, text)
		}
	}
	for _, keep := range []string{"fast", "bob", "public"} {
		if !strings.Contains(text, keep) {
			t.Errorf("non-secret value %q was masked away: %s", keep, text)
		}
	}
}

func TestRedactConfigSecrets_LeavesEmptySecretsEmpty(t *testing.T) {
	m := map[string]any{
		"providers": map[string]any{"p": map[string]any{"api_key": ""}},
		"mcps":      map[string]any{"svc": map[string]any{"env": map[string]any{"EMPTY_TOKEN": ""}}},
		"system":    map[string]any{"api_keys": []any{map[string]any{"key": ""}}},
	}
	RedactConfigSecrets(m)

	p := m["providers"].(map[string]any)["p"].(map[string]any)
	if p["api_key"] != "" {
		t.Errorf("empty provider key should stay empty, got %v", p["api_key"])
	}
	env := m["mcps"].(map[string]any)["svc"].(map[string]any)["env"].(map[string]any)
	if env["EMPTY_TOKEN"] != "" {
		t.Errorf("empty env value should stay empty, got %v", env["EMPTY_TOKEN"])
	}
	nk := m["system"].(map[string]any)["api_keys"].([]any)[0].(map[string]any)
	if nk["key"] != "" {
		t.Errorf("empty named key should stay empty, got %v", nk["key"])
	}
}

func TestRedactConfigSecrets_AbsentSectionsDoNotPanic(t *testing.T) {
	RedactConfigSecrets(nil)
	RedactConfigSecrets(map[string]any{})
	RedactConfigSecrets(map[string]any{"providers": "not-a-map", "mcps": 42, "system": nil})
}

func TestLooksSecretEnvName(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "DB_PASSWORD", "PASSWD", "SERVICE_API_KEY", "CLIENT_CREDENTIALS", "PRIVATE_DATA", "AUTH_HEADER"} {
		if !looksSecretEnvName(name) {
			t.Errorf("%q should be treated as a secret", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "LANG", "REGION", "LOG_LEVEL", "BASE_URL"} {
		if looksSecretEnvName(name) {
			t.Errorf("%q should pass through", name)
		}
	}
}
