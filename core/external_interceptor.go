package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
	lua "github.com/yuin/gopher-lua"
)

// ExternalInterceptor executes an external script (Lua, Python, Node) as a middleware.
func NewExternalInterceptor(scriptPath string, runtimes config.ExternalRuntimes, logger *Logger) Interceptor {
	ext := strings.ToLower(filepath.Ext(scriptPath))

	switch ext {
	case ".lua":
		return luaInterceptor(scriptPath, logger)
	case ".py", ".js":
		return subprocessInterceptor(scriptPath, runtimes, logger)
	default:
		// FIX (2026-07-14): mirror the same fix applied to
		// providers/external_provider.go - a developer can register any
		// extension via config.ExternalRuntimes.Runtimes (e.g.
		// {".rb": ["ruby"]}) without editing this switch and recompiling.
		if cmdParts, ok := runtimes.Runtimes[ext]; ok && len(cmdParts) > 0 {
			return subprocessInterceptorWithCommand(scriptPath, cmdParts[0], cmdParts[1:], logger)
		}
		return func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
			logger.Log("EventInterceptorError", req.TaskID, req.StepID, map[string]any{
				"error": fmt.Sprintf("Unsupported interceptor extension: %s (register it in config.external_runtimes.runtimes)", ext),
				"path":  scriptPath,
			})
			return next(ctx, req)
		}
	}
}

func luaInterceptor(scriptPath string, logger *Logger) Interceptor {
	return func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
		L := lua.NewState()
		defer L.Close()
		lua.OpenBase(L)
		lua.OpenTable(L)
		lua.OpenString(L)

		// Prepare request table
		reqTable := L.NewTable()
		reqTable.RawSetString("task_id", lua.LString(req.TaskID))
		reqTable.RawSetString("step_id", lua.LString(req.StepID))
		reqTable.RawSetString("role_id", lua.LString(req.RoleID))
		reqTable.RawSetString("task", lua.LString(req.Task))

		L.SetGlobal("request", reqTable)

		if err := L.DoFile(scriptPath); err != nil {
			logger.Log("EventInterceptorError", req.TaskID, req.StepID, map[string]any{
				"error": fmt.Sprintf("Lua interceptor failed: %v", err),
				"path":  scriptPath,
			})
			return next(ctx, req)
		}

		// Allow script to modify request fields
		if r := L.GetGlobal("request"); r.Type() == lua.LTTable {
			tbl := r.(*lua.LTable)
			if t, ok := tbl.RawGetString("task").(lua.LString); ok {
				req.Task = string(t)
			}
		}

		res, err := next(ctx, req)
		if err != nil {
			return res, err
		}

		// Post-execution hook in Lua if defined
		if post := L.GetGlobal("post_process"); post.Type() == lua.LTFunction {
			// Implementation for post-processing could go here
		}

		return res, nil
	}
}

func subprocessInterceptor(scriptPath string, runtimes config.ExternalRuntimes, logger *Logger) Interceptor {
	ext := strings.ToLower(filepath.Ext(scriptPath))
	var exe string
	if ext == ".py" {
		exe = runtimes.PythonPath
		if exe == "" {
			exe = env.GetPythonCmd()
		}
	} else {
		exe = runtimes.NodePath
		if exe == "" {
			exe = "node"
		}
	}
	return subprocessInterceptorWithCommand(scriptPath, exe, nil, logger)
}

// subprocessInterceptorWithCommand runs scriptPath via an arbitrary
// [exe, extraArgs..., scriptPath] command line. Added 2026-07-14 so any
// extension registered in config.ExternalRuntimes.Runtimes (not just the
// built-in .py/.js) can be used as an interceptor.
func subprocessInterceptorWithCommand(scriptPath, exe string, extraArgs []string, logger *Logger) Interceptor {
	return func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
		runtime := exe

		// Interceptor inputs
		input := map[string]any{
			"phase":   "pre",
			"request": req,
		}
		inputBytes, _ := json.Marshal(input)

		cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		args := append(append([]string{}, extraArgs...), scriptPath)
		cmd := exec.CommandContext(cmdCtx, exe, args...)
		cmd.Stdin = bytes.NewReader(inputBytes)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			logger.Log("EventInterceptorError", req.TaskID, req.StepID, map[string]any{
				"runtime": runtime,
				"error":   err.Error(),
				"stderr":  stderr.String(),
			})
			// Fail-safe: continue to next handler even if interceptor fails
			return next(ctx, req)
		}

		// Parse modifications from script
		var mods struct {
			Request *SpawnRequest `json:"request"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &mods); err == nil && mods.Request != nil {
			// Apply specific modifications (Task, Metadata)
			if mods.Request.Task != "" {
				req.Task = mods.Request.Task
			}
			if mods.Request.Metadata != nil {
				if req.Metadata == nil {
					req.Metadata = make(map[string]any)
				}
				for k, v := range mods.Request.Metadata {
					req.Metadata[k] = v
				}
			}
		}

		return next(ctx, req)
	}
}
