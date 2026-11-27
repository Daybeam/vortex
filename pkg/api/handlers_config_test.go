package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/tools"
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
	if persisted[1].Key != "reader-key-1" || persisted[1].OwnerID != "bob" || persisted[1].Tier != "public" {
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
		{"public key without owner", `{"api_keys":[{"key":"k1","tier":"public"}]}`},
		{"default public tier without owner", `{"api_keys":[{"key":"k1"}]}`},
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

type o24ConfigAssetHandler func(*APIHandler, http.ResponseWriter, *http.Request)

type o24ConfigAssetCase struct {
	name      string
	path      string
	sidecar   string
	handle    o24ConfigAssetHandler
	newBody   func(string) []byte
	isPresent func(*config.Registry, string) bool
}

func o24ConfigAssetCases() []o24ConfigAssetCase {
	return []o24ConfigAssetCase{
		{
			name: "skill", path: "/skills/", sidecar: "skills",
			handle: (*APIHandler).UpdateSkillHandler,
			newBody: func(id string) []byte {
				b, _ := json.Marshal(map[string]any{"id": id, "name": "O24 skill", "description": "durable skill"})
				return b
			},
			isPresent: func(r *config.Registry, id string) bool { _, ok := r.Skills[id]; return ok },
		},
		{
			name: "sop", path: "/sops/", sidecar: "sops",
			handle: (*APIHandler).UpdateSOPHandler,
			newBody: func(id string) []byte {
				b, _ := json.Marshal(map[string]any{"id": id, "description": "durable sop", "steps": map[string]any{}})
				return b
			},
			isPresent: func(r *config.Registry, id string) bool { _, ok := r.SOPs[id]; return ok },
		},
	}
}

func readO24WALCommits(t *testing.T, path string) []config.Commit {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	var commits []config.Commit
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var commit config.Commit
		if err := json.Unmarshal(line, &commit); err != nil {
			t.Fatalf("decode WAL commit: %v", err)
		}
		commits = append(commits, commit)
	}
	return commits
}

// O24-4 regression: skill/SOP updates bypassed CommitOps, so they had no WAL
// record, no caller attribution, and no restart durability. Pin add/replace,
// owner/fallback actors, sidecars, and WAL replay through a fresh registry.
func TestO24ConfigAssetHandlersWALCommit(t *testing.T) {
	for _, tc := range o24ConfigAssetCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VORTEX_WAL_ENABLED", "true")
			app, tmpDir := setupTestApp(t)
			defer os.RemoveAll(tmpDir)
			h := NewAPIHandler(app)
			id := "o24_" + tc.name

			for i, step := range []struct{ actor, op string }{{"tenant-a", config.OpAdd}, {"", config.OpReplace}} {
				req := httptest.NewRequest(http.MethodPost, "/api/config/update-"+tc.name, bytes.NewReader(tc.newBody(id)))
				if step.actor != "" {
					req = req.WithContext(context.WithValue(req.Context(), tools.AuthOwnerKey, step.actor))
				}
				rr := httptest.NewRecorder()
				tc.handle(h, rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("update %d: expected 200, got %d: %s", i, rr.Code, rr.Body.String())
				}
				if !tc.isPresent(app.Registry, id) {
					t.Fatalf("update %d did not reach registry", i)
				}

				commits := readO24WALCommits(t, app.ConfigPath+".wal")
				if len(commits) != i+1 {
					t.Fatalf("update %d: WAL commits = %d, want %d", i, len(commits), i+1)
				}
				commit := commits[i]
				wantActor := step.actor
				if wantActor == "" {
					wantActor = "api_user"
				}
				if commit.Actor != wantActor || commit.TaskID != "" {
					t.Errorf("update %d: actor/task = %q/%q, want %q/empty", i, commit.Actor, commit.TaskID, wantActor)
				}
				if len(commit.Ops) != 1 || commit.Ops[0].Op != step.op || commit.Ops[0].Path != tc.path+id {
					t.Fatalf("update %d: ops = %+v, want %s %s", i, commit.Ops, step.op, tc.path+id)
				}
				var value map[string]any
				if err := json.Unmarshal(commit.Ops[0].Value, &value); err != nil || value["id"] != id {
					t.Errorf("update %d: invalid patch value %s: %v", i, commit.Ops[0].Value, err)
				}
				sidecar := filepath.Join(tmpDir, "workspace", tc.sidecar, id+".json")
				if _, err := os.Stat(sidecar); err != nil {
					t.Errorf("update %d: runtime sidecar missing: %v", i, err)
				}
			}

			reloaded, err := config.NewRegistry(app.ConfigPath)
			if err != nil {
				t.Fatalf("reload registry: %v", err)
			}
			if !tc.isPresent(reloaded, id) {
				t.Error("update did not survive restart through WAL replay")
			}
		})
	}
}

// O24-4 regression: filepath.Base silently accepted traversal/multi-component
// IDs while the registry key retained the unsafe input. Reject before any write.
func TestO24ConfigAssetHandlersRejectUnsafeIDs(t *testing.T) {
	for _, tc := range o24ConfigAssetCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []string{"", "../escape", "nested/id", `nested\id`, ".", "..", "/absolute", `C:\escape`, "bad id", "bad~id", "bad%2fid"} {
				t.Run(id, func(t *testing.T) {
					t.Setenv("VORTEX_WAL_ENABLED", "true")
					app, tmpDir := setupTestApp(t)
					defer os.RemoveAll(tmpDir)
					req := httptest.NewRequest(http.MethodPost, "/api/config/update-"+tc.name, bytes.NewReader(tc.newBody(id)))
					rr := httptest.NewRecorder()
					tc.handle(NewAPIHandler(app), rr, req)
					if rr.Code != http.StatusBadRequest {
						t.Fatalf("ID %q: expected 400, got %d: %s", id, rr.Code, rr.Body.String())
					}
					if tc.isPresent(app.Registry, id) {
						t.Errorf("ID %q reached registry", id)
					}
					if _, err := os.Stat(app.ConfigPath + ".wal"); !os.IsNotExist(err) {
						t.Errorf("ID %q created a WAL record: %v", id, err)
					}
					if _, err := os.Stat(filepath.Join(tmpDir, "workspace")); !os.IsNotExist(err) {
						t.Errorf("ID %q created sidecar files: %v", id, err)
					}
				})
			}
		})
	}
}

// O24-4 regression: a failed WAL append must not produce a success response or
// a runtime sidecar. CommitOps itself currently applies memory before append;
// this test deliberately asserts the HTTP/persistence boundary, not rollback.
func TestO24ConfigAssetHandlersPersistenceFailure(t *testing.T) {
	for _, tc := range o24ConfigAssetCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VORTEX_WAL_ENABLED", "true")
			app, tmpDir := setupTestApp(t)
			defer os.RemoveAll(tmpDir)
			if err := os.Mkdir(app.ConfigPath+".wal", 0755); err != nil {
				t.Fatal(err)
			}
			id := "o24_" + tc.name
			req := httptest.NewRequest(http.MethodPost, "/api/config/update-"+tc.name, bytes.NewReader(tc.newBody(id)))
			rr := httptest.NewRecorder()
			tc.handle(NewAPIHandler(app), rr, req)
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("expected persistence failure status, got %d: %s", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), `"ok":true`) {
				t.Errorf("failure response claims success: %s", rr.Body.String())
			}
			if _, err := os.Stat(filepath.Join(tmpDir, "workspace", tc.sidecar, id+".json")); !os.IsNotExist(err) {
				t.Errorf("failed persistence wrote a sidecar: %v", err)
			}
		})
	}
}
