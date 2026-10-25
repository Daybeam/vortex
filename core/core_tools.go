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
	"github.com/daybeam/vortex/core/sandbox"
	"github.com/daybeam/vortex/core/skeleton"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/schemas"
)

// sandboxProvider (M3, 2026-10-25): when non-nil, execute_code routes through
// the SandboxProvider factory for OS-level isolation (bwrap/docker). Set at
// startup via SetSandboxProvider. Default = nil = raw exec + ApplySandbox.
var sandboxProvider sandbox.SandboxProvider

// SetSandboxProvider wires the SandboxProvider factory into execute_code.
// Call at startup: SetSandboxProvider(sandbox.SelectSandboxProvider(reg, outputBase)).
// nil or local provider = backward-compatible raw exec.
func SetSandboxProvider(p sandbox.SandboxProvider) { sandboxProvider = p }

var CoreToolNames = map[string]bool{
	"write_file":     true,
	"read_file":      true,
	"execute_code":   true,
	"decompress":     true,
	"search_archive": true,
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
			Description: "Read a file from the current task's output directory. Path is relative. Large files are auto-truncated with head+tail summary. Use mode='summary' for a structural skeleton with line numbers (JSON keys, HTML tags, code symbols). Use start_line/end_line to read a specific line range (1-indexed) — essential for batch-reading large files that exceed the model's context window.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "Relative path within the task output directory"},
					"mode":       map[string]any{"type": "string", "description": "Read mode: 'full' (default) or 'summary' (structural skeleton with line numbers)"},
					"start_line": map[string]any{"type": "integer", "description": "First line to read (1-indexed). If omitted, starts from line 1."},
					"end_line":   map[string]any{"type": "integer", "description": "Last line to read (1-indexed, inclusive). If omitted, reads to end."},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "execute_code",
			Description: "Execute code in a sandboxed environment. Supports: python, node, bun, lua. Returns stdout, stderr, and exit code.\n\nPREFER execute_code over multiple read_file calls when:\n- Counting functions/lines/patterns across many files\n- Extracting structured data from logs/JSON/CSV\n- Comparing or aggregating file contents\nOne script replaces N read_file calls and saves Nx context.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code":     map[string]any{"type": "string", "description": "Code to execute"},
					"language": map[string]any{"type": "string", "description": "Language: python, node, bun, or lua", "default": "python"},
				},
				"required": []string{"code"},
			},
		},
		{
			Name:        "decompress",
			Description: "Recover original content from a folded conversation segment. Use this when you need the full details of a past discussion that was compressed into a summary. Returns the original user messages and assistant responses for the specified segment.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"segment_id": map[string]any{"type": "string", "description": "The segment ID to decompress (from the Folded Conversation Segments section)"},
				},
				"required": []string{"segment_id"},
			},
		},
		{
			Name:        "search_archive",
			Description: "Search the context archive for relevant past conversation turns, step summaries, and task history. Use this when you need to recall what was discussed or done previously — especially after context compression. Returns matching summaries ranked by relevance.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Natural language query describing what you're looking for (e.g. 'output truncation implementation', 'spawner error handling')"},
					"limit": map[string]any{"type": "integer", "description": "Max results to return (default 5, max 10)", "default": 5},
				},
				"required": []string{"query"},
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
			return skeleton.FileSkeleton(data, filepath.Base(path)), nil
		}
		// Line-range reading: extract specific lines for batch-reading large files.
		startLine, _ := args["start_line"].(float64)
		endLine, _ := args["end_line"].(float64)
		if startLine > 0 || endLine > 0 {
			lines := strings.Split(string(data), "\n")
			s := int(startLine)
			if s < 1 {
				s = 1
			}
			e := int(endLine)
			if e <= 0 || e > len(lines) {
				e = len(lines)
			}
			if s > len(lines) {
				return fmt.Sprintf("[Line %d is beyond file end (total %d lines)]", s, len(lines)), nil
			}
			selected := strings.Join(lines[s-1:e], "\n")
			return fmt.Sprintf("[Lines %d-%d of %d]\n%s", s, e, len(lines), selected), nil
		}
		gated := skeleton.AdaptiveOutputResult(data, path, "orchestrator")
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

	case "decompress":
		// decompress is handled by the Spawner (which has the FoldEngine).
		// If we reach here, the fold engine is not available.
		return "", fmt.Errorf("decompress is not available in this context (no fold engine)")

	case "search_archive":
		// search_archive is handled by the Spawner or ChatHarness (which have
		// the ContextArchive). If we reach here, the archive is not available.
		return "", fmt.Errorf("search_archive is not available in this context (no archive)")

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

	// M3 (2026-10-25): if a SandboxProvider is configured (non-local mode),
	// route through it for OS-level isolation (bwrap/docker). Default = nil = raw
	// exec + ApplySandbox (backward-compatible). See TRIAL_SKILL_INSTANT_USE_DESIGN §4.
	if sandboxProvider != nil {
		result, err := sandboxProvider.Execute(ctx, sandbox.ExecRequest{
			Script:   code,
			Language: lang,
			Timeout:  30 * time.Second,
		})
		if err != nil {
			return nil, fmt.Errorf("sandbox execution failed: %v", err)
		}
		return map[string]any{
			"stdout":    result.Stdout,
			"stderr":    result.Stderr,
			"exit_code": result.ExitCode,
		}, nil
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

	// #17 (eval §8.35): On success (exit_code == 0), return only stdout/stderr
	// — omit exit_code/error so the model doesn't treat exit_code:0 as a domain
	// success signal. On failure, include the full wrapper for diagnosis.
	if cmd.ProcessState.ExitCode() == 0 {
		// #2 (eval §8.31): Observable classification for "false success" —
		// execute_code exits 0 but only produces stdout (print), with no
		// domain side-effect. Pure metric, zero behavior change. An arm can
		// compare `execute_code_print_only` / `execute_code_success` ratio
		// against reward to validate the "false success" hypothesis.
		observability.GetGlobalMetrics().Inc("orchestrator.calls.execute_code_success", 1)
		if stdout.Len() > 0 && stderr.Len() == 0 {
			observability.GetGlobalMetrics().Inc("orchestrator.calls.execute_code_print_only", 1)
		}
		// Output bounding: guard against oversized stdout/stderr hitting LLM context.
		// Mirrors SummarizeOutput in tools_command.go — AdaptiveOutputResult applies
		// Head+Tail truncation + Error Keyword Extraction + workspace dump for large outputs.
		gatedStdout := string(skeleton.AdaptiveOutputResult([]byte(stdout.String()), "execute_code", "orchestrator"))
		gatedStderr := string(skeleton.AdaptiveOutputResult([]byte(stderr.String()), "execute_code", "orchestrator"))
		return map[string]any{
			"stdout": gatedStdout,
			"stderr": gatedStderr,
		}, nil
	}
	return map[string]any{
		"stdout":    string(skeleton.AdaptiveOutputResult([]byte(stdout.String()), "execute_code", "orchestrator")),
		"stderr":    string(skeleton.AdaptiveOutputResult([]byte(stderr.String()), "execute_code", "orchestrator")),
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
