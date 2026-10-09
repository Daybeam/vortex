package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/daybeam/vortex/core"
)

// ── Sandbox run-command workspace cleanup ────────────────────────────────
//
// When orchestrator_run_command is called with sandboxed=true, the command
// runs in an isolated workspace under outputs/sandbox_run/<id>/. A background
// goroutine periodically removes workspaces older than the TTL so frequent
// sandboxed invocations don't accumulate unbounded disk usage.
const (
	sandboxRunDir           = "outputs/sandbox_run"
	sandboxRunTTL           = 24 * time.Hour
	sandboxRunCheckInterval = 1 * time.Hour
)

var sandboxRunCleanerOnce sync.Once

// activeSandboxRuns tracks sandbox workspaces currently in use so the
// background cleaner doesn't RemoveAll a directory while a command is
// still running in it (audit LOGIC-7: TOCTOU with long-running processes).
var activeSandboxRuns sync.Map

// startSandboxRunCleaner starts a background goroutine (once per process)
// that periodically removes sandbox run workspaces older than sandboxRunTTL.
func startSandboxRunCleaner() {
	sandboxRunCleanerOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[sandbox-cleaner] goroutine panic: %v\n%s", r, debug.Stack())
				}
			}()
			ticker := time.NewTicker(sandboxRunCheckInterval)
			defer ticker.Stop()
			for range ticker.C {
				cleanSandboxRunDir()
			}
		}()
	})
}

func cleanSandboxRunDir() {
	entries, err := os.ReadDir(sandboxRunDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-sandboxRunTTL)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		fullPath := filepath.Join(sandboxRunDir, entry.Name())
		// audit LOGIC-7: skip directories still in use by active sandbox runs
		if _, active := activeSandboxRuns.Load(fullPath); active {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.RemoveAll(fullPath)
		}
	}
}

func registerCommandList(s *server.MCPServer, app *App) {
	app.register(s, mcp.NewTool("orchestrator_run_command",
		mcp.WithDescription("[Admin] Execute a single, direct shell command with timeout & tracking. Use this for atomic operations (e.g., checking a file version, running a build script). For multi-step work involving judgment or coordination, use orchestrator_submit_task instead."),
		mcp.WithString("command", mcp.Required(), mcp.Description("The executable to run (e.g. 'git', 'go', 'powershell')")),
		mcp.WithAny("args", mcp.Description("Arguments for the command. JSON array preferred: [\"arg1\", \"arg2\"]. Example for powershell: [\"-NoProfile\", \"-Command\", \"Get-Date\"]")),
		mcp.WithString("_reason", mcp.Required(), mcp.Description("Audit reason for this direct execution")),
		mcp.WithString("cwd", mcp.Description("Working directory for the command")),
		mcp.WithString("input_encoding", mcp.Description("Output encoding: 'utf8' (default) or 'gbk' (Windows Chinese output)")),
		mcp.WithBoolean("diagnostic_mode", mcp.Description("Enable dual-path verification (direct vs script)")),
		mcp.WithBoolean("use_script_path", mcp.Description("Force execution via temporary script file (safer for complex quoting)")),
		mcp.WithBoolean("sandboxed", mcp.Description("When true, runs in an isolated workspace (outputs/sandbox_run/<id>/) with memory/CPU limits (256MB/30s). Default false — shell operations need local environment access. Enable for temporary script writing; workspace is auto-cleaned after 24h.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cmdName := strArg(req.Params.Arguments, "command")
		cwd := strArg(req.Params.Arguments, "cwd")
		encoding := strArg(req.Params.Arguments, "input_encoding")
		argMap, _ := req.Params.Arguments.(map[string]any)
		diagnostic := argMap["diagnostic_mode"] == true
		useScript := argMap["use_script_path"] == true
		sandboxed := argMap["sandboxed"] == true

		// Parse args: accept JSON array or CSV string
		cmdArgs := parseRunCommandArgs(req.Params.Arguments)

		// PowerShell special-case: auto-inject execution flags so expressions actually evaluate
		lowerCmd := strings.ToLower(strings.TrimSuffix(strings.ToLower(cmdName), ".exe"))
		isPowerShell := lowerCmd == "powershell" || lowerCmd == "pwsh"
		if isPowerShell {
			// Only inject if user hasn't already passed -Command or -EncodedCommand
			alreadyHasCmd := len(cmdArgs) > 0 &&
				(strings.EqualFold(cmdArgs[0], "-command") || strings.EqualFold(cmdArgs[0], "-encodedcommand") || strings.EqualFold(cmdArgs[0], "-file"))
			if !alreadyHasCmd {
				cmdArgs = append([]string{"-NoProfile", "-NonInteractive", "-Command"}, cmdArgs...)
			}
		}

		exec := core.NewControlledExecutor()
		exec.DiagnosticMode = diagnostic
		if encoding != "" {
			exec.OutputEncoding = encoding
		}
		if strArg(req.Params.Arguments, "disable_stdin_monitoring") == "true" {
			exec.DisableStdinMonitoring = true
		}

		// Sandbox mode: isolated workspace + resource limits. Off by default
		// because shell operations depend on the local environment (PATH, tools,
		// cwd). Enable for temporary script writing — workspace is auto-cleaned
		// after 24h by a background goroutine.
		if sandboxed {
			startSandboxRunCleaner()
			wsDir := filepath.Join(sandboxRunDir, uuid.New().String()[:8])
			if err := os.MkdirAll(wsDir, 0755); err != nil { // regression for audit LOGIC-3: was ignoring error
				return errResult(fmt.Sprintf("sandbox setup failed: cannot create workspace %s: %v", wsDir, err))
			}
			activeSandboxRuns.Store(wsDir, true)      // audit LOGIC-7: prevent cleaner from deleting while in use
			defer activeSandboxRuns.Delete(wsDir)     // audit LOGIC-7: release when command finishes
			cwd = wsDir
			exec.Sandboxed = true
		}

		// Handle forced script path
		if useScript && !diagnostic {
			// If use_script_path is true, we try to extract the body and run via script
			if shellType, body := core.IdentifyShellTask(cmdName, cmdArgs); shellType != "" {
				res, err := exec.RunViaTempScript(ctx, shellType, body, cwd)
				if err != nil {
					return errResult(err.Error())
				}
				gatedStdout := []byte(core.SummarizeOutput(res.Stdout, cmdName))
				gatedStderr := []byte(core.SummarizeOutput(res.Stderr, cmdName))
				return jsonOK(map[string]any{
					"status":    res.Status,
					"stdout":    string(gatedStdout),
					"stderr":    string(gatedStderr),
					"exit_code": res.ExitCode,
					"method":    "script_file",
				})
			}
		}

		res, err := exec.Run(ctx, cmdName, cmdArgs, cwd, nil)
		if err != nil {
			return errResult(err.Error())
		}

		// Apply SummarizeOutput to guard against oversized output hitting the LLM context
		gatedStdout := []byte(core.SummarizeOutput(res.Stdout, cmdName))
		gatedStderr := []byte(core.SummarizeOutput(res.Stderr, cmdName))

		resp := map[string]any{
			"status":    res.Status,
			"stdout":    string(gatedStdout),
			"stderr":    string(gatedStderr),
			"exit_code": res.ExitCode,
		}
		if res.Diagnostic != nil {
			resp["diagnostic"] = res.Diagnostic
		}
		return jsonOK(resp)
	})
}

// parseRunCommandArgs parses the "args" parameter for orchestrator_run_command.
// It accepts either a JSON array ["arg1", "arg2"] or a legacy CSV string "arg1,arg2".
func parseRunCommandArgs(rawArgs any) []string {
	args, _ := rawArgs.(map[string]any)
	if args == nil {
		return nil
	}
	raw, ok := args["args"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if v == "" {
			return nil
		}
		trimmed := strings.TrimSpace(v)
		if strings.HasPrefix(trimmed, "[") {
			var decoded []string
			if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
				return decoded
			}
			var decodedAny []any
			if err := json.Unmarshal([]byte(trimmed), &decodedAny); err == nil {
				out := make([]string, 0, len(decodedAny))
				for _, item := range decodedAny {
					if s, ok := item.(string); ok {
						out = append(out, s)
					}
				}
				return out
			}
			return []string{v}
		}
		return splitQuotedCSV(v)
	}
	return nil
}
