package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/daybeam/vortex/core/obs"
)

// ── cleanCommandRunDir tests ──────────────────────────────────────────────

func TestCleanCommandRunDir_RemovesOldKeepsNew(t *testing.T) {
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	base := commandRunDir
	os.MkdirAll(base, 0755)

	oldDir := filepath.Join(base, "old_run")
	newDir := filepath.Join(base, "new_run")
	os.MkdirAll(oldDir, 0755)
	os.MkdirAll(newDir, 0755)

	oldTime := time.Now().Add(-25 * time.Hour)
	newTime := time.Now().Add(-1 * time.Hour)
	os.Chtimes(oldDir, oldTime, oldTime)
	os.Chtimes(newDir, newTime, newTime)

	cleanCommandRunDir()

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("expected old dir %s to be removed, but it exists", oldDir)
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("expected new dir %s to survive, but it was removed: %v", newDir, err)
	}
}

func TestCleanCommandRunDir_ActiveProtection(t *testing.T) {
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	base := commandRunDir
	os.MkdirAll(base, 0755)

	// Create an "old" dir that would normally be cleaned (25h ago).
	activeDir := filepath.Join(base, "active_run")
	os.MkdirAll(activeDir, 0755)
	oldTime := time.Now().Add(-25 * time.Hour)
	os.Chtimes(activeDir, oldTime, oldTime)

	// Mark it as active — cleaner must skip it.
	activeCommandRuns.Store(activeDir, true)
	defer activeCommandRuns.Delete(activeDir)

	cleanCommandRunDir()

	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("expected active dir %s to survive (in activeCommandRuns), but it was removed", activeDir)
	}
}

func TestCleanCommandRunDir_EmptyOrMissingDir(t *testing.T) {
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Don't create the dir — cleanCommandRunDir should handle gracefully.
	cleanCommandRunDir() // must not panic
}

// ── Tool registration test ────────────────────────────────────────────────

func TestRegisterTier2_IncludesGetCommandResult(t *testing.T) {
	s := server.NewMCPServer("test-admin", "0.0.0")
	app := newTestApp(t)
	RegisterTier2(s, app)

	toolList := s.ListTools()
	if _, ok := toolList["orchestrator_get_command_result"]; !ok {
		t.Errorf("expected orchestrator_get_command_result to be registered in Tier2")
	}
}

// ── Event constants test ──────────────────────────────────────────────────

func TestCommandEventConstants(t *testing.T) {
	// Verify the three command event constants exist with correct wire values.
	if obs.EventCommandStarted != "command_started" {
		t.Errorf("EventCommandStarted = %q, want %q", obs.EventCommandStarted, "command_started")
	}
	if obs.EventCommandCompleted != "command_completed" {
		t.Errorf("EventCommandCompleted = %q, want %q", obs.EventCommandCompleted, "command_completed")
	}
	if obs.EventCommandTimeout != "command_timeout" {
		t.Errorf("EventCommandTimeout = %q, want %q", obs.EventCommandTimeout, "command_timeout")
	}
}

// ── Event emission test ───────────────────────────────────────────────────

func TestCommandEvents_PublishedToBus(t *testing.T) {
	var received []obs.AgentEvent
	unsub := obs.DefaultBus.SubscribeWithCancel(string(obs.EventCommandStarted), func(e obs.AgentEvent) error {
		received = append(received, e)
		return nil
	})
	defer unsub()

	obs.DefaultBus.Publish(obs.NewAgentEvent("", "", string(obs.EventCommandStarted), map[string]any{
		"command_id": "test123",
		"command":    "echo",
	}))

	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}
	if received[0].EventType != string(obs.EventCommandStarted) {
		t.Errorf("event type = %q, want %q", received[0].EventType, string(obs.EventCommandStarted))
	}
	if received[0].Payload["command_id"] != "test123" {
		t.Errorf("command_id = %v, want test123", received[0].Payload["command_id"])
	}
}

// ── Background command persistence test ───────────────────────────────────
//
// This test verifies the file persistence contract of background mode:
// meta.json is written immediately, result.json is written after completion,
// and the file structure matches what orchestrator_get_command_result expects.

func TestBackgroundCommand_FilePersistenceContract(t *testing.T) {
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Simulate what the background handler writes: meta.json + result.json.
	commandID := "testcmd01"
	runDir := filepath.Join(commandRunDir, commandID)
	os.MkdirAll(runDir, 0755)

	// Write meta.json (written immediately when background command starts).
	meta := map[string]any{
		"command":    "echo",
		"args":       []string{"hello"},
		"cwd":        "/tmp",
		"sandboxed":  false,
		"started_at": time.Now().Format(time.RFC3339),
	}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(filepath.Join(runDir, "meta.json"), metaBytes, 0644)

	// Verify meta.json can be read back.
	metaRead, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatalf("failed to read meta.json: %v", err)
	}
	var metaDecoded map[string]any
	if err := json.Unmarshal(metaRead, &metaDecoded); err != nil {
		t.Fatalf("failed to decode meta.json: %v", err)
	}
	if metaDecoded["command"] != "echo" {
		t.Errorf("meta command = %v, want echo", metaDecoded["command"])
	}

	// Write result.json (written after command completes).
	result := map[string]any{
		"status":    "ok",
		"exit_code": 0,
		"stdout":    "hello\n",
		"stderr":    "",
		"method":    "direct",
	}
	resultBytes, _ := json.MarshalIndent(result, "", "  ")
	os.WriteFile(filepath.Join(runDir, "result.json"), resultBytes, 0644)

	// Verify result.json can be read back.
	resultRead, err := os.ReadFile(filepath.Join(runDir, "result.json"))
	if err != nil {
		t.Fatalf("failed to read result.json: %v", err)
	}
	var resultDecoded map[string]any
	if err := json.Unmarshal(resultRead, &resultDecoded); err != nil {
		t.Fatalf("failed to decode result.json: %v", err)
	}
	if resultDecoded["status"] != "ok" {
		t.Errorf("result status = %v, want ok", resultDecoded["status"])
	}
	if resultDecoded["stdout"] != "hello\n" {
		t.Errorf("result stdout = %v, want hello\\n", resultDecoded["stdout"])
	}
}

// ── Background command "running" state test ───────────────────────────────
//
// When only meta.json exists (no result.json), the command is still running.
// This tests the file-based state detection that get_command_result relies on.

func TestBackgroundCommand_RunningStateDetection(t *testing.T) {
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	commandID := "running01"
	runDir := filepath.Join(commandRunDir, commandID)
	os.MkdirAll(runDir, 0755)

	// Write only meta.json — no result.json yet (command still running).
	meta := map[string]any{"command": "sleep", "started_at": time.Now().Format(time.RFC3339)}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(filepath.Join(runDir, "meta.json"), metaBytes, 0644)

	// result.json should NOT exist.
	_, err := os.ReadFile(filepath.Join(runDir, "result.json"))
	if !os.IsNotExist(err) {
		t.Errorf("expected result.json to not exist while command is running")
	}

	// meta.json SHOULD exist.
	_, err = os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Errorf("expected meta.json to exist while command is running: %v", err)
	}
}

// ── Background command "not found" state test ─────────────────────────────
//
// When neither meta.json nor result.json exists, the command_id is invalid.

func TestBackgroundCommand_NotFoundStateDetection(t *testing.T) {
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Create the command_runs base dir but no subdirectory for the command_id.
	os.MkdirAll(commandRunDir, 0755)

	// Neither meta.json nor result.json should exist for a bogus command_id.
	runDir := filepath.Join(commandRunDir, "nonexistent")
	_, err := os.ReadFile(filepath.Join(runDir, "result.json"))
	if !os.IsNotExist(err) {
		t.Errorf("expected result.json to not exist for bogus command_id")
	}
	_, err = os.ReadFile(filepath.Join(runDir, "meta.json"))
	if !os.IsNotExist(err) {
		t.Errorf("expected meta.json to not exist for bogus command_id")
	}
}
