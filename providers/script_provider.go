package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/safelimits"
	lua "github.com/yuin/gopher-lua"
)

// ScriptProvider adapts a Lua script into the Provider interface.
// The Lua script must define a global `provider` table with `name` (string)
// and `complete` (function) fields.
type ScriptProvider struct {
	cfg        *config.ProviderConfig
	client     *http.Client
	scriptPath string
	mu         sync.RWMutex
	vm         *lua.LState
	lastLoaded time.Time
}

// NewScriptProvider creates a new ScriptProvider by loading the given Lua script.
func NewScriptProvider(cfg *config.ProviderConfig, scriptPath string) (*ScriptProvider, error) {
	sp := &ScriptProvider{
		cfg:        cfg,
		client:     &http.Client{Timeout: 120 * time.Second},
		scriptPath: scriptPath,
	}
	if err := sp.loadScript(); err != nil {
		return nil, fmt.Errorf("load script %s: %w", scriptPath, err)
	}
	return sp, nil
}

// Name returns the provider name defined in the Lua script.
func (sp *ScriptProvider) Name() string {
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	prov := sp.vm.GetGlobal("provider")
	if tbl, ok := prov.(*lua.LTable); ok {
		if name, ok := tbl.RawGetString("name").(lua.LString); ok {
			return string(name)
		}
	}
	return "script"
}

// Complete calls the Lua provider.complete(req) function.
func (sp *ScriptProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	// gopher-lua LState is NOT thread-safe; use exclusive Lock, not RLock.
	sp.mu.Lock()
	defer sp.mu.Unlock()

	// Build request table
	reqTable := sp.vm.NewTable()
	reqTable.RawSetString("system", lua.LString(req.System))
	reqTable.RawSetString("user", lua.LString(req.User))
	reqTable.RawSetString("model", lua.LString(req.Model))
	reqTable.RawSetString("max_tokens", lua.LNumber(req.MaxTokens))

	// Get provider.complete function
	prov := sp.vm.GetGlobal("provider")
	provTbl, ok := prov.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("script does not define a 'provider' table")
	}
	completeFn := provTbl.RawGetString("complete")
	if completeFn.Type() != lua.LTFunction {
		return nil, fmt.Errorf("provider.complete is not a function")
	}

	// Call: result, err = provider.complete(req)
	if err := sp.vm.CallByParam(lua.P{
		Fn:      completeFn,
		NRet:    2,
		Protect: true,
	}, reqTable); err != nil {
		return nil, fmt.Errorf("lua error: %w", err)
	}

	// Pop return values (in reverse order on stack)
	errVal := sp.vm.Get(-1)
	resultVal := sp.vm.Get(-2)
	sp.vm.Pop(2)

	// Check for error
	if errVal != lua.LNil {
		if errStr, ok := errVal.(lua.LString); ok {
			errMsg := string(errStr)
			if isRateLimitError(errMsg) {
				return nil, &RateLimitError{Msg: errMsg}
			}
			return nil, &ProviderError{Msg: errMsg}
		}
	}

	// Parse result table
	resultTbl, ok := resultVal.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("provider.complete must return a table as first value, got %s", resultVal.Type())
	}

	resp := &ProviderResponse{}
	if text, ok := resultTbl.RawGetString("text").(lua.LString); ok {
		resp.Text = string(text)
	}
	if stopReason, ok := resultTbl.RawGetString("stop_reason").(lua.LString); ok {
		resp.StopReason = string(stopReason)
	}

	// Parse tool_calls if present
	if toolCallsVal := resultTbl.RawGetString("tool_calls"); toolCallsVal != lua.LNil {
		if toolCallsTbl, ok := toolCallsVal.(*lua.LTable); ok {
			toolCallsTbl.ForEach(func(_ lua.LValue, v lua.LValue) {
				if tc, ok := v.(*lua.LTable); ok {
					call := ToolCall{}
					if name, ok := tc.RawGetString("name").(lua.LString); ok {
						call.Name = string(name)
					}
					if callID, ok := tc.RawGetString("call_id").(lua.LString); ok {
						call.CallID = string(callID)
					}
					if args := tc.RawGetString("arguments"); args != lua.LNil {
						call.Arguments = luaTableToMap(args)
					}
					resp.ToolCalls = append(resp.ToolCalls, call)
				}
			})
		}
	}

	return resp, nil
}

// StreamComplete is a fallback that delegates to Complete for ScriptProvider.
func (sp *ScriptProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	resp, err := sp.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := onChunk(resp.Text); err != nil {
		return nil, err
	}
	return resp, nil
}

func (sp *ScriptProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by ScriptProvider")
}

// Reload reloads the Lua script from disk (hot-reload).
func (sp *ScriptProvider) Reload() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	// Close old VM
	if sp.vm != nil {
		sp.vm.Close()
	}
	return sp.loadScript()
}

// ScriptPath returns the path to the Lua script file.
func (sp *ScriptProvider) ScriptPath() string {
	return sp.scriptPath
}

// LastLoaded returns when the script was last loaded.
func (sp *ScriptProvider) LastLoaded() time.Time {
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	return sp.lastLoaded
}

// ─── Internal ─────────────────────────────────────────────────────────────

func (sp *ScriptProvider) loadScript() error {
	vm := lua.NewState(lua.Options{
		SkipOpenLibs: true, // Sandboxed: no os, io, etc.
	})
	// Only open safe libraries
	lua.OpenBase(vm)
	lua.OpenString(vm)
	lua.OpenTable(vm)
	lua.OpenMath(vm)

	// Register bridge APIs
	sp.registerBridge(vm)

	// Load script
	if err := vm.DoFile(sp.scriptPath); err != nil {
		vm.Close()
		return err
	}

	// Validate: script must define provider.complete
	prov := vm.GetGlobal("provider")
	provTbl, ok := prov.(*lua.LTable)
	if !ok {
		vm.Close()
		return fmt.Errorf("script must define a global 'provider' table")
	}
	if provTbl.RawGetString("complete").Type() != lua.LTFunction {
		vm.Close()
		return fmt.Errorf("provider table must have a 'complete' function")
	}

	sp.vm = vm
	sp.lastLoaded = time.Now()
	return nil
}

// registerBridge injects Go-provided functions into the Lua VM.
func (sp *ScriptProvider) registerBridge(vm *lua.LState) {
	// ── env(name) ?string ─────────────────────────────────────────────
	vm.SetGlobal("env", vm.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		L.Push(lua.LString(os.Getenv(name)))
		return 1
	}))

	// ── json.encode(table) ?string ────────────────────────────────────
	// ── json.decode(string) ?table ────────────────────────────────────
	jsonMod := vm.NewTable()
	vm.SetField(jsonMod, "encode", vm.NewFunction(func(L *lua.LState) int {
		val := L.CheckAny(1)
		goVal := luaValueToGo(val)
		data, err := json.Marshal(goVal)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(string(data)))
		return 1
	}))
	vm.SetField(jsonMod, "decode", vm.NewFunction(func(L *lua.LState) int {
		str := L.CheckString(1)
		var goVal any
		if err := json.Unmarshal([]byte(str), &goVal); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(goValueToLua(L, goVal))
		return 1
	}))
	vm.SetGlobal("json", jsonMod)

	// ── http.post(url, body, headers) ?(body, status, err) ────────────
	httpMod := vm.NewTable()
	vm.SetField(httpMod, "post", vm.NewFunction(func(L *lua.LState) int {
		url := L.CheckString(1)
		body := L.CheckString(2)
		headersTbl := L.OptTable(3, nil)

		httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LNumber(0))
			L.Push(lua.LString(err.Error()))
			return 3
		}

		// Set headers from Lua table
		if headersTbl != nil {
			headersTbl.ForEach(func(k, v lua.LValue) {
				if ks, ok := k.(lua.LString); ok {
					if vs, ok := v.(lua.LString); ok {
						httpReq.Header.Set(string(ks), string(vs))
					}
				}
			})
		}

		resp, err := sp.client.Do(httpReq)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LNumber(0))
			L.Push(lua.LString(err.Error()))
			return 3
		}
		defer resp.Body.Close()

		// audit H1: cap to prevent OOM from unbounded responses
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LNumber(resp.StatusCode))
			L.Push(lua.LString(err.Error()))
			return 3
		}

		L.Push(lua.LString(string(respBody)))
		L.Push(lua.LNumber(resp.StatusCode))
		L.Push(lua.LNil)
		return 3
	}))
	vm.SetGlobal("http", httpMod)

	// ── log.info(msg) / log.error(msg) ─────────────────────────────────
	logMod := vm.NewTable()
	vm.SetField(logMod, "info", vm.NewFunction(func(L *lua.LState) int {
		msg := L.CheckString(1)
		fmt.Fprintf(os.Stderr, "[script:%s] INFO: %s\n", sp.scriptPath, msg)
		return 0
	}))
	vm.SetField(logMod, "error", vm.NewFunction(func(L *lua.LState) int {
		msg := L.CheckString(1)
		fmt.Fprintf(os.Stderr, "[script:%s] ERROR: %s\n", sp.scriptPath, msg)
		return 0
	}))
	vm.SetGlobal("log", logMod)
}

// ─── Lua ?Go value conversion ───────────────────────────────────────────

// luaValueToGo converts a Lua value to a Go value for JSON serialization.
func luaValueToGo(val lua.LValue) any {
	switch v := val.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		f := float64(v)
		if f == float64(int64(f)) {
			return int64(f)
		}
		return f
	case lua.LString:
		return string(v)
	case *lua.LTable:
		return luaTableToInterface(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// luaTableToInterface converts a Lua table to either a Go slice (array) or map.
func luaTableToInterface(tbl *lua.LTable) any {
	// Check if it's an array (sequential integer keys starting from 1)
	maxN := tbl.MaxN()
	if maxN > 0 {
		arr := make([]any, 0, maxN)
		for i := 1; i <= maxN; i++ {
			arr = append(arr, luaValueToGo(tbl.RawGetInt(i)))
		}
		return arr
	}
	// Otherwise treat as map
	m := make(map[string]any)
	tbl.ForEach(func(k, v lua.LValue) {
		if ks, ok := k.(lua.LString); ok {
			m[string(ks)] = luaValueToGo(v)
		}
	})
	return m
}

// luaTableToMap is a convenience for extracting map[string]any from a Lua value.
func luaTableToMap(val lua.LValue) map[string]any {
	if tbl, ok := val.(*lua.LTable); ok {
		result := make(map[string]any)
		tbl.ForEach(func(k, v lua.LValue) {
			if ks, ok := k.(lua.LString); ok {
				result[string(ks)] = luaValueToGo(v)
			}
		})
		return result
	}
	return nil
}

// goValueToLua converts a Go value (from json.Unmarshal) to a Lua value.
func goValueToLua(L *lua.LState, val any) lua.LValue {
	switch v := val.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(v)
	case float64:
		return lua.LNumber(v)
	case string:
		return lua.LString(v)
	case []any:
		tbl := L.NewTable()
		for _, item := range v {
			tbl.Append(goValueToLua(L, item))
		}
		return tbl
	case map[string]any:
		tbl := L.NewTable()
		for key, item := range v {
			tbl.RawSetString(key, goValueToLua(L, item))
		}
		return tbl
	default:
		return lua.LString(fmt.Sprintf("%v", v))
	}
}

// isRateLimitError checks if an error message indicates rate limiting.
func isRateLimitError(msg string) bool {
	return len(msg) >= 12 && msg[:12] == "RATE_LIMITED"
}
