package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/schemas"
)

var CoreToolNames = map[string]bool{
	"write_file":   true,
	"read_file":    true,
	"execute_code": true,
}

func CoreToolDefinitions() []schemas.ToolDefinition {
	return []schemas.ToolDefinition{
		{
			Name:        "write_file",
			Description: "Write content to a file under the current task's output directory. Path is relative (e.g. 'report.json', 'sub/dir/output.txt'). Cannot write outside the task directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Relative path within the task output directory"},
					"content": map[string]any{"type": "string", "description": "File content (text or JSON string)"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "read_file",
			Description: "Read a file from the current task's output directory. Path is relative. Large files are auto-truncated with head+tail summary. Use mode='summary' for a structural skeleton with line numbers (JSON keys, HTML tags, code symbols).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Relative path within the task output directory"},
					"mode": map[string]any{"type": "string", "description": "Read mode: 'full' (default) or 'summary' (structural skeleton with line numbers)"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "execute_code",
			Description: "Execute code in a sandboxed environment. Supports: python, node, bun, lua. Returns stdout, stderr, and exit code.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code":     map[string]any{"type": "string", "description": "Code to execute"},
					"language": map[string]any{"type": "string", "description": "Language: python, node, bun, or lua", "default": "python"},
				},
				"required": []string{"code"},
			},
		},
	}
}

// HandleCoreTool executes a core tool by name. ctx is used for cancellation
// (e.g. execute_code subprocess timeout derives from ctx).
func HandleCoreTool(ctx context.Context, name string, args map[string]any, outputBase, taskID string, validators ...*SafePathValidator) (string, error) {
	taskDir := filepath.Join(outputBase, taskID)
	var validator *SafePathValidator
	if len(validators) > 0 {
		validator = validators[0]
	}

	resolvePath := func(rel string) (string, error) {
		if filepath.IsAbs(rel) {
			// Absolute path: check if within allowed roots.
			if validator != nil && validator.IsAllowed(rel) {
				return rel, nil
			}
			return "", fmt.Errorf("absolute path %q is outside allowed directories", rel)
		}
		if validator != nil && validator.SessionRoot != "" {
			// Prefer SessionRoot (harness-bench workspace) when available.
			sessionPath, err := validator.Resolve("session", rel)
			if err == nil {
				return sessionPath, nil
			}
		}
		// Fall back to taskDir (backward compatible).
		if validator != nil {
			return validator.Resolve("task", rel)
		}
		return safeJoin(taskDir, rel)
	}
	// resolveExistingPath finds an existing file, trying SessionRoot first
	// then taskDir. Used by read_file.
	resolveExistingPath := func(rel string) (string, error) {
		if filepath.IsAbs(rel) {
			// Absolute path: check if within allowed roots.
			if validator != nil && validator.IsAllowed(rel) {
				return rel, nil
			}
			return "", fmt.Errorf("absolute path %q is outside allowed directories", rel)
		}
		if validator != nil && validator.SessionRoot != "" {
			sessionPath, err := validator.Resolve("session", rel)
			if err == nil {
				if _, statErr := os.Stat(sessionPath); statErr == nil {
					return sessionPath, nil
				}
			}
		}
		// Try taskDir.
		var fallback string
		if validator != nil {
			fallback, _ = validator.Resolve("task", rel)
		} else {
			fallback, _ = safeJoin(taskDir, rel)
		}
		if fallback != "" {
			if _, statErr := os.Stat(fallback); statErr == nil {
				return fallback, nil
			}
		}
		// File not found anywhere — return session path for error message clarity.
		if validator != nil && validator.SessionRoot != "" {
			return "", fmt.Errorf("file not found: %s (searched session and task dirs)", rel)
		}
		return "", fmt.Errorf("file not found: %s", rel)
	}

	switch name {
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		if path == "" {
			return "", fmt.Errorf("path is required")
		}
		fullPath, err := resolvePath(path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return "", fmt.Errorf("failed to create directory: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("failed to write file: %v", err)
		}
		// Compute a friendly relative path for the response.
		var rel string
		if validator != nil && validator.SessionRoot != "" {
			rel, _ = filepath.Rel(validator.SessionRoot, fullPath)
		} else {
			rel, _ = filepath.Rel(taskDir, fullPath)
		}
		return fmt.Sprintf("Written %d bytes to %s", len(content), rel), nil

	case "read_file":
		path, _ := args["path"].(string)
		if path == "" {
			return "", fmt.Errorf("path is required")
		}
		fullPath, err := resolveExistingPath(path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("failed to read file: %v", err)
		}
		mode, _ := args["mode"].(string)
		if mode == "summary" {
			return FileSkeleton(data, filepath.Base(path)), nil
		}
		gated := AdaptiveOutputResult(data, path)
		return string(gated), nil

	case "execute_code":
		code, _ := args["code"].(string)
		lang, _ := args["language"].(string)
		if lang == "" {
			lang = "python"
		}
		// Use workspace (SessionRoot) as cwd so the model can use relative paths.
		execCwd := ""
		if validator != nil && validator.SessionRoot != "" {
			execCwd = validator.SessionRoot
		} else {
			execCwd = taskDir
		}
		result, err := executeCode(ctx, code, lang, execCwd)
		if err != nil {
			return "", fmt.Errorf("execution failed: %v", err)
		}
		out, _ := json.Marshal(result)
		return string(out), nil

	default:
		return "", fmt.Errorf("unknown core tool: %s", name)
	}
}

func safeJoin(base, rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, string(filepath.Separator)) || strings.Contains(rel, ":") {
		return "", fmt.Errorf("path traversal detected: %s", rel)
	}
	full := filepath.Join(base, cleaned)
	absBase, _ := filepath.Abs(base)
	absFull, _ := filepath.Abs(full)
	if !strings.HasPrefix(absFull, absBase) {
		return "", fmt.Errorf("path escapes task directory: %s", rel)
	}
	return full, nil
}

var langCommands = map[string][]string{
	"python": {"python3", "python"},
	"node":   {"node"},
	"bun":    {"bun"},
	"lua":    {"lua"},
}

func executeCode(ctx context.Context, code, lang, cwd string) (map[string]any, error) {
	candidates, ok := langCommands[lang]
	if !ok {
		return nil, fmt.Errorf("unsupported language: %s (supported: python, node, bun, lua)", lang)
	}

	var cmd *exec.Cmd
	// audit H11: derive from caller's ctx so code execution cancels on shutdown
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			cmd = exec.CommandContext(execCtx, path, "-c", code)
			break
		}
	}
	if cmd == nil {
		return nil, fmt.Errorf("%s runtime not found on PATH", lang)
	}

	// Set working directory so the model can use relative paths.
	if cwd != "" {
		cmd.Dir = cwd
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// audit C1: apply sandbox constraints (memory/CPU limits) to prevent
	// runaway code from consuming host resources. Uses the same Job Object
	// mechanism as JITSession and MCPConnectionManager.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start execution: %v", err)
	}

	var sandboxCleanup func()
	if cleanup, sbErr := client.ApplySandbox(cmd.Process.Pid, client.DefaultSandboxConstraints); sbErr != nil {
		fmt.Fprintf(os.Stderr, "[execute_code] WARNING: sandbox setup failed for pid=%d: %v\n", cmd.Process.Pid, sbErr)
	} else {
		sandboxCleanup = cleanup
	}

	err := cmd.Wait()
	if sandboxCleanup != nil {
		sandboxCleanup()
	}

	return map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": cmd.ProcessState.ExitCode(),
		"error":     errToString(err),
	}, nil
}

func errToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
