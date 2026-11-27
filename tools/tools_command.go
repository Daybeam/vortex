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
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/core/skeleton"
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
	// Background command run output persistence (same TTL/cleaner pattern).
	commandRunDir = "outputs/command_runs"
)

var (
	sandboxRunCleanerOnce   sync.Once
	sandboxRunCleanerStop   = make(chan struct{})
	sandboxRunCleanerClosed sync.Once
)

// activeSandboxRuns tracks sandbox workspaces currently in use so the
// background cleaner doesn't RemoveAll a directory while a command is
// still running in it (audit LOGIC-7: TOCTOU with long-running processes).
var activeSandboxRuns sync.Map

// activeCommandRuns tracks background command output directories in use,
// preventing the cleaner from deleting results while a goroutine is still
// writing them. Same TOCTOU prevention pattern as activeSandboxRuns.
var activeCommandRuns sync.Map

// startSandboxRunCleaner starts a background goroutine (once per process)
// that periodically removes sandbox run workspaces older than sandboxRunTTL.
// The goroutine exits when stopSandboxRunCleaner is called or the process
// exits. (audit RL-1: goroutine leak — no stop channel, 2026-10-24)
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
			for {
				select {
			case <-ticker.C:
				cleanSandboxRunDir()
				cleanCommandRunDir()
				case <-sandboxRunCleanerStop:
					return
				}
			}
		}()
	})
}

// stopSandboxRunCleaner signals the background cleaner goroutine to exit.
// Safe to call multiple times or when the cleaner was never started (no-op).
func stopSandboxRunCleaner() {
	sandboxRunCleanerClosed.Do(func() {
		close(sandboxRunCleanerStop)
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

// cleanCommandRunDir removes expired background command output directories.
// Same TOCTOU-safe pattern as cleanSandboxRunDir: skip dirs in activeCommandRuns.
func cleanCommandRunDir() {
	entries, err := os.ReadDir(commandRunDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-sandboxRunTTL) // reuse same 24h TTL
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		fullPath := filepath.Join(commandRunDir, entry.Name())
		if _, active := activeCommandRuns.Load(fullPath); active {
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
		mcp.WithBoolean("background", mcp.Description("When true, returns immediately with a command_id while the command runs asynchronously. Results are persisted to outputs/command_runs/<id>/ and can be retrieved via orchestrator_get_command_result. Default false.")),
		mcp.WithInteger("idle_timeout_sec", mcp.Description("Idle timeout in seconds — process is killed after this many seconds of no output. Default 15. Increase for slow builds (npm install, pip install).")),
		mcp.WithInteger("total_timeout_sec", mcp.Description("Total timeout in seconds — hard kill after this many seconds regardless of output. Default 300. Increase for long builds (go test ./...).")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cmdName := strArg(req.Params.Arguments, "command")
		cwd := strArg(req.Params.Arguments, "cwd")
		encoding := strArg(req.Params.Arguments, "input_encoding")
		argMap, _ := req.Params.Arguments.(map[string]any)
		diagnostic := argMap["diagnostic_mode"] == true
		useScript := argMap["use_script_path"] == true
		sandboxed := argMap["sandboxed"] == true
		background := argMap["background"] == true

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
		// Per-call timeout overrides — default 15s/300s preserved when not specified.
		// Useful for long builds (go test ./...) or slow-output commands (npm install).
		if v := intArg(req.Params.Arguments, "idle_timeout_sec", 0); v > 0 {
			exec.IdleTimeout = time.Duration(v) * time.Second
		}
		if v := intArg(req.Params.Arguments, "total_timeout_sec", 0); v > 0 {
			exec.TotalTimeout = time.Duration(v) * time.Second
		}

		// Sandbox mode: isolated workspace + resource limits. Off by default
		// because shell operations depend on the local environment (PATH, tools,
		// cwd). Enable for temporary script writing — workspace is auto-cleaned
		// after 24h by a background goroutine.
		var sandboxDir string
		if sandboxed {
			startSandboxRunCleaner()
			sandboxDir = filepath.Join(sandboxRunDir, uuid.New().String()[:8])
			if err := os.MkdirAll(sandboxDir, 0755); err != nil {
				return errResult(fmt.Sprintf("sandbox setup failed: cannot create workspace %s: %v", sandboxDir, err))
			}
			activeSandboxRuns.Store(sandboxDir, true)
			if !background {
				defer activeSandboxRuns.Delete(sandboxDir)
			}
			cwd = sandboxDir
			exec.Sandboxed = true
		}

		// Background mode: return immediately with a command_id while the
		// command runs asynchronously. Results are persisted to
		// outputs/command_runs/<id>/ for later retrieval via
		// orchestrator_get_command_result. The goroutine uses a detached
		// context (context.Background) so cancellation of the request context
		// when the tool returns does not kill the command.
		if background {
			startSandboxRunCleaner() // ensures cleanCommandRunDir ticker is running

			commandID := uuid.New().String()[:8]
			runDir := filepath.Join(commandRunDir, commandID)
			if err := os.MkdirAll(runDir, 0755); err != nil {
				return errResult(fmt.Sprintf("background setup failed: %v", err))
			}
			activeCommandRuns.Store(runDir, true)

			// Write meta.json for identification while command is still running.
			meta := map[string]any{
				"command":    cmdName,
				"args":       cmdArgs,
				"cwd":        cwd,
				"sandboxed":  sandboxed,
				"started_at": time.Now().Format(time.RFC3339),
			}
			if metaBytes, err := json.MarshalIndent(meta, "", "  "); err == nil {
				os.WriteFile(filepath.Join(runDir, "meta.json"), metaBytes, 0644)
			}

			// Emit command_started event for webhook/chat-wake subscribers.
			obs.DefaultBus.Publish(obs.NewAgentEvent("", "", string(obs.EventCommandStarted), map[string]any{
				"command_id": commandID,
				"command":    cmdName,
			}))

			go func() {
				defer activeCommandRuns.Delete(runDir)
				if sandboxed {
					defer activeSandboxRuns.Delete(sandboxDir)
				}

			// Lifecycle-aware context: derive from app.LifecycleCtx so the
			// background command is cancelled on shutdown (audit RL-3).
			// Falls back to Background when LifecycleCtx is not wired.
			baseCtx := app.LifecycleCtx
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			bgCtx, cancel := context.WithTimeout(baseCtx, exec.TotalTimeout)
				defer cancel()

				var res *core.ExecutionResult
				var runErr error
				method := "direct"

				if useScript && !diagnostic {
					if shellType, body := core.IdentifyShellTask(cmdName, cmdArgs); shellType != "" {
						res, runErr = exec.RunViaTempScript(bgCtx, shellType, body, cwd)
						method = "script_file"
					}
				}
				if res == nil && runErr == nil {
					res, runErr = exec.Run(bgCtx, cmdName, cmdArgs, cwd, nil)
				}

				// Build result — redact+gate output before persisting to disk
				// (single cleaning point: disk holds the gated version only).
				result := map[string]any{"method": method}
				if runErr != nil {
					result["status"] = "error"
					result["error"] = runErr.Error()
				} else {
					gatedStdout, gatedStderr := redactAndGateOutput(res.Stdout, res.Stderr, cmdName)
					result["status"] = res.Status
					result["exit_code"] = res.ExitCode
					result["stdout"] = string(gatedStdout)
					result["stderr"] = string(gatedStderr)
				}
				if resultBytes, err := json.MarshalIndent(result, "", "  "); err == nil {
					os.WriteFile(filepath.Join(runDir, "result.json"), resultBytes, 0644)
				}

				// Emit terminal event — Completed for ok/error, Timeout for
				// timeout/idle_timeout statuses.
				eventType := string(obs.EventCommandCompleted)
				if s, _ := result["status"].(string); s == "timeout" || s == "idle_timeout" {
					eventType = string(obs.EventCommandTimeout)
				}
				obs.DefaultBus.Publish(obs.NewAgentEvent("", "", eventType, map[string]any{
					"command_id": commandID,
					"command":    cmdName,
					"status":     result["status"],
				}))
			}()

			return jsonOK(map[string]any{
				"command_id": commandID,
				"status":     "running",
			})
		}

		// Handle forced script path
		if useScript && !diagnostic {
			// If use_script_path is true, we try to extract the body and run via script
			if shellType, body := core.IdentifyShellTask(cmdName, cmdArgs); shellType != "" {
				res, err := exec.RunViaTempScript(ctx, shellType, body, cwd)
				if err != nil {
					return errResult(err.Error())
				}
				gatedStdout, gatedStderr := redactAndGateOutput(res.Stdout, res.Stderr, cmdName)
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

		// Apply redact+summarize to guard against oversized output hitting the
		// LLM context and to strip secrets before they enter that context.
		gatedStdout, gatedStderr := redactAndGateOutput(res.Stdout, res.Stderr, cmdName)

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

// registerGetCommandResult registers the orchestrator_get_command_result tool,
// which retrieves the persisted output of a background command started with
// orchestrator_run_command background=true. Reads result.json (completed) or
// meta.json (still running) from outputs/command_runs/<command_id>/.
func registerGetCommandResult(s *server.MCPServer, app *App) {
	app.register(s, mcp.NewTool("orchestrator_get_command_result",
		mcp.WithDescription("[Admin] Retrieve the result of a background command started with orchestrator_run_command background=true. Returns status, exit_code, stdout, stderr if completed; status='running' if still in progress; or not found if the command_id doesn't exist."),
		mcp.WithString("command_id", mcp.Required(), mcp.Description("The command_id returned by orchestrator_run_command with background=true")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		commandID := strArg(req.Params.Arguments, "command_id")
		if commandID == "" {
			return errResult("command_id is required")
		}

		runDir := filepath.Join(commandRunDir, commandID)

		// Try result.json first — command has completed.
		if resultBytes, err := os.ReadFile(filepath.Join(runDir, "result.json")); err == nil {
			var result map[string]any
			if json.Unmarshal(resultBytes, &result) == nil {
				return jsonOK(result)
			}
		}

		// No result.json — check meta.json to see if command is still running.
		if metaBytes, err := os.ReadFile(filepath.Join(runDir, "meta.json")); err == nil {
			var meta map[string]any
			if json.Unmarshal(metaBytes, &meta) == nil {
				return jsonOK(map[string]any{
					"command_id": commandID,
					"status":     "running",
					"meta":       meta,
				})
			}
		}

		return errResult(fmt.Sprintf("command_id %q not found", commandID))
	})
}

// redactAndGateOutput strips secrets from raw command stdout/stderr, then
// applies SummarizeOutput to bound size for the LLM context window.
//
// L2 fix (DELEGATION_HANDOVER_REDESIGN.md §12.6 L2): orchestrator_run_command
// output flows back into the host model's context. Without redaction, a
// command like `cat .env` returns plaintext API keys to the model vendor.
// RedactEgress runs BEFORE SummarizeOutput so a truncated tail can never
// retain a partial secret. Toggled by VORTEX_DISABLE_EGRESS_REDACTION.
func redactAndGateOutput(stdout, stderr []byte, cmdName string) ([]byte, []byte) {
	ro := core.RedactEgress(string(stdout))
	re := core.RedactEgress(string(stderr))
	gatedOut := []byte(skeleton.SummarizeOutput([]byte(ro), cmdName))
	gatedErr := []byte(skeleton.SummarizeOutput([]byte(re), cmdName))
	return gatedOut, gatedErr
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
