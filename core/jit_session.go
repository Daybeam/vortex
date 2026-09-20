package core

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
)

// JITSession/JITSessionManager: POC implementation of the session-state
// mechanism proposed in playbook/jit-session-poc-design-2026.md, itself a
// counter-proposal to playbook/local-refinement-architecture-2026.md's
// independent persistent REPL/kernel subsystem.
//
// SCOPE (this commit implements design doc §5 validation steps 1-2 only,
// deliberately NOT step 3 — see the design doc for the full plan):
//   1. A persistent Python interpreter process (scripts/jit/repl_server.py)
//      that keeps a single globals() dict alive across multiple Eval calls,
//      proving variables/imports genuinely persist within one session.
//   2. Sandbox + TTL cleanup reusing the exact same mechanism already
//      verified for one-shot JIT tools (client.ApplySandbox via
//      client.NewMCPClient's constraints parameter, F25).
// NOT implemented here: wiring CloseSessionsForStep into
// core/scheduler.go's executeStep (design doc §3.5/§5 step 3), and the Lua
// language path (design doc §3.3 flags a known limitation there: gopher-lua
// has no way to interrupt a running DoString call, so a per-eval timeout
// can't actually stop a runaway Lua eval the way it can for the Python
// subprocess path). session_id is caller-supplied for this POC rather than
// auto-derived from task/step context, since the Action handler signature
// tools/subsystems.go actions run under (ctx, app, args) doesn't carry
// taskID/stepID today -- see design doc §3.4's note on this simplification.
//
// Wire protocol: JSON-RPC-lite matching client.MCPClient.SendRequest/
// readLoop's existing framing exactly (request:
// {"jsonrpc":"2.0","method":...,"params":...,"id":...}, response:
// {"jsonrpc":"2.0","id":...,"result":...|"error":{...}}) so this reuses
// client.MCPClient/SendRequest directly instead of inventing a new client
// type or read/dispatch loop. Deliberately bypasses ensureMCPClient's
// forced MCP "initialize" handshake (repl_server.py doesn't speak that
// protocol) -- JITSessionManager calls client.NewMCPClient directly.

// EvalResult is the decoded "result" object repl_server.py returns for a
// successful "eval" call.
type EvalResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Result any    `json:"result"`
}

// JITSession wraps one long-lived, sandboxed REPL subprocess.
type JITSession struct {
	ID        string
	Lang      string // only "python" implemented in this POC
	CreatedAt time.Time
	ExpiresAt time.Time

	cli *client.MCPClient
	mu  sync.Mutex // serializes Eval calls within this session; see design doc §4
}

// Eval runs code against this session's persistent interpreter state and
// returns its stdout/stderr/result. Each call gets its own timeout via ctx
// (design doc §3.3: CPU limiting for a session is per-call, NOT the sandboxed
// process's cumulative Job Object CPU-time limit, which would exhaust after
// a handful of short calls and defeat the point of a long-lived session).
func (s *JITSession) Eval(ctx context.Context, code string) (*EvalResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.cli.SendRequest(ctx, "eval", map[string]any{"code": code})
	if err != nil {
		return nil, err
	}

	// raw comes back as the generic `any` SendRequest decodes json "result"
	// into (map[string]any here, since EvalResult isn't known to json at
	// that layer) -- re-marshal/unmarshal into the typed struct rather than
	// hand-rolling a type assertion per field.
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("jit_session: unexpected eval result shape: %#v", raw)
	}
	res := &EvalResult{}
	if v, ok := m["stdout"].(string); ok {
		res.Stdout = v
	}
	if v, ok := m["stderr"].(string); ok {
		res.Stderr = v
	}
	res.Result = m["result"]
	return res, nil
}

// Close terminates the session's subprocess and releases its sandbox.
func (s *JITSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cli != nil {
		s.cli.Close()
	}
}

// JITSessionManager owns the set of currently-live JIT sessions for one
// orchestrator process.
type JITSessionManager struct {
	registry   *config.Registry
	scriptPath string // absolute path to scripts/jit/repl_server.py

	mu       sync.RWMutex
	sessions map[string]*JITSession
}

// NewJITSessionManager constructs a manager. scriptsDir is the project's
// own scripts/ directory (same convention as JITManager's workDir param);
// the manager resolves scriptsDir/jit/repl_server.py from it.
func NewJITSessionManager(reg *config.Registry, scriptsDir string) *JITSessionManager {
	return &JITSessionManager{
		registry:   reg,
		scriptPath: filepath.Join(scriptsDir, "jit", "repl_server.py"),
		sessions:   make(map[string]*JITSession),
	}
}

// GetOrCreateSession returns the existing session for sessionID if it's
// still alive, or spawns a new one. lang is validated but only "python" is
// implemented in this POC (design doc §3.3's Lua limitation).
func (m *JITSessionManager) GetOrCreateSession(sessionID, lang string, ttl time.Duration) (*JITSession, error) {
	if lang != "python" {
		return nil, fmt.Errorf("jit_session: lang %q not implemented in this POC (only \"python\")", lang)
	}

	m.mu.Lock()
	if sess, ok := m.sessions[sessionID]; ok {
		if !sess.cli.IsDead() && time.Now().Before(sess.ExpiresAt) {
			m.mu.Unlock()
			return sess, nil
		}
		// Stale/dead: evict and fall through to respawn.
		sess.Close()
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	// Spawn subprocess WITHOUT holding the lock — Python startup can take
	// hundreds of milliseconds, and blocking all other session lookups
	// during that window is a latency multiplier under concurrent load.
	memMB := m.registry.System.SandboxedMemoryMB
	if memMB <= 0 {
		memMB = 256
	}
	cpuSecs := m.registry.System.SandboxedCPUSecs
	if cpuSecs <= 0 {
		cpuSecs = 30
	}
	constraints := &client.SandboxConstraints{
		MaxMemoryBytes: uint64(memMB) * 1024 * 1024,
		MaxCPUSeconds:  uint64(cpuSecs),
		KillOnClose:    true,
	}

	pythonCmd := env.GetPythonCmd()
	cli, err := client.NewMCPClient(pythonCmd, []string{m.scriptPath}, "", nil, constraints)
	if err != nil {
		return nil, fmt.Errorf("jit_session: spawn repl_server.py: %w", err)
	}

	sess := &JITSession{
		ID:        sessionID,
		Lang:      lang,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
		cli:       cli,
	}

	// Double-check: another goroutine may have created the same session
	// while we were spawning. If so, close our new cli and return the
	// existing one.
	m.mu.Lock()
	if existing, ok := m.sessions[sessionID]; ok {
		if !existing.cli.IsDead() && time.Now().Before(existing.ExpiresAt) {
			m.mu.Unlock()
			sess.Close()
			return existing, nil
		}
		existing.Close()
		delete(m.sessions, sessionID)
	}
	m.sessions[sessionID] = sess
	m.mu.Unlock()

	// TTL cleanup, mirroring JITManager.RegisterTool's own goroutine pattern.
	go func() {
		time.Sleep(ttl)
		m.CloseSession(sessionID)
	}()

	return sess, nil
}

// CloseSession closes and removes sessionID if it currently exists.
// Idempotent -- closing an already-closed or nonexistent session is a
// harmless no-op, so both the TTL goroutine and an explicit caller can
// race to call this without needing to coordinate.
func (m *JITSessionManager) CloseSession(sessionID string) {
	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if ok {
		sess.Close()
	}
}

// CloseAll closes every live JIT session subprocess. Call on shutdown to
// prevent orphaned REPL processes (audit finding C3).
func (m *JITSessionManager) CloseAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	sessions := make([]*JITSession, 0, len(m.sessions))
	for id, sess := range m.sessions {
		sessions = append(sessions, sess)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	for _, sess := range sessions {
		sess.Close()
	}
}

// SessionCount reports how many sessions are currently tracked (test/
// diagnostic helper).
func (m *JITSessionManager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// jitStepSessionID derives the session key used by the jit.eval action and
// by CloseSessionsForStep -- "one session per (task, step)" is this POC's
// convention (design doc §3.4's note: session_id is caller-supplied rather
// than pulled from request context, since the Action handler signature
// doesn't carry taskID/stepID; callers supply them as explicit args instead
// and this function makes the derivation consistent between creation and
// cleanup).
func jitStepSessionID(taskID, stepID string) string {
	return taskID + "::" + stepID
}

// CloseSessionsForStep closes whatever JIT session belongs to (taskID,
// stepID), if any. Safe to call even if no session was ever created for
// this step (no-op). Intended call sites: core/scheduler.go's executeStep,
// on both the success and failure paths (design doc §3.5) -- a session
// must never outlive the step that created it.
func (m *JITSessionManager) CloseSessionsForStep(taskID, stepID string) {
	m.CloseSession(jitStepSessionID(taskID, stepID))
}

// GetOrCreateSessionForStep is the (taskID, stepID)-keyed convenience
// wrapper around GetOrCreateSession, used by tools/subsystems.go's jit.eval
// action so the "::" key-derivation convention lives in exactly one place
// (here) rather than being duplicated across packages.
func (m *JITSessionManager) GetOrCreateSessionForStep(taskID, stepID, lang string, ttl time.Duration) (*JITSession, error) {
	return m.GetOrCreateSession(jitStepSessionID(taskID, stepID), lang, ttl)
}
