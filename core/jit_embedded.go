package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
	lua "github.com/yuin/gopher-lua"
)

// jit_embedded.go — Embedded JIT execution tier.
// Implements §二.1 (Embedded Tier) of docs/architecture/EMBEDDED_JIT_AND_PREFLIGHT_DESIGN.md.
//
// Two pure-Go engines are compiled into the binary:
//   - goja       (dop251/goja):  ECMAScript 5.1. Used for lang="js".
//   - gopher-lua (yuin/gopher-lua): Lua 5.1 with context-cancellation support
//     (L.SetContext) for runaway-loop protection. Used for lang="lua".
//
// Both engines execute in-process — no host binary required. This is the
// "zero external dependency" tier that makes the single-binary distribution
// reliable on Windows / minimal Linux.
//
// Safety: callers MUST have already passed PreflightCheck before invoking
// these runners. The runners do NOT re-lint (that would double the cost);
// they trust the gate and focus on execution + resource limits.

// EmbeddedRunResult is the unified return shape for both embedded engines.
// It mirrors JITSession.EvalResult so callers can treat embedded and managed
// results uniformly.
type EmbeddedRunResult struct {
	Stdout  string        `json:"stdout"`
	Stderr  string        `json:"stderr"`
	Result  any           `json:"result"`
	Elapsed time.Duration `json:"elapsed"`
}

// EmbeddedRunner is the entry point for in-process JIT execution. It picks
// the engine by canonical lang and enforces a per-call CPU budget via ctx.
// Returns a wrapped ErrRuntimeNotAvailable if lang has no embedded backend
// (e.g. "python" — python has no embedded tier by design).
func EmbeddedRunner(ctx context.Context, lang, code string) (*EmbeddedRunResult, error) {
	canonical := normalizeLang(lang)
	switch canonical {
	case "js":
		return runGoja(ctx, code)
	case "lua":
		return runGopherLua(ctx, code)
	default:
		return nil, fmt.Errorf("%w: no embedded backend for %q (managed tier requires host runtime)",
			ErrRuntimeNotAvailable, canonical)
	}
}

// ---- goja (JavaScript) -----------------------------------------------------

// runGoja executes JS in a fresh goja runtime. A fresh runtime per call is
// intentional: it gives us free isolation (no state leaks between calls) at
// the cost of re-parse, which is acceptable for the short scripts JIT tools
// run. For stateful JS, callers should use the session mechanism (which
// today is python-only; a JS session variant can follow the same pattern).
//
// goja has no built-in instruction-count limit and no context hook, so we
// enforce the ctx deadline by running Run in a goroutine and selecting on
// ctx.Done(). When ctx is cancelled we call vm.Interrupt(nil), which causes
// goja's VM loop to stop and RunString to return *InterruptedError. We then
// drain the result channel to ensure the goroutine has fully exited — no leak.
// The script is also bounded by the preflight safety lint and the caller's
// timeout.
func runGoja(ctx context.Context, code string) (*EmbeddedRunResult, error) {
	start := time.Now()
	vm := goja.New()

	// Capture console.log/error output into our result buffers.
	var stdout, stderr strings.Builder
	console := map[string]func(goja.FunctionCall) goja.Value{
		"log":   func(fc goja.FunctionCall) goja.Value { return jsConsoleWrite(&stdout, fc) },
		"info":  func(fc goja.FunctionCall) goja.Value { return jsConsoleWrite(&stdout, fc) },
		"warn":  func(fc goja.FunctionCall) goja.Value { return jsConsoleWrite(&stderr, fc) },
		"error": func(fc goja.FunctionCall) goja.Value { return jsConsoleWrite(&stderr, fc) },
	}
	consoleObj := vm.NewObject()
	for name, fn := range console {
		_ = consoleObj.Set(name, fn)
	}
	_ = vm.Set("console", consoleObj)

	type execOutcome struct {
		val goja.Value
		err error
	}
	outCh := make(chan execOutcome, 1)
	go func() {
		val, err := vm.RunString(code)
		outCh <- execOutcome{val, err}
	}()

	select {
	case out := <-outCh:
		res := &EmbeddedRunResult{
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Elapsed: time.Since(start),
		}
		if out.err != nil {
			// goja wraps JS exceptions in *goja.Exception; surface the message.
			var jsExc *goja.Exception
			if errors.As(out.err, &jsExc) {
				res.Stderr += jsExc.String()
				return res, fmt.Errorf("js runtime error: %s", jsExc.String())
			}
			return res, fmt.Errorf("js execution failed: %w", out.err)
		}
		res.Result = gojaToGo(out.val)
		return res, nil
	case <-ctx.Done():
		// Interrupt the VM so the goroutine exits instead of leaking.
		// goja's Interrupt causes RunString to return *InterruptedError.
		vm.Interrupt(nil)
		// Drain the channel to ensure the goroutine has fully exited.
		<-outCh
		return &EmbeddedRunResult{
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Elapsed: time.Since(start),
		}, fmt.Errorf("js execution cancelled: %w", ctx.Err())
	}
}

// jsConsoleWrite appends all arguments (space-separated, like Node's console)
// to the given builder and returns undefined.
func jsConsoleWrite(b *strings.Builder, fc goja.FunctionCall) goja.Value {
	parts := make([]string, 0, len(fc.Arguments))
	for _, arg := range fc.Arguments {
		parts = append(parts, arg.String())
	}
	b.WriteString(strings.Join(parts, " "))
	b.WriteByte('\n')
	return goja.Undefined()
}

// gojaToGo unwraps a goja.Value into a plain Go value for JSON marshalling.
// goja.Export handles numbers, strings, bools, arrays, and plain objects
// well enough for our JIT result surface; nil/undefined/null collapse to nil.
func gojaToGo(v goja.Value) any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return v.Export()
}

// ---- gopher-lua (Lua) ------------------------------------------------------

// runGopherLua executes Lua in a fresh gopher-lua VM. Cancellation flows
// through L.SetContext(ctx): gopher-lua's main loop checks ctx between VM
// instructions and raises a "context canceled" error when ctx is done, which
// unwinds the stack cleanly — no leaked goroutine, no os.Exit (unlike
// SetMx, which we deliberately avoid).
//
// Stdlib is skipped (Options.SkipOpenLibs=true) so even a lint bypass can't
// reach os/io. We provide a minimal `print`/`print_err` surface instead.
func runGopherLua(ctx context.Context, code string) (*EmbeddedRunResult, error) {
	start := time.Now()
	L := lua.NewState(lua.Options{
		SkipOpenLibs: true,
	})
	defer L.Close()

	// Open only the safe base libs that don't touch the host. OpenBase gives
	// us assert, error, pairs, tostring, tonumber, type, unpack, select, etc.
	// We deliberately do NOT open io/os/package/loadfile/dofile.
	lua.OpenBase(L)
	lua.OpenTable(L)
	lua.OpenString(L)
	lua.OpenMath(L)

	var stdout, stderr strings.Builder
	// Override `print` (base lib provides one that writes to os.Stdout) with
	// a capturing version. Provide `print_err` for stderr symmetry.
	L.SetGlobal("print", L.NewFunction(func(l *lua.LState) int {
		top := l.GetTop()
		parts := make([]string, 0, top)
		for i := 1; i <= top; i++ {
			parts = append(parts, l.ToStringMeta(l.Get(i)).String())
		}
		stdout.WriteString(strings.Join(parts, "\t"))
		stdout.WriteByte('\n')
		return 0
	}))
	L.SetGlobal("print_err", L.NewFunction(func(l *lua.LState) int {
		top := l.GetTop()
		parts := make([]string, 0, top)
		for i := 1; i <= top; i++ {
			parts = append(parts, l.ToStringMeta(l.Get(i)).String())
		}
		stderr.WriteString(strings.Join(parts, "\t"))
		stderr.WriteByte('\n')
		return 0
	}))

	// Wire ctx so a cancelled context unwinds the VM cleanly.
	L.SetContext(ctx)

	type execOutcome struct {
		err error
	}
	outCh := make(chan execOutcome, 1)
	go func() {
		err := L.DoString(code)
		outCh <- execOutcome{err}
	}()

	select {
	case out := <-outCh:
		res := &EmbeddedRunResult{
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Elapsed: time.Since(start),
		}
		if out.err != nil {
			res.Stderr += out.err.Error() + "\n"
			if ctx.Err() != nil {
				return res, fmt.Errorf("lua execution cancelled: %w", out.err)
			}
			return res, fmt.Errorf("lua runtime error: %w", out.err)
		}
		// Lua scripts return values via `return X`; gopher-lua surfaces the
		// last returned value on the stack after DoString. We also check the
		// `result` global as a convention for scripts that set it explicitly.
		if rv := L.GetGlobal("result"); rv != nil && rv.Type() != lua.LTNil {
			res.Result = luaToGo(rv)
		}
		return res, nil
	case <-ctx.Done():
		return &EmbeddedRunResult{
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Elapsed: time.Since(start),
		}, fmt.Errorf("lua execution cancelled: %w", ctx.Err())
	}
}

// luaToGo coerces a lua.LValue to a plain Go value for JSON marshalling.
func luaToGo(v lua.LValue) any {
	if v == nil {
		return nil
	}
	switch v.Type() {
	case lua.LTNil:
		return nil
	case lua.LTBool:
		return bool(v.(lua.LBool))
	case lua.LTNumber:
		return float64(v.(lua.LNumber))
	case lua.LTString:
		return string(v.(lua.LString))
	case lua.LTTable:
		t := v.(*lua.LTable)
		// Try array first.
		if arr := tableToSlice(t); arr != nil {
			return arr
		}
		return tableToMap(t)
	default:
		return v.String()
	}
}

func tableToSlice(t *lua.LTable) []any {
	n := t.Len()
	if n == 0 {
		return nil
	}
	out := make([]any, 0, n)
	for i := 1; i <= n; i++ {
		v := t.RawGetInt(i)
		if v == lua.LNil {
			return nil // not a dense array
		}
		out = append(out, luaToGo(v))
	}
	return out
}

func tableToMap(t *lua.LTable) map[string]any {
	out := make(map[string]any)
	t.ForEach(func(k, v lua.LValue) {
		out[k.String()] = luaToGo(v)
	})
	if len(out) == 0 {
		return nil
	}
	return out
}
