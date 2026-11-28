package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression: when RequireAudit=true and the audit Spawn fails,
// verifyExitCriteria must return false (fail-closed), not fall through to
// Case 4 where empty ExitCriteria silently returns true.
func TestVerifyExitCriteria_AuditSpawnFail_FailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s, step, graph := newAuditGateTestEngine(t, srv.URL, true, "gpt-4")

	result := &SpawnResult{
		Output: schemas.SubagentOutput{
			Result: map[string]any{"raw": "some output"},
			Status: schemas.StatusOK,
		},
	}

	ok, reason, failType := s.verifyExitCriteria(context.Background(), graph, step, result)
	if ok {
		t.Errorf("verifyExitCriteria returned true when audit Spawn failed; expected fail-closed (false). reason=%q failType=%q", reason, failType)
	}
	if failType != schemas.FailureLogicError {
		t.Errorf("expected FailureLogicError, got %q", failType)
	}
}

// Regression: when RequireAudit=true and ExitCriteria="", the audit
// gate must still trigger (Case 3). The mock server returns PASS, so the
// step should be accepted — proving the audit path was entered.
func TestVerifyExitCriteria_RequireAuditEmptyCriteria_EntersCase3(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"PASS"}}]}`))
	}))
	defer srv.Close()

	s, step, graph := newAuditGateTestEngine(t, srv.URL, true, "")

	result := &SpawnResult{
		Output: schemas.SubagentOutput{
			Result: map[string]any{"raw": "some output"},
			Status: schemas.StatusOK,
		},
	}

	ok, reason, _ := s.verifyExitCriteria(context.Background(), graph, step, result)
	if !ok {
		t.Errorf("verifyExitCriteria should enter Case 3 and accept on PASS, but returned false. reason=%q", reason)
	}
}

// newAuditGateTestEngine creates a minimal DirectedEngine with a Spawner whose
// provider points at srvURL. requireAudit controls step.RequireAudit;
// verifierModel controls step.VerifierModel (empty string = unset).
func newAuditGateTestEngine(t *testing.T, srvURL string, requireAudit bool, verifierModel string) (*DirectedEngine, *schemas.Step, *schemas.TaskGraph) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	cfgJSON := `{"default_provider":"p1","providers":{"p1":{"provider":"openai","model":"gpt-4","base_url":"` + srvURL + `","api_key":"fake"}},"roles":[{"id":"auditor","base_capability":"text"}]}`
	if err := os.WriteFile(configPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	reg, _ := config.NewRegistry(configPath)
	ts := store.NewTaskStore(store.NewFileTaskBackend(filepath.Join(tmpDir, "tasks")))
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })
	spawner := NewSpawner(reg, ts, nil, logger, nil, tmpDir)

	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, tmpDir, tmpDir, nil)
	s.spawner = spawner

	step := &schemas.Step{
		ID:            "s1",
		Task:          "do something",
		ExitCriteria:  "",
		RequireAudit:  requireAudit,
		VerifierModel: verifierModel,
	}
	graph := &schemas.TaskGraph{
		TaskID: "t-audit-gate",
		Steps:  map[string]*schemas.Step{"s1": step},
	}
	return s, step, graph
}
