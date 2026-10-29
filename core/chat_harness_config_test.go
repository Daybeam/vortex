package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// maxTokensRecorderProvider records the MaxTokens value from the last
// CompleteRequest, to verify that ChatHarness.MaxOutputTokens is wired
// correctly (and not hardcoded to 2048).
type maxTokensRecorderProvider struct {
	recordedMaxTokens int
	calls             int
}

func (m *maxTokensRecorderProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *maxTokensRecorderProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	m.recordedMaxTokens = req.MaxTokens
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (m *maxTokensRecorderProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func (m *maxTokensRecorderProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}

func (m *maxTokensRecorderProvider) Name() string { return "max-tokens-recorder" }

// ─── Finding 1: MaxOutputTokens config wiring (harness-bench report §4.1) ───

// TestChatHarness_MaxOutputTokens_FromConfig verifies that when
// MaxOutputTokens is set on ChatHarness, the value is passed to the
// provider's CompleteRequest.MaxTokens — not hardcoded to 2048.
//
// Regression: core/chat_harness.go:443 had `MaxTokens: 2048` hardcoded
// with no config wiring. (harness-bench report §4.1, 2026-10-28)
func TestChatHarness_MaxOutputTokens_FromConfig(t *testing.T) {
	p := &maxTokensRecorderProvider{}
	h := &ChatHarness{
		Provider:        p,
		Model:           "test-model",
		OutputBase:      t.TempDir(),
		MaxTurns:        1,
		MaxOutputTokens: 8192, // custom value from config
	}
	_, err := h.Run(context.Background(), "test-task",
		[]ChatMessage{{Role: "user", Content: "hi"}},
		func(ChatEvent) error { return nil })
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if p.recordedMaxTokens != 8192 {
		t.Errorf("expected MaxTokens=8192 from config, got %d (hardcoded 2048 bug?)", p.recordedMaxTokens)
	}
}

// TestChatHarness_MaxOutputTokens_Default verifies that when MaxOutputTokens
// is 0 (unset), the default of 2048 is used — backward compatible.
func TestChatHarness_MaxOutputTokens_Default(t *testing.T) {
	p := &maxTokensRecorderProvider{}
	h := &ChatHarness{
		Provider:   p,
		Model:      "test-model",
		OutputBase: t.TempDir(),
		MaxTurns:   1,
		// MaxOutputTokens = 0 → should default to 2048
	}
	_, err := h.Run(context.Background(), "test-task",
		[]ChatMessage{{Role: "user", Content: "hi"}},
		func(ChatEvent) error { return nil })
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if p.recordedMaxTokens != 2048 {
		t.Errorf("expected default MaxTokens=2048, got %d", p.recordedMaxTokens)
	}
}

// ─── Finding 2: SessionRoot workspace path resolution (harness-bench report §4.2) ───

// TestChatHarness_SessionRoot_ResolvesRelativePath verifies that when
// SessionRoot is set, execTool passes a SafePathValidator to HandleCoreTool
// so relative paths resolve against the workspace root.
//
// Regression: chat_harness.go execTool called HandleCoreTool without a
// validator, so relative paths fell back to outputs/<taskID> instead of
// the workspace. (harness-bench report §4.2, 2026-10-28)
func TestChatHarness_SessionRoot_ResolvesRelativePath(t *testing.T) {
	wsRoot := t.TempDir()
	testContent := "workspace file content"
	if err := os.WriteFile(filepath.Join(wsRoot, "input.txt"), []byte(testContent), 0644); err != nil {
		t.Fatal(err)
	}

	h := &ChatHarness{
		OutputBase:  t.TempDir(),
		SessionRoot: wsRoot,
	}
	result, err := h.execTool(context.Background(), "read_file",
		map[string]any{"path": "input.txt"}, "test-task")
	if err != nil {
		t.Fatalf("execTool read_file failed: %v", err)
	}
	if !strings.Contains(result, testContent) {
		t.Errorf("expected workspace file content %q, got: %s", testContent, result)
	}
}

// TestChatHarness_SessionRoot_Empty_BackwardCompat verifies that when
// SessionRoot is empty, execTool behavior is unchanged — relative paths
// resolve against outputs/<taskID> (legacy safeJoin behavior).
func TestChatHarness_SessionRoot_Empty_BackwardCompat(t *testing.T) {
	outputBase := t.TempDir()
	taskID := "test-task"
	taskDir := filepath.Join(outputBase, taskID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	testContent := "task file content"
	if err := os.WriteFile(filepath.Join(taskDir, "input.txt"), []byte(testContent), 0644); err != nil {
		t.Fatal(err)
	}

	h := &ChatHarness{
		OutputBase:  outputBase,
		SessionRoot: "", // empty = backward compatible
	}
	result, err := h.execTool(context.Background(), "read_file",
		map[string]any{"path": "input.txt"}, taskID)
	if err != nil {
		t.Fatalf("execTool read_file failed: %v", err)
	}
	if !strings.Contains(result, testContent) {
		t.Errorf("expected task file content %q, got: %s", testContent, result)
	}
}

// TestChatHarness_SessionRoot_WriteFile verifies that write_file with
// SessionRoot writes to the workspace, not outputs/<taskID>.
func TestChatHarness_SessionRoot_WriteFile(t *testing.T) {
	wsRoot := t.TempDir()
	h := &ChatHarness{
		OutputBase:  t.TempDir(),
		SessionRoot: wsRoot,
	}
	_, err := h.execTool(context.Background(), "write_file",
		map[string]any{"path": "output.txt", "content": "written to workspace"}, "test-task")
	if err != nil {
		t.Fatalf("execTool write_file failed: %v", err)
	}
	// Verify the file was written to the workspace root, not outputs/<taskID>
	data, err := os.ReadFile(filepath.Join(wsRoot, "output.txt"))
	if err != nil {
		t.Fatalf("file not found in workspace root: %v", err)
	}
	if string(data) != "written to workspace" {
		t.Errorf("expected 'written to workspace', got %q", string(data))
	}
}

// ─── Defect 3: Tool descriptions adapt when SessionRoot is set (report §3) ───

// TestChatHarness_ToolDescriptions_SessionRoot_Adapted verifies that when
// SessionRoot is set, tool descriptions reflect the real workspace root
// and clarify execute_code is not a shell.
//
// Regression: write_file said "Cannot write outside the task directory"
// even when SessionRoot was set, and execute_code didn't clarify it's
// not a shell — model sent git/ls as Python code. (harness-bench report
// §3 defect 3, 2026-10-28)
func TestChatHarness_ToolDescriptions_SessionRoot_Adapted(t *testing.T) {
	wsRoot := "/test/workspace"
	h := &ChatHarness{
		SessionRoot: wsRoot,
	}
	tools := h.ToolDefinitions()
	for _, tool := range tools {
		switch tool.Name {
		case "write_file":
			if strings.Contains(tool.Description, "Cannot write outside") {
				t.Error("write_file description should not say 'Cannot write outside' when SessionRoot is set")
			}
			if !strings.Contains(tool.Description, wsRoot) {
				t.Errorf("write_file description should mention workspace root %q, got: %s", wsRoot, tool.Description)
			}
		case "execute_code":
			if !strings.Contains(tool.Description, "SOURCE CODE") {
				t.Error("execute_code description should clarify it's source code, not shell")
			}
			if !strings.Contains(tool.Description, "SyntaxError") {
				t.Error("execute_code description should warn about SyntaxError for shell commands")
			}
		}
	}
}

// TestChatHarness_ToolDescriptions_NoSessionRoot_Original verifies that
// when SessionRoot is empty, tool descriptions are unchanged (backward compat).
func TestChatHarness_ToolDescriptions_NoSessionRoot_Original(t *testing.T) {
	h := &ChatHarness{
		SessionRoot: "",
	}
	tools := h.ToolDefinitions()
	for _, tool := range tools {
		if tool.Name == "write_file" {
			if !strings.Contains(tool.Description, "Cannot write outside") {
				t.Error("write_file description should retain original text when SessionRoot is empty")
			}
		}
	}
}
