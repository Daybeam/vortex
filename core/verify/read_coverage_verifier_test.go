package verify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeStepResultJSON(t *testing.T, dir, filename string, trace []map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	data, err := json.MarshalIndent(map[string]any{"trace": trace}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadCoverageVerifier_AllFilesRead(t *testing.T) {
	dir := t.TempDir()
	trace := []map[string]any{
		{"tool_name": "read_file", "arguments": map[string]any{"path": filepath.Join(dir, "a.go")}},
		{"tool_name": "read_file", "arguments": map[string]any{"path": filepath.Join(dir, "b.go")}},
	}
	tracePath := writeStepResultJSON(t, dir, "step.json", trace)

	v := &ReadCoverageVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{
		"required_files": []any{filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")},
		"trace_file":     filepath.Base(tracePath),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected pass, got: %s", msg)
	}
}

func TestReadCoverageVerifier_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	trace := []map[string]any{
		{"tool_name": "read_file", "arguments": map[string]any{"path": filepath.Join(dir, "a.go")}},
	}
	tracePath := writeStepResultJSON(t, dir, "step.json", trace)

	v := &ReadCoverageVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{
		"required_files": []any{filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")},
		"trace_file":     filepath.Base(tracePath),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected failure when required file not read")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestReadCoverageVerifier_ReadFileLinesCounts(t *testing.T) {
	dir := t.TempDir()
	trace := []map[string]any{
		{"tool_name": "read_file_lines", "arguments": map[string]any{"path": filepath.Join(dir, "config.go")}},
	}
	tracePath := writeStepResultJSON(t, dir, "step.json", trace)

	v := &ReadCoverageVerifier{}
	passed, _, err := v.Verify(context.Background(), dir, map[string]any{
		"required_files": []any{filepath.Join(dir, "config.go")},
		"trace_file":     filepath.Base(tracePath),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("read_file_lines should count as reading the file")
	}
}

func TestReadCoverageVerifier_EmptyTrace(t *testing.T) {
	dir := t.TempDir()
	tracePath := writeStepResultJSON(t, dir, "step.json", []map[string]any{})

	v := &ReadCoverageVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{
		"required_files": []any{filepath.Join(dir, "a.go")},
		"trace_file":     filepath.Base(tracePath),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected failure with empty trace")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestReadCoverageVerifier_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "step.json")
	os.WriteFile(path, []byte("{not json"), 0644)

	v := &ReadCoverageVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{
		"required_files": []any{"a.go"},
		"trace_file":     "step.json",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected failure with malformed JSON")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestReadCoverageVerifier_MissingTraceFile(t *testing.T) {
	dir := t.TempDir()

	v := &ReadCoverageVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{
		"required_files": []any{"a.go"},
		"trace_file":     "nonexistent.json",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected failure with missing trace file")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestReadCoverageVerifier_MissingRequiredFilesParam(t *testing.T) {
	v := &ReadCoverageVerifier{}
	passed, msg, err := v.Verify(context.Background(), "", map[string]any{
		"trace_file": "step.json",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected failure with missing required_files")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestReadCoverageVerifier_MissingTraceFileParam(t *testing.T) {
	v := &ReadCoverageVerifier{}
	passed, msg, err := v.Verify(context.Background(), "", map[string]any{
		"required_files": []any{"a.go"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected failure with missing trace_file")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestReadCoverageVerifier_PathNormalization(t *testing.T) {
	dir := t.TempDir()
	absFile := filepath.Join(dir, "a.go")
	trace := []map[string]any{
		{"tool_name": "read_file", "arguments": map[string]any{"path": absFile}},
	}
	tracePath := writeStepResultJSON(t, dir, "step.json", trace)

	v := &ReadCoverageVerifier{}
	// Pass required_files as relative path, trace has absolute — should still match
	passed, _, err := v.Verify(context.Background(), dir, map[string]any{
		"required_files": []any{"a.go"},
		"trace_file":     filepath.Base(tracePath),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("path normalization should match relative required_files against absolute trace paths")
	}
}
