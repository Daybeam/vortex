package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// jit_preflight_test.go — tests for the preflight gate (architecture §二.2).
// Covers all three gate classes: unknown language, unavailable runtime,
// safety violations, and the happy path. Pure-Go, no host runtime required.

func TestPreflightCheck_UnsupportedLanguage(t *testing.T) {
	t.Parallel()
	err := PreflightCheck("ruby", "puts 'hi'")
	if err == nil {
		t.Fatal("expected error for unsupported language")
	}
	if !errors.Is(err, ErrUnknownLanguage) {
		t.Errorf("expected ErrUnknownLanguage, got: %v", err)
	}
}

func TestPreflightCheck_EmbeddedLangAlwaysAvailable(t *testing.T) {
	t.Parallel()
	// js and lua have embedded backends — always available regardless of host.
	if err := PreflightCheck("js", "1+1"); err != nil {
		t.Errorf("js preflight should pass: %v", err)
	}
	if err := PreflightCheck("lua", "return 1"); err != nil {
		t.Errorf("lua preflight should pass: %v", err)
	}
}

func TestPreflightCheck_LangNormalization(t *testing.T) {
	t.Parallel()
	// "javascript", "ts", "node", "bun" all normalize to "js" (embedded goja).
	for _, lang := range []string{"javascript", "js", "ts", "typescript"} {
		if err := PreflightCheck(lang, "1"); err != nil {
			t.Errorf("%q should normalize to js and pass: %v", lang, err)
		}
	}
	// "py" normalizes to "python".
	pf := PreflightCheckDetailed("py", "print('x')")
	if pf.Lang != "python" {
		t.Errorf("py should normalize to python, got %q", pf.Lang)
	}
}

func TestPreflightCheck_EmptyCode(t *testing.T) {
	t.Parallel()
	err := PreflightCheck("js", "")
	if err == nil {
		t.Fatal("expected error for empty code")
	}
	if !errors.Is(err, ErrSafetyViolation) {
		t.Errorf("expected ErrSafetyViolation for empty code, got: %v", err)
	}
}

func TestPreflightCheck_UnbalancedBrackets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lang, code string
	}{
		{"js", "function f() { return 1;"},
		{"js", "console.log(1, 2"},
		{"lua", "print('hello'"},
		{"python", "print('hello'"},
	}
	for _, c := range cases {
		err := PreflightCheck(c.lang, c.code)
		if err == nil {
			t.Errorf("%q: expected unbalanced-bracket error, got nil", c.code)
			continue
		}
		if !errors.Is(err, ErrSafetyViolation) {
			t.Errorf("%q: expected ErrSafetyViolation, got: %v", c.code, err)
		}
	}
}

func TestPreflightCheck_BracketsInStrings(t *testing.T) {
	t.Parallel()
	// Brackets inside string literals should not count toward balance.
	code := `console.log("this has (unbalanced { brackets in it")`
	if err := PreflightCheck("js", code); err != nil {
		t.Errorf("brackets in strings should not trigger imbalance: %v", err)
	}
}

func TestPreflightCheck_PythonSafetyPatterns(t *testing.T) {
	t.Parallel()
	banned := []struct {
		code, why string
	}{
		{"import os; os.system('rm -rf /')", "os.system"},
		{"import subprocess; subprocess.run(['ls'])", "subprocess"},
		{"os.popen('ls')", "os.popen"},
		{"eval('1+1')", "eval"},
		{"exec('x=1')", "exec"},
	}
	for _, b := range banned {
		err := PreflightCheck("python", b.code)
		if err == nil {
			t.Errorf("%q: expected safety violation, got nil", b.why)
			continue
		}
		if !errors.Is(err, ErrSafetyViolation) {
			t.Errorf("%q: expected ErrSafetyViolation, got: %v", b.why, err)
		}
	}
}

func TestPreflightCheck_JSSafetyPatterns(t *testing.T) {
	t.Parallel()
	banned := []string{
		`require('child_process').execSync('ls')`,
		`require('fs').readFileSync('/etc/passwd')`,
		`process.env.SECRET`,
		`eval('1+1')`,
		`Function('return 1')()`,
	}
	for _, code := range banned {
		err := PreflightCheck("js", code)
		if err == nil {
			t.Errorf("expected safety violation for %q, got nil", code)
			continue
		}
		if !errors.Is(err, ErrSafetyViolation) {
			t.Errorf("expected ErrSafetyViolation for %q, got: %v", code, err)
		}
	}
}

func TestPreflightCheck_LuaSafetyPatterns(t *testing.T) {
	t.Parallel()
	banned := []string{
		`os.execute("rm -rf /")`,
		`io.popen("ls")`,
		`loadfile("/etc/passwd")`,
		`dofile("/etc/passwd")`,
		`os.getenv("SECRET")`,
	}
	for _, code := range banned {
		err := PreflightCheck("lua", code)
		if err == nil {
			t.Errorf("expected safety violation for %q, got nil", code)
			continue
		}
		if !errors.Is(err, ErrSafetyViolation) {
			t.Errorf("expected ErrSafetyViolation for %q, got: %v", code, err)
		}
	}
}

func TestPreflightCheck_SafeCodePasses(t *testing.T) {
	t.Parallel()
	safe := []struct{ lang, code string }{
		{"js", "console.log('hello world')"},
		{"js", "var x = 1 + 2; console.log(x)"},
		{"lua", "print('hello world')"},
		{"lua", "local x = 1 + 2; print(x)"},
		{"python", "print('hello world')"},
		{"python", "x = 1 + 2; print(x)"},
	}
	for _, s := range safe {
		// python may fail on availability if no host python; skip that check
		// by only asserting no safety violation.
		pf := PreflightCheckDetailed(s.lang, s.code)
		if errors.Is(pf.Err, ErrSafetyViolation) {
			t.Errorf("%q: safe code should not trigger safety violation: %v", s.code, pf.Err)
		}
		if errors.Is(pf.Err, ErrUnknownLanguage) {
			t.Errorf("%q: safe code should not trigger unknown language: %v", s.code, pf.Err)
		}
	}
}

func TestCapabilityPreflight(t *testing.T) {
	t.Parallel()
	// Role allows js + lua, not python.
	allowed := []string{"js", "lua"}
	if err := CapabilityPreflight("js", allowed); err != nil {
		t.Errorf("js should be allowed: %v", err)
	}
	if err := CapabilityPreflight("lua", allowed); err != nil {
		t.Errorf("lua should be allowed: %v", err)
	}
	err := CapabilityPreflight("python", allowed)
	if err == nil || !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("python should be denied with ErrCapabilityDenied, got: %v", err)
	}
	// Normalization: "javascript" should match "js" in the allowed set.
	if err := CapabilityPreflight("javascript", allowed); err != nil {
		t.Errorf("javascript should normalize to js and be allowed: %v", err)
	}
	// Empty allowed set denies everything.
	if err := CapabilityPreflight("js", nil); err == nil {
		t.Error("empty allowed set should deny js")
	}
}

func TestListRuntimeCapabilities(t *testing.T) {
	t.Parallel()
	caps := ListRuntimeCapabilities()
	if len(caps) < 2 {
		t.Fatalf("expected at least 2 runtime capabilities, got %d", len(caps))
	}
	// Embedded js + lua must always be present and available.
	foundJS, foundLua := false, false
	for _, c := range caps {
		if c.Lang == "js" {
			foundJS = true
			if !c.IsEmbedded || !c.IsAvailable {
				t.Errorf("js should be embedded+available, got %+v", c)
			}
		}
		if c.Lang == "lua" {
			foundLua = true
			if !c.IsEmbedded || !c.IsAvailable {
				t.Errorf("lua should be embedded+available, got %+v", c)
			}
		}
	}
	if !foundJS || !foundLua {
		t.Errorf("expected js and lua in registry, got: %+v", caps)
	}
}

func TestResolveRuntime(t *testing.T) {
	t.Parallel()
	c := ResolveRuntime("js")
	if c == nil || !c.IsEmbedded {
		t.Error("js should resolve to embedded runtime")
	}
	c = ResolveRuntime("nonexistent")
	if c != nil {
		t.Error("nonexistent lang should resolve to nil")
	}
}

// ---- Embedded runner tests -------------------------------------------------

func TestEmbeddedRunner_JS(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := EmbeddedRunner(ctx, "js", "1 + 2")
	if err != nil {
		t.Fatalf("js eval failed: %v", err)
	}
	if res.Result == nil {
		t.Fatal("expected non-nil result")
	}
	// goja exports numbers as int64 or float64 depending on value.
	switch v := res.Result.(type) {
	case int64:
		if v != 3 {
			t.Errorf("expected 3, got %d", v)
		}
	case float64:
		if v != 3 {
			t.Errorf("expected 3, got %f", v)
		}
	default:
		t.Errorf("expected numeric result, got %T: %v", v, v)
	}
}

func TestEmbeddedRunner_JSConsole(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := EmbeddedRunner(ctx, "js", "console.log('hello'); console.error('oops')")
	if err != nil {
		t.Fatalf("js eval failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("expected 'hello' in stdout, got %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "oops") {
		t.Errorf("expected 'oops' in stderr, got %q", res.Stderr)
	}
}

func TestEmbeddedRunner_Lua(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := EmbeddedRunner(ctx, "lua", "print('hello from lua')")
	if err != nil {
		t.Fatalf("lua eval failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello from lua") {
		t.Errorf("expected 'hello from lua' in stdout, got %q", res.Stdout)
	}
}

func TestEmbeddedRunner_LuaResult(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Lua scripts communicate results via the `result` global convention.
	res, err := EmbeddedRunner(ctx, "lua", "result = 42")
	if err != nil {
		t.Fatalf("lua eval failed: %v", err)
	}
	if res.Result != float64(42) {
		t.Errorf("expected result=42, got %v (%T)", res.Result, res.Result)
	}
}

func TestEmbeddedRunner_LuaTable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := EmbeddedRunner(ctx, "lua", "result = {1, 2, 3}")
	if err != nil {
		t.Fatalf("lua eval failed: %v", err)
	}
	arr, ok := res.Result.([]any)
	if !ok || len(arr) != 3 {
		t.Errorf("expected 3-element array, got %T: %v", res.Result, res.Result)
	}
}

func TestEmbeddedRunner_UnknownLang(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := EmbeddedRunner(ctx, "python", "print('hi')")
	if err == nil {
		t.Fatal("expected error for python (no embedded backend)")
	}
	if !errors.Is(err, ErrRuntimeNotAvailable) {
		t.Errorf("expected ErrRuntimeNotAvailable, got: %v", err)
	}
}

func TestEmbeddedRunner_JSSyntaxError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := EmbeddedRunner(ctx, "js", "this is not valid javascript!!!")
	if err == nil {
		t.Fatal("expected syntax error")
	}
}

func TestEmbeddedRunner_LuaSyntaxError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := EmbeddedRunner(ctx, "lua", "this is not valid lua!!!")
	if err == nil {
		t.Fatal("expected syntax error")
	}
}

func TestEmbeddedRunner_JSContextCancellation(t *testing.T) {
	t.Parallel()
	// Already-cancelled context should fail fast.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := EmbeddedRunner(ctx, "js", "1+1")
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}
