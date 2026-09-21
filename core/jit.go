package core

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
	"github.com/google/uuid"
)

// JITManager handles the creation, registration, and lifecycle of temporary tools.
type JITManager struct {
	registry   *config.Registry
	dir        string
	promoted   map[string]bool // Track promoted tools to prevent cleanup
	promotedMu sync.RWMutex
	stopCh     chan struct{} // closed by Close() to cancel cleanup goroutines (audit M6)
	closeOnce  sync.Once
}

func NewJITManager(reg *config.Registry, workDir string) *JITManager {
	dir := filepath.Join(workDir, "jit_tools")
	os.MkdirAll(dir, 0755)

	if runtime.GOOS != "windows" {
		log.Printf("[SECURITY WARNING] JIT code isolation (Job Objects) is only supported on Windows. On %s, code runs with the same privileges as the host process. Use with caution.", runtime.GOOS)
	}

	return &JITManager{
		registry: reg,
		dir:      dir,
		promoted: make(map[string]bool),
		stopCh:   make(chan struct{}),
	}
}

// Close cancels all pending cleanup goroutines and prevents new ones from
// running. Idempotent. Call this when the JITManager is no longer needed
// (e.g., on scheduler shutdown) to avoid goroutine leaks (audit M6).
func (m *JITManager) Close() {
	m.closeOnce.Do(func() {
		close(m.stopCh)
	})
}

// EmbeddedCommandPrefix is the marker command prefix the spawner recognizes to
// dispatch an MCP to the in-process EmbeddedRunner instead of spawning a host
// binary. The full command is "__embedded__:<lang>" (e.g. "__embedded__:js").
// This keeps config.MCPDef unchanged — the script path lives in Args[0] just
// like the managed tier, so GetSource/PromotePermanent work uniformly.
const EmbeddedCommandPrefix = "__embedded__:"

// RegisterTool creates a temporary script file and registers it as a dynamic MCP.
// As of the embedded-runtime + preflight architecture
// (docs/architecture/EMBEDDED_JIT_AND_PREFLIGHT_DESIGN.md), this method:
//  1. Runs PreflightCheck first — bad code is rejected before any file I/O.
//  2. Routes embedded langs (js, lua) to RegisterEmbeddedTool, which registers
//     an in-process MCP marker instead of requiring a host binary.
//  3. Keeps the legacy host-binary path for managed langs (python, node, bun).
func (m *JITManager) RegisterTool(script, lang string, ttl time.Duration, sandboxed bool) (string, error) {
	if err := PreflightCheck(lang, script); err != nil {
		return "", err
	}

	canonical := normalizeLang(lang)
	if IsEmbeddedLang(canonical) {
		return m.RegisterEmbeddedTool(script, canonical, ttl, sandboxed)
	}
	return m.registerManagedTool(script, lang, ttl, sandboxed)
}

// RegisterEmbeddedTool writes the script to the JIT dir and registers a dynamic
// MCP whose Command is the embedded marker (e.g. "__embedded__:js"). The
// spawner recognizes the marker and dispatches to EmbeddedRunner at execute
// time, so no host binary is required. The script path is stored in Args[0],
// matching the managed-tier convention so GetSource/PromotePermanent work
// unchanged.
func (m *JITManager) RegisterEmbeddedTool(script, lang string, ttl time.Duration, sandboxed bool) (string, error) {
	id := "jit_" + uuid.New().String()[:8]
	ext := ".js"
	if lang == "lua" {
		ext = ".lua"
	}
	path := filepath.Join(m.dir, id+ext)
	if err := os.WriteFile(path, []byte(script), 0644); err != nil {
		return "", fmt.Errorf("failed to write JIT script: %w", err)
	}

	mcp := &config.MCPDef{
		ID:        id,
		Command:   EmbeddedCommandPrefix + lang,
		Args:      []string{path},
		Trusted:   false,
		Sandboxed: sandboxed,
		ExpiresAt: time.Now().Add(ttl),
	}
	m.registry.RegisterDynamicMCP(mcp)
	m.scheduleCleanup(id, path, ttl)
	return id, nil
}

// registerManagedTool is the legacy host-binary path (python/node/bun/lua
// via external interpreter). Split out of RegisterTool so the embedded
// routing stays clean. PreflightCheck has already passed by the time this is
// called, so we don't re-validate here.
func (m *JITManager) registerManagedTool(script, lang string, ttl time.Duration, sandboxed bool) (string, error) {
	id := "jit_" + uuid.New().String()[:8]
	ext := ""
	command := ""
	var args []string

	switch lang {
	case "python":
		ext = ".py"
		command = env.GetPythonCmd()
	case "node":
		ext = ".js"
		command = "node"
	case "bun":
		ext = ".ts"
		command = "bun"
		args = []string{"run"}
	case "lua":
		ext = ".lua"
		command = "lua" // host lua interpreter; embedded Lua uses RegisterEmbeddedTool
	default:
		return "", fmt.Errorf("unsupported managed JIT language: %s", lang)
	}

	filename := id + ext
	path := filepath.Join(m.dir, filename)
	if err := os.WriteFile(path, []byte(script), 0644); err != nil {
		return "", fmt.Errorf("failed to write JIT script: %w", err)
	}

	mcp := &config.MCPDef{
		ID:        id,
		Command:   command,
		Args:      append(args, path),
		Trusted:   false,
		Sandboxed: sandboxed,
		ExpiresAt: time.Now().Add(ttl),
	}
	m.registry.RegisterDynamicMCP(mcp)
	m.scheduleCleanup(id, path, ttl)
	return id, nil
}

// scheduleCleanup is the shared TTL goroutine for both embedded and managed
// JIT tools. Idempotent and safe to call once per registration. The goroutine
// exits early if Close() is called before the TTL expires (audit M6).
func (m *JITManager) scheduleCleanup(id, path string, ttl time.Duration) {
	go func() {
		timer := time.NewTimer(ttl)
		defer timer.Stop()
		select {
		case <-timer.C:
			// TTL expired — clean up if not promoted.
			m.promotedMu.RLock()
			isPromoted := m.promoted[id]
			m.promotedMu.RUnlock()

			if !isPromoted {
				m.registry.UnregisterDynamicMCP(id)
				os.Remove(path)
			}
		case <-m.stopCh:
			// JITManager closed — cancel cleanup, goroutine exits.
			return
		}
	}()
}

// GetSource retrieves the script content of a registered JIT tool.
func (m *JITManager) GetSource(id string) (string, error) {
	mcp := m.registry.GetMCP(id)
	if mcp == nil {
		return "", fmt.Errorf("tool %q not found or already expired", id)
	}

	if len(mcp.Args) == 0 {
		return "", fmt.Errorf("tool %q has no arguments (expected script path)", id)
	}

	// The last argument in JIT MCP is the script path
	path := mcp.Args[len(mcp.Args)-1]
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read JIT source from %s: %w", path, err)
	}

	return string(content), nil
}

// PromotePermanent moves a dynamic MCP to the permanent configuration.
func (m *JITManager) PromotePermanent(id string) error {
	m.promotedMu.Lock()
	m.promoted[id] = true
	m.promotedMu.Unlock()

	mcp := m.registry.GetMCP(id)
	if mcp == nil {
		return fmt.Errorf("tool %q not found or already expired", id)
	}

	data, err := json.Marshal(mcp)
	if err != nil {
		return fmt.Errorf("failed to serialize MCP for promotion: %w", err)
	}

	ops := []config.PatchOp{
		{Op: config.OpAdd, Path: "/mcps/" + id, Value: data},
	}

	return m.registry.CommitOps(ops, "jit_manager", "")
}

// ── Task-Local Closure ─────────────────────────────────────────────────────
//
// Implements §二.1 of docs/architecture/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md.
// At task submission time, SnapshotJITTools deep-copies any JIT MCP definitions
// referenced by mcpIDs into a map[json.RawMessage] the task graph carries. If a
// JIT tool's global TTL expires mid-task, RestoreLocalMCPs re-registers the
// definition from the snapshot so the step never hits "tool not found".

// SnapshotJITTools returns a JSON-serialized snapshot of every JIT tool in
// mcpIDs that currently exists in the registry's DynamicMCPs. Non-JIT IDs and
// already-expired IDs are silently skipped (they have no snapshot to take).
// The returned map is safe to store on a TaskGraph.
func (m *JITManager) SnapshotJITTools(mcpIDs []string) map[string]json.RawMessage {
	if len(mcpIDs) == 0 {
		return nil
	}
	m.registry.Mu.RLock()
	defer m.registry.Mu.RUnlock()

	snap := make(map[string]json.RawMessage)
	for _, id := range mcpIDs {
		mcp, ok := m.registry.DynamicMCPs[id]
		if !ok || mcp == nil {
			continue
		}
		data, err := json.Marshal(mcp)
		if err != nil {
			continue
		}
		snap[id] = data
	}
	if len(snap) == 0 {
		return nil
	}
	return snap
}

// RestoreLocalMCPs re-registers any JIT tools from the snapshot that are
// missing from the global DynamicMCPs. Tools still present globally are
// left untouched (the global copy is authoritative while it lives).
// Returns the IDs that were restored (empty if nothing was missing).
//
// This is the "closure" half of the design: even if the global GC reaped
// the tool between task submission and step execution, the task carries
// its own copy and self-heals.
func (m *JITManager) RestoreLocalMCPs(snapshot map[string]json.RawMessage) []string {
	if len(snapshot) == 0 {
		return nil
	}
	m.registry.Mu.Lock()
	defer m.registry.Mu.Unlock()

	var restored []string
	for id, data := range snapshot {
		if _, ok := m.registry.DynamicMCPs[id]; ok {
			continue // still alive globally — don't clobber
		}
		var mcp config.MCPDef
		if err := json.Unmarshal(data, &mcp); err != nil {
			continue
		}
		if len(mcp.Args) > 0 {
			scriptPath := mcp.Args[len(mcp.Args)-1]
			if _, err := os.Stat(scriptPath); err != nil {
				continue
			}
		}
		m.registry.DynamicMCPs[id] = &mcp
		restored = append(restored, id)
	}
	return restored
}
