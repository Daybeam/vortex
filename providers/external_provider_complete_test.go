package providers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// skipIfNoPython skips the test if neither python nor python3 is in PATH.
// Audit FIX 2026-11-22: replaced runtime.GOOS=="windows" skips with Python
// availability check so tests run on all platforms where Python is installed.
func skipIfNoPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python"); err == nil {
		return
	}
	if _, err := exec.LookPath("python3"); err == nil {
		return
	}
	t.Skip("python not found in PATH")
}

func TestExternalScriptProvider_Embed_NotSupported(t *testing.T) {
	p := &ExternalScriptProvider{
		cfg:        &config.ProviderConfig{},
		scriptPath: "script.py",
		runtime:    "python",
		exePath:    "python",
	}
	_, err := p.Embed(context.Background(), "test text")
	if err == nil {
		t.Fatal("expected error for ExternalScriptProvider Embed, got nil")
	}
}

func TestExternalScriptProvider_Complete_ExecutesScript(t *testing.T) {
	skipIfNoPython(t)

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "echo.py")
	script := `import sys
sys.stdin.read()
print('{"text":"hello from script","stop_reason":"end_turn","prompt_tokens":5,"completion_tokens":10}')
`
	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	p, err := NewExternalScriptProvider(&config.ProviderConfig{}, scriptPath, config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("NewExternalScriptProvider failed: %v", err)
	}

	resp, err := p.Complete(context.Background(), CompleteRequest{
		Model: "test", System: "s", User: "u", MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.Text != "hello from script" {
		t.Errorf("Text = %q, want %q", resp.Text, "hello from script")
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "end_turn")
	}
	if resp.PromptTokens != 5 {
		t.Errorf("PromptTokens = %d, want 5", resp.PromptTokens)
	}
	if resp.CompletionTokens != 10 {
		t.Errorf("CompletionTokens = %d, want 10", resp.CompletionTokens)
	}
}

func TestExternalScriptProvider_StreamComplete_DelegatesToComplete(t *testing.T) {
	skipIfNoPython(t)

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "echo.py")
	script := `import sys
sys.stdin.read()
print('{"text":"streamed response","stop_reason":"end_turn","prompt_tokens":3,"completion_tokens":7}')
`
	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	p, err := NewExternalScriptProvider(&config.ProviderConfig{}, scriptPath, config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("NewExternalScriptProvider failed: %v", err)
	}

	var chunks []string
	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "test", System: "s", User: "u", MaxTokens: 100,
	}, func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.Text != "streamed response" {
		t.Errorf("Text = %q, want %q", resp.Text, "streamed response")
	}
	if len(chunks) != 1 || chunks[0] != "streamed response" {
		t.Errorf("chunks = %v, want [streamed response]", chunks)
	}
}

func TestExternalScriptProvider_Complete_ScriptError(t *testing.T) {
	skipIfNoPython(t)

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fail.py")
	script := `import sys
sys.stdin.read()
sys.stderr.write('not valid json\n')
sys.exit(1)
`
	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	p, err := NewExternalScriptProvider(&config.ProviderConfig{}, scriptPath, config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("NewExternalScriptProvider failed: %v", err)
	}

	_, err = p.Complete(context.Background(), CompleteRequest{
		Model: "test", System: "s", User: "u", MaxTokens: 100,
	})
	if err == nil {
		t.Fatal("expected error for failing script, got nil")
	}
}

func TestExternalScriptProvider_Complete_InvalidJSON(t *testing.T) {
	skipIfNoPython(t)

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "badjson.py")
	script := `import sys
sys.stdin.read()
print('this is not json')
`
	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	p, err := NewExternalScriptProvider(&config.ProviderConfig{}, scriptPath, config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("NewExternalScriptProvider failed: %v", err)
	}

	_, err = p.Complete(context.Background(), CompleteRequest{
		Model: "test", System: "s", User: "u", MaxTokens: 100,
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON output, got nil")
	}
}

func TestExternalScriptProvider_Name(t *testing.T) {
	p := &ExternalScriptProvider{
		cfg:        &config.ProviderConfig{},
		scriptPath: "/path/to/myscript.py",
		runtime:    "python",
		exePath:    "python",
	}
	name := p.Name()
	expected := "external:python:myscript.py"
	if name != expected {
		t.Errorf("Name() = %q, want %q", name, expected)
	}
}

func TestExternalScriptProvider_Reload_NoOp(t *testing.T) {
	p := &ExternalScriptProvider{
		cfg:        &config.ProviderConfig{},
		scriptPath: "script.py",
		runtime:    "python",
		exePath:    "python",
	}
	if err := p.Reload(); err != nil {
		t.Errorf("Reload() returned error: %v", err)
	}
}

func TestExternalScriptProvider_BridgeInputFormat(t *testing.T) {
	skipIfNoPython(t)

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "verify_input.py")
	outputFile := filepath.Join(dir, "bridge_input_test.json")
	// Embed the output file path directly in the script for cross-platform compatibility.
	script := `import sys
input = sys.stdin.read()
with open(r'` + outputFile + `', 'w') as f:
    f.write(input)
print('{"text":"ok","stop_reason":"end_turn","prompt_tokens":1,"completion_tokens":1}')
`
	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	p, err := NewExternalScriptProvider(&config.ProviderConfig{
		BaseURL: "http://example.com",
		Model:   "test-model",
		Extra:   map[string]any{"key": "value"},
	}, scriptPath, config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("NewExternalScriptProvider failed: %v", err)
	}

	_, err = p.Complete(context.Background(), CompleteRequest{
		Model: "test-model", System: "s", User: "u", MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	savedInput, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("failed to read saved input: %v", err)
	}

	var bridgeInput map[string]any
	if err := json.Unmarshal(savedInput, &bridgeInput); err != nil {
		t.Fatalf("failed to parse bridge input: %v", err)
	}

	if _, ok := bridgeInput["request"]; !ok {
		t.Error("bridge input missing 'request' key")
	}
	cfg, ok := bridgeInput["config"].(map[string]any)
	if !ok {
		t.Fatal("bridge input missing 'config' key or wrong type")
	}
	if cfg["base_url"] != "http://example.com" {
		t.Errorf("config.base_url = %v, want http://example.com", cfg["base_url"])
	}
	if cfg["model"] != "test-model" {
		t.Errorf("config.model = %v, want test-model", cfg["model"])
	}
}
