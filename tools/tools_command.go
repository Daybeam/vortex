package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/daybeam/vortex/core"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

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
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cmdName := strArg(req.Params.Arguments, "command")
		cwd := strArg(req.Params.Arguments, "cwd")
		encoding := strArg(req.Params.Arguments, "input_encoding")
		argMap, _ := req.Params.Arguments.(map[string]any)
		diagnostic := argMap["diagnostic_mode"] == true
		useScript := argMap["use_script_path"] == true

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
