package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadCoverageVerifier checks that all required_files appear in the step's
// tool-call trace (i.e., the step actually read them via read_file or
// read_file_lines). Prevents the "overclaim" pattern where an agent reports
// reviewing files it never read (arXiv:2609.20812, OverclaimBench).
//
// Params:
//
//	required_files: []string — file paths that MUST appear in the trace
//	trace_file: string       — path to the step's StepResult JSON
//	                           (relative to workdir or absolute)
//
// Passes if every required_file appears in at least one read_file or
// read_file_lines tool call. Fails with the list of unread files otherwise.
type ReadCoverageVerifier struct{}

func (v *ReadCoverageVerifier) Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error) {
	requiredRaw, _ := criteria["required_files"].([]any)
	if len(requiredRaw) == 0 {
		return false, "missing 'required_files' parameter", nil
	}
	required := make([]string, 0, len(requiredRaw))
	for _, r := range requiredRaw {
		if s, ok := r.(string); ok && s != "" {
			required = append(required, s)
		}
	}
	if len(required) == 0 {
		return false, "'required_files' has no valid strings", nil
	}

	traceFile, _ := criteria["trace_file"].(string)
	if traceFile == "" {
		return false, "missing 'trace_file' parameter", nil
	}
	absPath := traceFile
	if !filepath.IsAbs(absPath) && workdir != "" {
		absPath = filepath.Join(workdir, traceFile)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return false, fmt.Sprintf("cannot read trace file: %s", err), nil
	}

	var stepResult struct {
		Trace []struct {
			ToolName  string         `json:"tool_name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(data, &stepResult); err != nil {
		return false, fmt.Sprintf("cannot parse StepResult JSON: %s", err), nil
	}

	readFiles := make(map[string]bool)
	for _, ti := range stepResult.Trace {
		switch ti.ToolName {
		case "read_file", "read_file_lines":
			if path, ok := ti.Arguments["path"].(string); ok && path != "" {
				readFiles[resolvePath(path, workdir)] = true
			}
		}
	}

	var missing []string
	for _, req := range required {
		if !readFiles[resolvePath(req, workdir)] {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("required files not read: %s", strings.Join(missing, ", ")), nil
	}
	return true, "", nil
}

func resolvePath(p, workdir string) string {
	if filepath.IsAbs(p) || workdir == "" {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(workdir, p))
}
