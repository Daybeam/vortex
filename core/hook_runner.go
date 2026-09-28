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

// ── Unified Hook Runner ─────────────────────────────────────────────────────
//
// Config-driven automated habits/rules that fire at specific lifecycle points
// in both chat and DAG tracks. Reuses the Lua VM and subprocess patterns from
// external_interceptor.go. See docs/completed/2026-09-28/UNIFIED_HOOK_ARCHITECTURE_DESIGN.md.

// HookPoint constants — the lifecycle stages where hooks fire.
const (
	HookChatHabit        = "chat_habit"         // chat: before pre-turn, analyzes conversation patterns
	HookChatPreTurn      = "chat_pre_turn"      // chat: before LLM call, can redirect/inject/abort
	HookChatPostTurn     = "chat_post_turn"     // chat: after LLM response, advisory annotation
	HookStepPre          = "step_pre"           // DAG: before step execution (= existing interceptors)
	HookStepPost         = "step_post"          // DAG: after step completion, advisory
	HookTaskCompletion   = "task_completion"    // DAG: after task reaches terminal state
	HookDecisionRequired = "decision_required"  // DAG: when a decision is needed, suggests a choice
)

// HookContext is the input passed to every hook script.
type HookContext struct {
	Point     string         `json:"point"`
	TaskID    string         `json:"task_id,omitempty"`
	StepID    string         `json:"step_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	RoleID    string         `json:"role_id,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	Output    map[string]any `json:"output,omitempty"`
}

// HookResult is what a hook script returns.
type HookResult struct {
	Action      string         `json:"action"`                 // none|inject_context|redirect_to_orchestrator|abort|annotate|suggest_decision|publish_event
	Data        map[string]any `json:"data,omitempty"`
	Annotations []HookAnnotation `json:"annotations,omitempty"`
}

// HookAnnotation is an advisory note attached to a response or step.
type HookAnnotation struct {
	Type string `json:"type"` // warning|info|link|citation
	Text string `json:"text"`
}

// HookRunner loads hooks from config, matches filters, and executes scripts.
// It is safe for concurrent use — each Run call creates its own Lua state or
// subprocess.
type HookRunner struct {
	hooks    []config.HookConfig
	runtimes config.ExternalRuntimes
	logger   *Logger
}

// NewHookRunner creates a HookRunner from config.
func NewHookRunner(hooks []config.HookConfig, runtimes config.ExternalRuntimes, logger *Logger) *HookRunner {
	return &HookRunner{hooks: hooks, runtimes: runtimes, logger: logger}
}

// HooksForPoint returns all configured hooks for a given point, after filter
// matching. This is the main entry point for callers.
func (r *HookRunner) HooksForPoint(point string) []config.HookConfig {
	var result []config.HookConfig
	for _, h := range r.hooks {
		if h.Point == point {
			result = append(result, h)
		}
	}
	return result
}

// Run executes all hooks for a given point that match the filter.
// Returns the aggregated result (last non-"none" action wins).
// Errors in individual hooks are logged and skipped (fail-safe).
func (r *HookRunner) Run(ctx context.Context, point string, hctx HookContext) HookResult {
	var result HookResult
	result.Action = "none"

	for _, h := range r.HooksForPoint(point) {
		if !matchFilter(h.Filter, hctx) {
			continue
		}

		timeout := time.Duration(h.TimeoutMs) * time.Millisecond
		if timeout == 0 {
			timeout = 10 * time.Second
		}

		hr, err := r.runOne(ctx, h.Script, timeout, hctx)
		if err != nil {
			r.logger.Log("EventHookError", hctx.TaskID, hctx.StepID, map[string]any{
				"point":  point,
				"script": h.Script,
				"error":  err.Error(),
			})
			continue // fail-safe: skip failed hook
		}

		if hr.Action != "none" && hr.Action != "" {
			result = hr // last non-trivial action wins
		}
		if len(hr.Annotations) > 0 {
			result.Annotations = append(result.Annotations, hr.Annotations...)
		}
	}

	return result
}

// matchFilter returns true if all filter conditions match the context.
func matchFilter(f config.HookFilter, hctx HookContext) bool {
	if f.RoleID != "" && f.RoleID != hctx.RoleID {
		return false
	}
	if f.TaskContains != "" {
		task, _ := hctx.Input["task"].(string)
		if task == "" {
			task, _ = hctx.Input["user_message"].(string)
		}
		if !strings.Contains(strings.ToLower(task), strings.ToLower(f.TaskContains)) {
			return false
		}
	}
	if f.TaskType != "" {
		// TaskType matches against the role's BaseCapability, which the caller
		// should put in Input["task_type"]. If absent, filter fails.
		tt, _ := hctx.Input["task_type"].(string)
		if tt != f.TaskType {
			return false
		}
	}
	if f.StepIDGlob != "" {
		matched, _ := filepath.Match(f.StepIDGlob, hctx.StepID)
		if !matched {
			return false
		}
	}
	return true
}

// runOne executes a single hook script (Lua or subprocess).
func (r *HookRunner) runOne(ctx context.Context, scriptPath string, timeout time.Duration, hctx HookContext) (HookResult, error) {
	ext := strings.ToLower(filepath.Ext(scriptPath))

	switch ext {
	case ".lua":
		return r.runLua(ctx, scriptPath, hctx)
	case ".py", ".js", ".sh":
		return r.runSubprocess(ctx, scriptPath, timeout, hctx)
	default:
		if cmdParts, ok := r.runtimes.Runtimes[ext]; ok && len(cmdParts) > 0 {
			return r.runSubprocessWithCmd(ctx, scriptPath, cmdParts[0], cmdParts[1:], timeout, hctx)
		}
		return HookResult{}, fmt.Errorf("unsupported hook extension: %s", ext)
	}
}

// runLua executes a Lua hook script. Reuses the pattern from luaInterceptor.
func (r *HookRunner) runLua(ctx context.Context, scriptPath string, hctx HookContext) (HookResult, error) {
	L := lua.NewState()
	defer L.Close()
	lua.OpenBase(L)
	lua.OpenTable(L)
	lua.OpenString(L)

	// Build context table
	ctxTable := L.NewTable()
	ctxTable.RawSetString("point", lua.LString(hctx.Point))
	ctxTable.RawSetString("task_id", lua.LString(hctx.TaskID))
	ctxTable.RawSetString("step_id", lua.LString(hctx.StepID))
	ctxTable.RawSetString("session_id", lua.LString(hctx.SessionID))
	ctxTable.RawSetString("role_id", lua.LString(hctx.RoleID))

	inputTable := L.NewTable()
	for k, v := range hctx.Input {
		ctxTable.RawSetString(k, luaValue(L, v))
		_ = inputTable // suppress unused
	}
	ctxTable.RawSetString("input", inputTable)

	L.SetGlobal("hook_context", ctxTable)

	if err := L.DoFile(scriptPath); err != nil {
		return HookResult{}, fmt.Errorf("lua hook failed: %w", err)
	}

	// Read result from global "hook_result"
	result := HookResult{Action: "none"}
	if r := L.GetGlobal("hook_result"); r.Type() == lua.LTTable {
		tbl := r.(*lua.LTable)
		if action, ok := tbl.RawGetString("action").(lua.LString); ok {
			result.Action = string(action)
		}
		if data := tbl.RawGetString("data"); data.Type() == lua.LTTable {
			result.Data = luaTableToMap(data.(*lua.LTable))
		}
	}

	return result, nil
}

// runSubprocess executes a .py/.js/.sh hook script via stdin/stdout JSON.
func (r *HookRunner) runSubprocess(ctx context.Context, scriptPath string, timeout time.Duration, hctx HookContext) (HookResult, error) {
	ext := strings.ToLower(filepath.Ext(scriptPath))
	var exe string
	switch ext {
	case ".py":
		exe = r.runtimes.PythonPath
		if exe == "" {
			exe = env.GetPythonCmd()
		}
	case ".js":
		exe = r.runtimes.NodePath
		if exe == "" {
			exe = "node"
		}
	case ".sh":
		exe = "sh"
	default:
		return HookResult{}, fmt.Errorf("unsupported subprocess extension: %s", ext)
	}
	return r.runSubprocessWithCmd(ctx, scriptPath, exe, nil, timeout, hctx)
}

// runSubprocessWithCmd runs a hook script with an arbitrary command.
func (r *HookRunner) runSubprocessWithCmd(ctx context.Context, scriptPath, exe string, extraArgs []string, timeout time.Duration, hctx HookContext) (HookResult, error) {
	inputBytes, _ := json.Marshal(hctx)

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append(append([]string{}, extraArgs...), scriptPath)
	cmd := exec.CommandContext(cmdCtx, exe, args...)
	cmd.Stdin = bytes.NewReader(inputBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return HookResult{}, fmt.Errorf("hook subprocess failed: %w (stderr: %s)", err, stderr.String())
	}

	var result HookResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return HookResult{Action: "none"}, nil // script ran but no valid JSON = no action
	}
	if result.Action == "" {
		result.Action = "none"
	}
	return result, nil
}

// ── Lua helpers ─────────────────────────────────────────────────────────────

func luaValue(L *lua.LState, v any) lua.LValue {
	switch val := v.(type) {
	case string:
		return lua.LString(val)
	case int:
		return lua.LNumber(val)
	case int64:
		return lua.LNumber(val)
	case float64:
		return lua.LNumber(val)
	case bool:
		return lua.LBool(val)
	default:
		b, _ := json.Marshal(v)
		return lua.LString(string(b))
	}
}

func luaTableToMap(tbl *lua.LTable) map[string]any {
	result := make(map[string]any)
	tbl.ForEach(func(k, v lua.LValue) {
		key := k.String()
		switch v.Type() {
		case lua.LTString:
			result[key] = string(v.(lua.LString))
		case lua.LTNumber:
			result[key] = float64(v.(lua.LNumber))
		case lua.LTBool:
			result[key] = bool(v.(lua.LBool))
		default:
			result[key] = v.String()
		}
	})
	return result
}
