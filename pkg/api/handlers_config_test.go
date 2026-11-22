package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// O24-3 regression: /api/config masked providers.*.api_key and returned every
// other secret in the registry verbatim. The handler is reachable on the admin
// mux, but its output is also what the console renders and what an operator
// copies into a ticket, so the mask has to cover the named keys, MCP env
// credentials, OAuth client secrets and the system bearer tokens.
func TestConfigHandler_MasksAllSecrets(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	reg := app.Registry
	reg.System.APIKeys = []config.NamedAPIKey{{Key: "named-secret", OwnerID: "alice", Tier: "admin"}}
	reg.System.SystemOne.APIKey = "systemone-secret"
	reg.System.Reranker.APIKey = "rerank-secret"
	reg.Providers["test"].APIKeys = []string{"pool-secret"}
	reg.Providers["test"].Instances = []config.ProviderInstance{{ID: "i1", APIKey: "instance-secret"}}
	reg.MCPs["svc"] = &config.MCPDef{
		ID:  "svc",
		Env: map[string]string{"GITHUB_TOKEN": "env-secret", "REGION": "us-east-1"},
		OAuthClient: &config.OAuthClientConfig{
			ClientID: "client-id-visible", ClientSecret: "oauth-secret",
			TokenEndpoint: "https://oauth.example.test/token",
		},
	}

	req, _ := http.NewRequest(http.MethodGet, "/api/config", nil)
	rr := httptest.NewRecorder()
	NewAPIHandler(app).ConfigHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, secret := range []string{"named-secret", "systemone-secret", "rerank-secret", "pool-secret", "instance-secret", "env-secret", "oauth-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("secret %q leaked by /api/config: %s", secret, body)
		}
	}
	for _, keep := range []string{"client-id-visible", "us-east-1", "alice"} {
		if !strings.Contains(body, keep) {
			t.Errorf("non-secret value %q missing from /api/config: %s", keep, body)
		}
	}
	if !strings.Contains(body, config.RedactedSecret) {
		t.Errorf("expected the redaction placeholder in /api/config, body: %s", body)
	}
}

// O24-3 follow-up: named keys had no supported write path — they were
// hand-edit-only, so a key set through the UI could not exist and an edit
// outside CommitOps would be neither attributable nor durable. The write must
// survive a fresh registry load from the same config file.
func TestUpdateSystemHandler_PersistsNamedAPIKeys(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	body := `{"api_keys":[{"key":"operator-key-1","owner_id":"alice","tier":"admin"},{"key":"reader-key-1","owner_id":"bob","tier":"public"}]}`
	req, _ := http.NewRequest(http.MethodPost, "/api/config/update-system", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	NewAPIHandler(app).UpdateSystemHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["restart_required"] != true {
		t.Errorf("api_keys write should say a restart is required, got %v", resp)
	}

	got := app.Registry.System.APIKeys
	if len(got) != 2 || got[0].Key != "operator-key-1" || got[1].Tier != "public" {
		t.Fatalf("registry did not take the keys: %+v", got)
	}

	reloaded, err := config.NewRegistry(app.ConfigPath)
	if err != nil {
		t.Fatalf("reload from %s: %v", app.ConfigPath, err)
	}
	persisted := reloaded.System.APIKeys
	if len(persisted) != 2 {
		t.Fatalf("keys did not survive a reload: %+v", persisted)
	}
	if persisted[0].Key != "operator-key-1" || persisted[0].OwnerID != "alice" || persisted[0].Tier != "admin" {
		t.Errorf("first key lost its fields in persistence: %+v", persisted[0])
	}
	if persisted[1].Key != "reader-key-1" || persisted[1].Tier != "public" {
		t.Errorf("second key lost its fields in persistence: %+v", persisted[1])
	}
}

func TestUpdateSystemHandler_RejectsMalformedNamedAPIKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"redaction placeholder", `{"api_keys":[{"key":"` + config.RedactedSecret + `","owner_id":"a","tier":"admin"}]}`},
		{"empty key", `{"api_keys":[{"key":"","owner_id":"a","tier":"admin"}]}`},
		{"unknown tier", `{"api_keys":[{"key":"k1","owner_id":"a","tier":"superuser"}]}`},
		{"duplicate key", `{"api_keys":[{"key":"dup","tier":"admin"},{"key":"dup","tier":"public"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, tmpDir := setupTestApp(t)
			defer os.RemoveAll(tmpDir)

			req, _ := http.NewRequest(http.MethodPost, "/api/config/update-system", bytes.NewBufferString(tc.body))
			rr := httptest.NewRecorder()
			NewAPIHandler(app).UpdateSystemHandler(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if n := len(app.Registry.System.APIKeys); n != 0 {
				t.Errorf("rejected request still wrote %d key(s)", n)
			}
		})
	}
}

// A partial body must not revoke existing keys: the write is presence-gated
// like every other field, and losing the key set locks the operator out.
func TestUpdateSystemHandler_OmittedAPIKeysPreservesExisting(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)
	app.Registry.System.APIKeys = []config.NamedAPIKey{{Key: "keep-me", OwnerID: "alice", Tier: "admin"}}

	body := `{"max_context_keep":42}`
	req, _ := http.NewRequest(http.MethodPost, "/api/config/update-system", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	NewAPIHandler(app).UpdateSystemHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := app.Registry.System.APIKeys
	if len(got) != 1 || got[0].Key != "keep-me" {
		t.Errorf("partial body changed the key set: %+v", got)
	}
	if strings.Contains(rr.Body.String(), "restart_required") {
		t.Errorf("response should not claim a key change: %s", rr.Body.String())
	}
}
