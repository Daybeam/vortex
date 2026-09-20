package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func newSessionTestLogger() *Logger {
	dir, _ := os.MkdirTemp("", "session_test_log_*")
	logger, _ := NewLogger(dir, &config.SystemSettings{})
	return logger
}

func TestSessionContext_IsExpired(t *testing.T) {
	sc := &SessionContext{SessionID: "s1", Workspace: "/tmp"}
	if sc.IsExpired() {
		t.Error("zero ExpiresAt should never expire")
	}

	sc.ExpiresAt = time.Now().Add(-time.Hour)
	if !sc.IsExpired() {
		t.Error("past ExpiresAt should be expired")
	}

	sc.ExpiresAt = time.Now().Add(time.Hour)
	if sc.IsExpired() {
		t.Error("future ExpiresAt should not be expired")
	}
}

func TestSessionManager_CreateOrBind_PermissiveMode(t *testing.T) {
	dir, _ := os.MkdirTemp("", "session_test_*")
	defer os.RemoveAll(dir)

	mgr := NewSessionManager(nil, 0)
	sc, err := mgr.CreateOrBind("sess1", dir)
	if err != nil {
		t.Fatalf("CreateOrBind: %v", err)
	}
	if sc.SessionID != "sess1" {
		t.Errorf("SessionID = %s, want sess1", sc.SessionID)
	}
	if sc.Workspace != dir {
		t.Errorf("Workspace = %s, want %s", sc.Workspace, dir)
	}
}

func TestSessionManager_CreateOrBind_RestrictedMode(t *testing.T) {
	allowed, _ := os.MkdirTemp("", "allowed_*")
	defer os.RemoveAll(allowed)
	disallowed, _ := os.MkdirTemp("", "disallowed_*")
	defer os.RemoveAll(disallowed)

	mgr := NewSessionManager([]string{allowed}, 0)

	_, err := mgr.CreateOrBind("sess1", allowed)
	if err != nil {
		t.Fatalf("allowed workspace should pass: %v", err)
	}

	_, err = mgr.CreateOrBind("sess2", disallowed)
	if err == nil {
		t.Fatal("disallowed workspace should be rejected")
	}
}

func TestSessionManager_CreateOrBind_Idempotent(t *testing.T) {
	dir, _ := os.MkdirTemp("", "session_test_*")
	defer os.RemoveAll(dir)

	mgr := NewSessionManager(nil, 0)
	sc1, _ := mgr.CreateOrBind("sess1", dir)
	sc2, err := mgr.CreateOrBind("sess1", dir)
	if err != nil {
		t.Fatalf("second CreateOrBind: %v", err)
	}
	if sc1.CreatedAt != sc2.CreatedAt {
		t.Error("idempotent rebind should return same session")
	}
}

func TestSessionManager_Lookup(t *testing.T) {
	dir, _ := os.MkdirTemp("", "session_test_*")
	defer os.RemoveAll(dir)

	mgr := NewSessionManager(nil, 0)
	_, _ = mgr.CreateOrBind("sess1", dir)

	sc := mgr.Lookup("sess1")
	if sc == nil {
		t.Fatal("expected session, got nil")
	}
	if sc.SessionID != "sess1" {
		t.Errorf("SessionID = %s, want sess1", sc.SessionID)
	}

	if mgr.Lookup("nonexistent") != nil {
		t.Error("nonexistent session should return nil")
	}
}

func TestSessionManager_Lookup_Expired(t *testing.T) {
	dir, _ := os.MkdirTemp("", "session_test_*")
	defer os.RemoveAll(dir)

	mgr := NewSessionManager(nil, 100*time.Millisecond)
	_, _ = mgr.CreateOrBind("sess1", dir)

	time.Sleep(150 * time.Millisecond)
	if mgr.Lookup("sess1") != nil {
		t.Error("expired session should return nil")
	}
}

func TestSessionManager_EmptySessionID(t *testing.T) {
	mgr := NewSessionManager(nil, 0)
	_, err := mgr.CreateOrBind("", "/tmp")
	if err == nil {
		t.Error("empty session_id should be rejected")
	}
}

func TestSeedTaskInputs_NoWorkspaceRoot(t *testing.T) {
	engine := &DirectedEngine{
		logger: newSessionTestLogger(),
	}
	graph := &schemas.TaskGraph{
		TaskID: "task1",
		Steps:  map[string]*schemas.Step{"s1": {ID: "s1", InputDir: "data"}},
	}
	err := engine.seedTaskInputs(graph, os.TempDir())
	if err != nil {
		t.Fatalf("should be no-op with empty WorkspaceRoot: %v", err)
	}
}

func TestSeedTaskInputs_NoInputDir(t *testing.T) {
	engine := &DirectedEngine{
		logger: newSessionTestLogger(),
	}
	graph := &schemas.TaskGraph{
		TaskID:        "task1",
		WorkspaceRoot: os.TempDir(),
		Steps:         map[string]*schemas.Step{"s1": {ID: "s1"}},
	}
	err := engine.seedTaskInputs(graph, os.TempDir())
	if err != nil {
		t.Fatalf("should be no-op with no InputDir: %v", err)
	}
}

func TestSeedTaskInputs_CopiesFiles(t *testing.T) {
	wsRoot, _ := os.MkdirTemp("", "wsroot_*")
	defer os.RemoveAll(wsRoot)
	outputBase, _ := os.MkdirTemp("", "outputbase_*")
	defer os.RemoveAll(outputBase)

	srcDir := filepath.Join(wsRoot, "input_data")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(srcDir, "file2.txt"), []byte("world"), 0644)

	engine := &DirectedEngine{
		logger: newSessionTestLogger(),
	}
	graph := &schemas.TaskGraph{
		TaskID:        "task1",
		WorkspaceRoot: wsRoot,
		Steps:         map[string]*schemas.Step{"s1": {ID: "s1", InputDir: "input_data"}},
	}

	err := engine.seedTaskInputs(graph, outputBase)
	if err != nil {
		t.Fatalf("seedTaskInputs: %v", err)
	}

	dstFile1 := filepath.Join(outputBase, "task1", "workspace", "input_data", "file1.txt")
	data, err := os.ReadFile(dstFile1)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", dstFile1, err)
	}
	if string(data) != "hello" {
		t.Errorf("file1 content = %q, want 'hello'", string(data))
	}

	dstFile2 := filepath.Join(outputBase, "task1", "workspace", "input_data", "file2.txt")
	data, err = os.ReadFile(dstFile2)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", dstFile2, err)
	}
	if string(data) != "world" {
		t.Errorf("file2 content = %q, want 'world'", string(data))
	}
}

func TestSeedTaskInputs_SkipsMissingSource(t *testing.T) {
	wsRoot, _ := os.MkdirTemp("", "wsroot_*")
	defer os.RemoveAll(wsRoot)
	outputBase, _ := os.MkdirTemp("", "outputbase_*")
	defer os.RemoveAll(outputBase)

	engine := &DirectedEngine{
		logger: newSessionTestLogger(),
	}
	graph := &schemas.TaskGraph{
		TaskID:        "task1",
		WorkspaceRoot: wsRoot,
		Steps:         map[string]*schemas.Step{"s1": {ID: "s1", InputDir: "nonexistent_dir"}},
	}

	err := engine.seedTaskInputs(graph, outputBase)
	if err != nil {
		t.Fatalf("missing source should be skipped, not error: %v", err)
	}
}
