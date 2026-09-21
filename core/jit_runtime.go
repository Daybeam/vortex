package core

import (
	"os/exec"
	"strings"
	"sync"

	"github.com/daybeam/vortex/pkg/env"
)

// jit_runtime.go — Embedded JIT runtime declaration contract.
// Implements §三.1 of docs/architecture/EMBEDDED_JIT_AND_PREFLIGHT_DESIGN.md.
//
// Two execution tiers are declared here:
//   - Embedded Tier  (IsEmbedded=true):  pure-Go engines compiled into the
//     binary (goja for JS, gopher-lua for Lua). Zero host dependency.
//   - Managed Tier  (IsEmbedded=false):  external host binaries probed via
//     exec.LookPath (python/node/bun/lua). Optional escape hatch.
//
// RuntimeCapability snapshots are cheap, read-only, and safe to hand to
// callers (e.g. a jit.list_runtimes action) since each field is a value type
// or a copy-constructed slice.

// JITRuntimeType identifies the concrete engine backing a language.
type JITRuntimeType string

const (
	RuntimeGojaJS    JITRuntimeType = "goja_js"         // pure-Go ECMAScript 5.1 via dop251/goja
	RuntimeGopherLua JITRuntimeType = "gopher_lua"      // pure-Go Lua 5.1 via yuin/gopher-lua
	RuntimePythonExt JITRuntimeType = "python_external" // host python3/python
	RuntimeNodeExt   JITRuntimeType = "node_external"   // host node
	RuntimeBunExt    JITRuntimeType = "bun_external"    // host bun
)

// RuntimeCapability is the serializable declaration of one language backend.
type RuntimeCapability struct {
	Lang        string         `json:"lang"`         // canonical language key: js|lua|python|node|bun
	Type        JITRuntimeType `json:"type"`         // concrete engine
	IsEmbedded  bool           `json:"is_embedded"`  // true => compiled into binary
	IsAvailable bool           `json:"is_available"` // true => ready to execute right now
	Note        string         `json:"note,omitempty"`
}

// runtimeRegistry is the singleton catalog of declared backends. It is
// immutable after init(); the per-call IsAvailable flag is computed fresh
// (host probes are cached inside pkg/env via sync.Once, so repeated calls
// are cheap).
var (
	runtimeRegistry     []RuntimeCapability
	runtimeRegistryOnce sync.Once
)

func buildRuntimeRegistry() []RuntimeCapability {
	// Embedded tier is always available — it's compiled in.
	caps := []RuntimeCapability{
		{Lang: "js", Type: RuntimeGojaJS, IsEmbedded: true, IsAvailable: true, Note: "goja ECMAScript 5.1 (pure Go)"},
		{Lang: "lua", Type: RuntimeGopherLua, IsEmbedded: true, IsAvailable: true, Note: "gopher-lua 5.1 (pure Go)"},
	}
	// Managed tier — probe host binaries.
	caps = append(caps,
		RuntimeCapability{Lang: "python", Type: RuntimePythonExt, IsEmbedded: false, IsAvailable: isHostBinaryAvailable(env.GetPythonCmd()), Note: "host python interpreter"},
		RuntimeCapability{Lang: "node", Type: RuntimeNodeExt, IsEmbedded: false, IsAvailable: isHostBinaryAvailable("node"), Note: "host node runtime"},
		RuntimeCapability{Lang: "bun", Type: RuntimeBunExt, IsEmbedded: false, IsAvailable: isHostBinaryAvailable("bun"), Note: "host bun runtime"},
	)
	return caps
}

// isHostBinaryAvailable reports whether name resolves on PATH. Wrapped so
// tests can stub it without touching exec.LookPath globally.
var isHostBinaryAvailable = func(name string) bool {
	if name == "" {
		return false
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// ListRuntimeCapabilities returns the full catalog. The embedded entries are
// always present and available; managed entries reflect the current host.
func ListRuntimeCapabilities() []RuntimeCapability {
	runtimeRegistryOnce.Do(func() {
		runtimeRegistry = buildRuntimeRegistry()
	})
	// Return a defensive copy — callers may iterate concurrently.
	out := make([]RuntimeCapability, len(runtimeRegistry))
	copy(out, runtimeRegistry)
	// Refresh IsAvailable for managed entries on each call (cheap; env caches).
	for i := range out {
		if out[i].IsEmbedded {
			continue
		}
		switch out[i].Lang {
		case "python":
			out[i].IsAvailable = isHostBinaryAvailable(env.GetPythonCmd())
		case "node":
			out[i].IsAvailable = isHostBinaryAvailable("node")
		case "bun":
			out[i].IsAvailable = isHostBinaryAvailable("bun")
		}
	}
	return out
}

// ResolveRuntime returns the best available backend for lang, preferring the
// embedded tier (zero-dependency) when one exists. Returns nil if no backend
// is declared for lang at all; callers distinguish "unknown language" from
// "known but unavailable" by checking IsAvailable on the returned cap.
func ResolveRuntime(lang string) *RuntimeCapability {
	for _, c := range ListRuntimeCapabilities() {
		if c.Lang == lang {
			c := c
			return &c
		}
	}
	return nil
}

// IsEmbeddedLang reports whether lang has a pure-Go embedded backend.
func IsEmbeddedLang(lang string) bool {
	if c := ResolveRuntime(lang); c != nil {
		return c.IsEmbedded
	}
	return false
}

// normalizeLang maps user-facing aliases to canonical keys. Accepts "javascript"
// and "ts"/"typescript" as JS (goja is ES5.1; TS is transpiled by caller if
// needed) so the preflight gate's error messages stay consistent with the
// existing jit.create action's "language" parameter.
func normalizeLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "js", "javascript", "ts", "typescript", "node", "bun":
		// "node"/"bun" historically meant "run JS via host runtime". With the
		// embedded tier now present, we normalize them to "js" so the embedded
		// goja backend can serve them when the host binary is absent. Callers
		// that explicitly want the host runtime can pass "python" (no embedded
		// equivalent) or check RuntimeCapability.Type.
		return "js"
	case "lua":
		return "lua"
	case "python", "py":
		return "python"
	default:
		return strings.ToLower(strings.TrimSpace(lang))
	}
}
