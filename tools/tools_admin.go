package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type deploySession struct {
	ID           string
	Path         string
	TmpPath      string
	TotalSize    int64
	ExpectedHash string
	Reason       string
	CreatedAt    time.Time
	mu           sync.Mutex
}

var (
	deploySessions sync.Map // map[string]*deploySession
)

func registerAdminTools(s *server.MCPServer, app *App) {
	// REMOVED (2026-09-20): orchestrator_reload — redundant with
	// config.reload subsystem action. Use:
	//   orchestrator_invoke(subsystem="config", action="reload")

	// MOVED (2026-09-20): orchestrator_debug_dump → admin.debug_dump
	// subsystem action. Use:
	//   orchestrator_invoke(subsystem="admin", action="debug_dump")
	// The Promoter may auto-elevate it to a top-level tool if usage crosses threshold.

	// MOVED (2026-09-20): orchestrator_get_code_details → intel.get_code_details
	// subsystem action. Use:
	//   orchestrator_invoke(subsystem="intel", action="get_code_details", args={path:..., symbol:...})
	// The Promoter may auto-elevate it to a top-level tool if usage crosses threshold.

	// NOTE: orchestrator_reindex_memory was moved to subsystem "admin.reindex_memory"
	// (2026-09-04) because it is a low-frequency maintenance op. Use:
	//   orchestrator_invoke(subsystem="admin", action="reindex_memory")
	// The Promoter may auto-elevate it to a top-level tool if usage crosses threshold.

	// Subsystem Invocation
	// RESTORED (2026-07-26): orchestrator_invoke was silently dropped during an
	// upstream merge. This tool is critical for direct subsystem interaction.
	app.register(s, mcp.NewTool("orchestrator_invoke",
		mcp.WithDescription("[Admin] Directly invoke an atomic action in a specific subsystem. Use this for single-step administrative tasks. For complex workflows, use orchestrator_submit_task."),
		mcp.WithString("subsystem", mcp.Required(), mcp.Description("Target subsystem: config|jit|admin|schedule|exp|intel|proxy")),
		mcp.WithString("action", mcp.Required(), mcp.Description("Specific action within the subsystem")),
		mcp.WithAny("args", mcp.Description(`Map of arguments for the action as a JSON object: {"param":"value"}`)),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sub := strArg(req.Params.Arguments, "subsystem")
		act := strArg(req.Params.Arguments, "action")

		rawArgs, _ := req.Params.Arguments.(map[string]any)
		var args map[string]any
		if rawArgs != nil {
			switch v := rawArgs["args"].(type) {
			case map[string]any:
				args = v
			case string:
				if v != "" {
					trimmed := strings.TrimSpace(v)
					if strings.HasPrefix(trimmed, "{") {
						_ = json.Unmarshal([]byte(trimmed), &args)
					}
				}
			}
		}
		if args == nil {
			args = make(map[string]any)
		}

		res, err := registry.Invoke(ctx, app, sub, act, args)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonOK(res)
	})

	// Deployment Pipeline
	app.register(s, mcp.NewTool("orchestrator_admin_deploy",
		mcp.WithDescription("[Admin] Robustly deploy/write a file to disk. Supports direct write or chunked upload."),
		mcp.WithString("action", mcp.Required(), mcp.Description("Action: 'write', 'init', 'push', 'commit'")),
		mcp.WithString("path", mcp.Description("Target path (required for write/init)")),
		mcp.WithString("content", mcp.Description("Content (for write/push)")),
		mcp.WithString("encoding", mcp.Description("Encoding: 'utf8' (default) or 'base64'")),
		mcp.WithString("_reason", mcp.Description("Reason (required for write/init)")),
		mcp.WithString("session_id", mcp.Description("Session ID (required for push/commit)")),
		mcp.WithNumber("total_size", mcp.Description("Total size (required for init)")),
		mcp.WithString("expected_sha256", mcp.Description("Optional checksum (init)")),
		mcp.WithNumber("offset", mcp.Description("Offset (push)")),
		mcp.WithBoolean("create_dirs", mcp.Description("Auto-create parent directories (default: true)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		action := strArg(req.Params.Arguments, "action")
		switch action {
		case "write":
			return HandleAdminDeployFile(ctx, app, req)
		case "init":
			return HandleAdminDeployInit(ctx, app, req)
		case "push":
			return HandleAdminDeployPush(ctx, app, req)
		case "commit":
			return HandleAdminDeployCommit(ctx, app, req)
		default:
			return errResult("invalid action: " + action)
		}
	})

	// Task Management
	app.register(s, mcp.NewTool("orchestrator_cancel_task",
		mcp.WithDescription("[Admin] Cancel a running task."),
		mcp.WithString("task_id", mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := strArg(req.Params.Arguments, "task_id")
		err := app.Scheduler.CancelTask(id)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonOK(map[string]string{"task_id": id, "status": "cancelled"})
	})

	// Time Travel & Fork (ADDED 2026-09-14): fork a task from a historical step.
	// See docs/architecture/TIME_TRAVEL_AND_OBSERVABILITY_DESIGN.md
	app.register(s, mcp.NewTool("orchestrator_fork_task",
		mcp.WithDescription("[Admin] Fork a new task from a historical step checkpoint. The new task inherits completed steps up to the target step and resets all downstream steps to pending."),
		mcp.WithString("source_task_id", mcp.Required()),
		mcp.WithString("target_step_id", mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sourceTaskID := strArg(req.Params.Arguments, "source_task_id")
		targetStepID := strArg(req.Params.Arguments, "target_step_id")
		newTaskID, err := app.Scheduler.ForkTask(sourceTaskID, targetStepID)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonOK(map[string]string{
			"new_task_id":    newTaskID,
			"parent_task_id": sourceTaskID,
			"forked_at_step": targetStepID,
		})
	})

	// Removed (2026-09-04): UI suspend/resume is a frontend/lifecycle concern,
	// not an Agent action. Exposing it as an MCP tool adds token overhead and
	// invites hallucinated misuse. Frontend/OOM handlers should drive this
	// via local IPC or health hooks instead.

	// NOTE: orchestrator_import_skill was moved to subsystem "admin.import_skill"
	// (2026-09-04) because it is a low-frequency ops tool. Use:
	//   orchestrator_invoke(subsystem="admin", action="import_skill", path="...")
	// The Promoter may auto-elevate it to a top-level tool if usage crosses threshold.
}

// Handlers

func HandleAdminDeployFile(ctx context.Context, app *App, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Security gate (ADDED 2026-09-14): arbitrary file write is a known Open Core
	// risk (see docs/architecture/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §1.4 问题B).
	// Disabled by default; must explicitly opt in via env var.
	if os.Getenv("VORTEX_ALLOW_DEPLOY_WRITE") != "1" {
		return errResult("admin deploy write is disabled by default for security. Set VORTEX_ALLOW_DEPLOY_WRITE=1 to enable.")
	}
	path := strArg(req.Params.Arguments, "path")
	content := strArg(req.Params.Arguments, "content")
	encoding := strArg(req.Params.Arguments, "encoding")
	reason := strArg(req.Params.Arguments, "_reason")
	createDirs := true
	if val, ok := req.Params.Arguments.(map[string]any)["create_dirs"].(bool); ok {
		createDirs = val
	}

	if path == "" || reason == "" {
		return errResult("path and _reason are required")
	}

	var data []byte
	var err error

	if encoding == "base64" {
		data, err = base64.StdEncoding.DecodeString(content)
		if err != nil {
			return errResult(fmt.Sprintf("base64 decode failed: %v", err))
		}
	} else {
		data = []byte(content)
	}

	absPath, _ := filepath.Abs(path)
	if createDirs {
		_ = os.MkdirAll(filepath.Dir(absPath), 0755)
	}

	if err := os.WriteFile(absPath, data, 0644); err != nil {
		return errResult(fmt.Sprintf("failed to write file: %v", err))
	}

	checksum := fmt.Sprintf("%x", sha256.Sum256(data))
	app.Logger.Log(core.EventAdminFileDeployed, "system", "", map[string]any{
		"path": absPath, "size": len(data), "checksum": checksum, "reason": reason,
	})

	return jsonOK(map[string]any{
		"status": "success", "path": absPath, "sha256": checksum, "_reason": reason,
	})
}

func HandleAdminDeployInit(ctx context.Context, app *App, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := strArg(req.Params.Arguments, "path")
	totalSize := int64(intArg(req.Params.Arguments, "total_size", 0))
	expectedHash := strArg(req.Params.Arguments, "expected_sha256")
	reason := strArg(req.Params.Arguments, "_reason")

	if path == "" || totalSize <= 0 || reason == "" {
		return errResult("path, total_size, and _reason are required")
	}

	absPath, _ := filepath.Abs(path)
	_ = os.MkdirAll(filepath.Dir(absPath), 0755)

	sessionID := uuid.New().String()
	tmpPath := absPath + "." + sessionID + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return errResult(fmt.Sprintf("create tmp file failed: %v", err))
	}
	if err := f.Close(); err != nil {
		return errResult(fmt.Sprintf("close tmp file failed: %v", err))
	}

	session := &deploySession{
		ID: sessionID, Path: absPath, TmpPath: tmpPath, TotalSize: totalSize, ExpectedHash: expectedHash, Reason: reason, CreatedAt: time.Now(),
	}
	deploySessions.Store(sessionID, session)

	return jsonOK(map[string]any{"session_id": sessionID, "status": "initialized"})
}

func HandleAdminDeployPush(ctx context.Context, app *App, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := strArg(req.Params.Arguments, "session_id")
	content := strArg(req.Params.Arguments, "chunk_content")
	encoding := strArg(req.Params.Arguments, "encoding")
	offset := int64(intArg(req.Params.Arguments, "offset", 0))

	val, ok := deploySessions.Load(sessionID)
	if !ok {
		return errResult("session not found")
	}
	session := val.(*deploySession)

	var data []byte
	if encoding == "base64" {
		data, _ = base64.StdEncoding.DecodeString(content)
	} else {
		data = []byte(content)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	f, err := os.OpenFile(session.TmpPath, os.O_WRONLY, 0644)
	if err != nil {
		return errResult(fmt.Sprintf("open tmp file failed: %v", err))
	}
	defer f.Close()
	if _, err := f.WriteAt(data, offset); err != nil {
		return errResult(fmt.Sprintf("write chunk failed at offset %d: %v", offset, err))
	}

	stat, err := f.Stat()
	if err != nil {
		return errResult(fmt.Sprintf("stat tmp file failed: %v", err))
	}
	return jsonOK(map[string]any{"status": "pushing", "current_size": stat.Size()})
}

func HandleAdminDeployCommit(ctx context.Context, app *App, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := strArg(req.Params.Arguments, "session_id")
	val, ok := deploySessions.Load(sessionID)
	if !ok {
		return errResult("session not found")
	}
	session := val.(*deploySession)
	defer deploySessions.Delete(sessionID)

	session.mu.Lock()
	defer session.mu.Unlock()

	f, err := os.Open(session.TmpPath)
	if err != nil {
		return errResult(fmt.Sprintf("open tmp file failed: %v", err))
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return errResult(fmt.Sprintf("stat tmp file failed: %v", err))
	}
	if stat.Size() != session.TotalSize {
		return errResult("size mismatch")
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return errResult(fmt.Sprintf("checksum read failed: %v", err))
	}
	actualHash := fmt.Sprintf("%x", hasher.Sum(nil))
	if session.ExpectedHash != "" && session.ExpectedHash != actualHash {
		return errResult("checksum mismatch")
	}
	if err := f.Close(); err != nil {
		return errResult(fmt.Sprintf("close tmp file failed: %v", err))
	}

	if err := os.Rename(session.TmpPath, session.Path); err != nil {
		if copyErr := copyFile(session.TmpPath, session.Path); copyErr != nil {
			_ = os.Remove(session.TmpPath)
			return errResult(fmt.Sprintf("deploy failed: rename: %v, copy: %v", err, copyErr))
		}
		_ = os.Remove(session.TmpPath)
	}

	app.Logger.Log(core.EventAdminFileDeployed, "system", "", map[string]any{
		"path": session.Path, "size": session.TotalSize, "checksum": actualHash, "reason": session.Reason,
	})
	return jsonOK(map[string]any{"status": "success", "path": session.Path, "sha256": actualHash})
}

func HandleAdminSuspendUI(ctx context.Context, app *App, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	reason := strArg(req.Params.Arguments, "reason")
	if reason == "" {
		reason = "manual_optimization"
	}
	app.Logger.Log(core.EventUISuspendRequested, "system", "", map[string]any{"reason": reason})
	suspendFile := filepath.Join(filepath.Dir(app.ConfigPath), "tmp", "sig_ui_suspend")
	_ = os.MkdirAll(filepath.Dir(suspendFile), 0755)
	_ = os.WriteFile(suspendFile, []byte(reason), 0644)
	return jsonOK(map[string]any{"status": "suspending", "reason": reason})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
