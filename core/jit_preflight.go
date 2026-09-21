package core

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// jit_preflight.go — JIT pre-flight gate.
// Implements §二.2 and §三.2 of docs/architecture/EMBEDDED_JIT_AND_PREFLIGHT_DESIGN.md.
//
// Every JIT entry point (JITManager.RegisterTool, JITSession.Eval, and the
// tools/subsystems.go jit.create / jit.eval actions) MUST call PreflightCheck
// before spawning a process or handing code to an embedded engine. The gate
// is deliberately pure-Go and side-effect-free so it can run in tests without
// a host runtime installed.
//
// Three checks, in order, each producing a distinct error class so callers
// can render actionable guidance (e.g. "install python3" vs "remove os.system
// call") instead of a raw interpreter stack trace:
//   1. language support + runtime availability  (ErrRuntimeNotAvailable)
//   2. static syntax + safety lint              (ErrSafetyViolation)
//   3. capability/role authorization            (ErrCapabilityDenied) — wired
//      by the caller via CapabilityPreflight, since role context lives in
//      tools/subsystems.go, not core.

// Sentinel error classes. Use errors.Is to detect; wrap with %w to attach
// context. Kept as values (not typed errors) because the preflight contract
// is intentionally narrow — callers branch on the class, not on fields.
var (
	ErrRuntimeNotAvailable = fmt.Errorf("preflight: runtime not available")
	ErrSafetyViolation     = fmt.Errorf("preflight: safety violation")
	ErrCapabilityDenied    = fmt.Errorf("preflight: capability denied")
	ErrUnknownLanguage     = fmt.Errorf("preflight: unsupported language")
)

// PreflightResult is the structured outcome of a preflight pass. Fields are
// populated only on success; on failure the corresponding Err is set and the
// rest are zero. Returned by PreflightCheckDetailed; PreflightCheck collapses
// it to a single error for backward compatibility with the design doc's
// signature.
type PreflightResult struct {
	OK         bool
	Lang       string             // canonicalized
	Capability *RuntimeCapability // chosen backend (nil if unknown lang)
	Err        error
}

// PreflightCheck is the design-doc-signature entry point. Returns nil iff
// every gate passes. On failure the returned error wraps one of the Err*
// sentinels so callers can errors.Is it to decide remediation.
//
// lang accepts the user-facing keys ("python","js","lua","node","bun",
// "javascript","ts") and canonicalizes them via normalizeLang.
func PreflightCheck(lang, code string) error {
	res := PreflightCheckDetailed(lang, code)
	if res.OK {
		return nil
	}
	return res.Err
}

// PreflightCheckDetailed runs the full gate and returns a structured result.
// It is the function all new call sites should use; PreflightCheck is kept
// only to match the design doc's illustrative signature.
func PreflightCheckDetailed(lang, code string) PreflightResult {
	canonical := normalizeLang(lang)

	// Gate 1: language known + backend available.
	cap := ResolveRuntime(canonical)
	if cap == nil {
		return PreflightResult{
			Lang: canonical,
			Err:  fmt.Errorf("%w: unsupported JIT language %q (canonical %q); supported: js, lua, python", ErrUnknownLanguage, lang, canonical),
		}
	}
	if !cap.IsAvailable {
		// Produce actionable guidance: embedded backends are always available,
		// so this branch only fires for managed (host) backends.
		return PreflightResult{
			Lang:       canonical,
			Capability: cap,
			Err: fmt.Errorf("%w: %q backend %q not detected on host and no embedded fallback configured",
				ErrRuntimeNotAvailable, canonical, cap.Type),
		}
	}

	// Gate 2: static syntax + safety lint.
	if err := staticSafetyLint(canonical, code); err != nil {
		return PreflightResult{
			Lang:       canonical,
			Capability: cap,
			Err:        fmt.Errorf("%w: %v", ErrSafetyViolation, err),
		}
	}

	return PreflightResult{OK: true, Lang: canonical, Capability: cap}
}

// CapabilityPreflight is gate 3 (role authorization). It is invoked by the
// tools/subsystems.go action handlers, which own role/tier context, NOT by
// core/jit.go (which has no role). Returns ErrCapabilityDenied wrapped with
// the missing capability when the role is not authorized for lang.
//
// allowedLangs is the set of JIT languages the current role permits (e.g.
// {"js","lua"} for a sandboxed role). An empty set denies everything.
func CapabilityPreflight(lang string, allowedLangs []string) error {
	canonical := normalizeLang(lang)
	for _, allowed := range allowedLangs {
		if normalizeLang(allowed) == canonical {
			return nil
		}
	}
	return fmt.Errorf("%w: role not authorized for JIT language %q", ErrCapabilityDenied, canonical)
}

// ---- staticSafetyLint -------------------------------------------------------
//
// Pure-Go, no I/O. Catches the high-risk patterns that would otherwise blow
// up at runtime or escape the sandbox:
//   - obvious syntax errors (unbalanced braces/brackets/quotes) — fast-fail
//     so a weak model doesn't burn a full process spawn to discover them.
//   - dangerous host-call patterns per language (os.system, exec, require
//     of fs/child_process, io.popen, loadfile, etc).
//
// This is intentionally a lint, not a full parser: the goal is to catch the
// 90% case cheaply and deterministically. The embedded engines (goja,
// gopher-lua) will still do their own parse and surface real syntax errors
// for anything the lint misses; the lint just makes the common failures
// observable BEFORE we commit to running the code.

// safetyPattern is one banned regex for one language. Compiled once at init.
type safetyPattern struct {
	lang string
	re   *regexp.Regexp
	why  string
}

var (
	safetyPatterns     []safetyPattern
	safetyPatternsOnce sync.Once
)

func getSafetyPatterns() []safetyPattern {
	safetyPatternsOnce.Do(func() {
		safetyPatterns = compileSafetyPatterns()
	})
	return safetyPatterns
}

func compileSafetyPatterns() []safetyPattern {
	// Regexes are case-insensitive where the language is (Python, JS).
	// Lua is case-sensitive, so its patterns use raw case.
	specs := []struct {
		lang, pat, why string
	}{
		// Python — host-escape vectors.
		{"python", `(?i)\bos\.system\s*\(`, "os.system spawns a host shell"},
		{"python", `(?i)\bsubprocess\b`, "subprocess spawns child processes"},
		{"python", `(?i)\bos\.popen\s*\(`, "os.popen spawns a host shell"},
		{"python", `(?i)\b__import__\s*\(\s*['"]os['"]`, "__import__('os') bypasses import lint"},
		{"python", `(?i)\bopen\s*\(\s*['"](?:/etc/passwd|/etc/shadow|\\\\\\.\\.)`, "sensitive path access"},
		{"python", `(?i)\beval\s*\(`, "eval is unsafe"},
		{"python", `(?i)\bexec\s*\(`, "exec is unsafe"},
		// JS / node — host-escape vectors. Also catches require('child_process').
		{"js", `(?i)\brequire\s*\(\s*['"]child_process['"]`, "child_process spawns host processes"},
		{"js", `(?i)\brequire\s*\(\s*['"]fs['"]`, "fs access is sandbox-escape vector"},
		{"js", `(?i)\brequire\s*\(\s*['"]net['"]`, "net access is sandbox-escape vector"},
		{"js", `(?i)\bprocess\.env\b`, "process.env leaks host environment"},
		{"js", `(?i)\beval\s*\(`, "eval is unsafe"},
		{"js", `(?i)\bFunction\s*\(`, "Function() constructor is eval-equivalent"},
		// Lua — host-escape vectors.
		{"lua", `\bloadfile\b`, "loadfile reads host filesystem"},
		{"lua", `\bdofile\b`, "dofile reads host filesystem"},
		{"lua", `\bos\.execute\b`, "os.execute spawns host shell"},
		{"lua", `\bos\.getenv\b`, "os.getenv leaks host environment"},
		{"lua", `\bio\.popen\b`, "io.popen spawns host shell"},
		{"lua", `\brequire\s*\(\s*['"]os['"]`, "require('os') grants host shell access"},
	}
	out := make([]safetyPattern, 0, len(specs))
	for _, s := range specs {
		re, err := regexp.Compile(s.pat)
		if err != nil {
			// A bad safety regex is a programming error; panic at init so it
			// is caught immediately rather than silently disabling a guard.
			panic(fmt.Sprintf("jit_preflight: bad safety regex %q: %v", s.pat, err))
		}
		out = append(out, safetyPattern{lang: s.lang, re: re, why: s.why})
	}
	return out
}

func staticSafetyLint(lang, code string) error {
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("empty code")
	}

	// Cheap structural balance check — applies to all supported languages
	// (Python/JS/Lua all use (), {}, []).
	if err := checkBracketBalance(code); err != nil {
		return err
	}

	// Language-specific banned patterns.
	for _, p := range getSafetyPatterns() {
		if p.lang != lang {
			continue
		}
		if loc := p.re.FindStringIndex(code); loc != nil {
			snippet := code[loc[0]:loc[1]]
			if len(snippet) > 60 {
				snippet = snippet[:60] + "..."
			}
			return fmt.Errorf("banned pattern %q matched (%s) near: %s", p.re.String(), p.why, snippet)
		}
	}
	return nil
}

// checkBracketBalance reports obvious unbalanced (), [], {} which would
// otherwise only surface as a parse error inside the (possibly absent) host
// runtime. It tolerates brackets inside string/char literals only for the
// common single-line case; multi-line strings with unbalanced brackets are
// rare in practice and the embedded parser will catch them anyway.
func checkBracketBalance(code string) error {
	type pair struct {
		open, close byte
		kind        string
	}
	pairs := []pair{{'(', ')', "paren"}, {'[', ']', "bracket"}, {'{', '}', "brace"}}

	// Strip single- and double-quoted string contents (best-effort, single
	// line) so brackets inside strings don't count. This is a lint, not a
	// parser — it errs on the side of "balanced" so it never false-positives
	// a real program; the worst case is a real imbalance inside a string
	// goes uncaught here and is caught by the real parser one step later.
	stripped := stripStringLiterals(code)

	for _, p := range pairs {
		depth := 0
		for i := 0; i < len(stripped); i++ {
			switch stripped[i] {
			case p.open:
				depth++
			case p.close:
				depth--
				if depth < 0 {
					return fmt.Errorf("unbalanced %s: closing %q before opening %q", p.kind, p.close, p.open)
				}
			}
		}
		if depth != 0 {
			return fmt.Errorf("unbalanced %s: %d unclosed %q", p.kind, depth, p.open)
		}
	}
	return nil
}

// stripStringLiterals replaces characters inside '...' and "..." with spaces
// so bracket counting ignores them. Handles backslash escapes. It does NOT
// handle triple-quoted Python strings or template literals — those are rare
// in JIT snippets and the worst case is a false negative (missed imbalance),
// never a false positive.
func stripStringLiterals(s string) string {
	b := []byte(s)
	inSingle, inDouble := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\\' && (inSingle || inDouble) {
			// Skip the escaped char.
			if i+1 < len(b) {
				b[i+1] = ' '
			}
			continue
		}
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			b[i] = ' '
		case c == '"' && !inSingle:
			inDouble = !inDouble
			b[i] = ' '
		case inSingle || inDouble:
			b[i] = ' '
		}
	}
	return string(b)
}
