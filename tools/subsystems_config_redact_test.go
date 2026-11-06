package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// O24-3 regression: config.get_config is in tier1AllowedActions, so a
// public-tier caller can invoke it, and it used to mask only provider
// api_key. Every secret in the config file is asserted absent from the
// response, and the request is made as TierPublic on purpose.
func TestGetConfig_RedactsSecretsForPublicTier(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
	  "default_provider": "p",
	  "providers": {
	    "p": {
	      "provider": "openai", "model": "gpt-4",
	      "api_key": "prov-secret",
	      "api_keys": ["pool-secret"],
	      "instances": [{"id": "i1", "api_key": "inst-secret"}]
	    }
	  },
	  "mcps": [{
	    "id": "svc",
	    "env": {"GITHUB_TOKEN": "env-secret", "REGION": "us-east-1"},
	    "oauth_client": {"client_id": "cid", "client_secret": "oauth-secret", "token_endpoint": "https://x"}
	  }],
	  "system": {
	    "api_keys": [{"key": "named-secret", "owner_id": "alice", "tier": "admin"}],
	    "systemone": {"api_key": "systemone-secret"},
	    "reranker": {"api_key": "rerank-secret"}
	  }
	}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	reg, err := config.NewRegistry(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{Registry: reg, Tier: TierPublic}
	registerConfigSubsystem(app)
	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)
	result, err := registry.Invoke(ctx, app, "config", "get_config", nil)
	if err != nil {
		t.Fatalf("public tier must be able to read config (that is the premise): %v", err)
	}
	out, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)

	for _, secret := range []string{"prov-secret", "pool-secret", "inst-secret", "env-secret", "oauth-secret", "named-secret", "systemone-secret", "rerank-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("secret %q leaked by get_config: %s", secret, body)
		}
	}
	for _, keep := range []string{"us-east-1", "cid", "alice"} {
		if !strings.Contains(body, keep) {
			t.Errorf("non-secret value %q missing from get_config: %s", keep, body)
		}
	}
	if !strings.Contains(body, config.RedactedSecret) {
		t.Errorf("expected the redaction placeholder in get_config, body: %s", body)
	}
}
