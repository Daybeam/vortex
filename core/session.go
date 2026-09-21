package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionContext binds a session ID to an authorized workspace root directory.
// All tasks within the same session inherit this binding, so the user/adapter
// only authorizes once. See
// docs/completed/2026-09-19/WORKSPACE_AND_SANDBOX_REDESIGN.md §2.2 ②.
type SessionContext struct {
	SessionID string
	Workspace string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// IsExpired returns true if the session has a TTL and it has passed.
func (sc *SessionContext) IsExpired() bool {
	if sc.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(sc.ExpiresAt)
}

// SessionManager tracks active session-to-workspace bindings. It is the
// runtime counterpart to config.SandboxConfig.AllowedWorkspaces (the global
// whitelist). SessionManager is safe for concurrent use.
type SessionManager struct {
	mu         sync.RWMutex
	sessions   map[string]*SessionContext
	allowed    []string
	defaultTTL time.Duration
}

// NewSessionManager creates a manager with the given global whitelist.
// When allowed is empty, any workspace_root is accepted (permissive mode).
// defaultTTL of 0 means sessions never expire.
func NewSessionManager(allowed []string, defaultTTL time.Duration) *SessionManager {
	return &SessionManager{
		sessions:   make(map[string]*SessionContext),
		allowed:    allowed,
		defaultTTL: defaultTTL,
	}
}

// CreateOrBind validates workspaceRoot against the global whitelist and
// creates (or refreshes) a SessionContext for the given sessionID.
// If the session already exists and is not expired, the existing binding
// is returned unchanged (idempotent).
//
// Returns an error if workspaceRoot is not in the allowed list and the
// list is non-empty (i.e., restricted mode is active).
func (m *SessionManager) CreateOrBind(sessionID, workspaceRoot string) (*SessionContext, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid workspace_root %q: %w", workspaceRoot, err)
	}

	if !m.isAllowed(absRoot) {
		return nil, fmt.Errorf("workspace_root %q is not in allowed_workspaces whitelist", absRoot)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.sessions[sessionID]; ok && !existing.IsExpired() {
		return existing, nil
	}

	sc := &SessionContext{
		SessionID: sessionID,
		Workspace: absRoot,
		CreatedAt: time.Now(),
	}
	if m.defaultTTL > 0 {
		sc.ExpiresAt = sc.CreatedAt.Add(m.defaultTTL)
	}
	m.sessions[sessionID] = sc
	return sc, nil
}

// Lookup retrieves the SessionContext for the given sessionID.
// Returns nil if the session does not exist or has expired.
func (m *SessionManager) Lookup(sessionID string) *SessionContext {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sc, ok := m.sessions[sessionID]
	if !ok || sc.IsExpired() {
		return nil
	}
	return sc
}

// isAllowed checks whether a workspace path is in the global whitelist.
// An empty whitelist means permissive mode (everything allowed).
func (m *SessionManager) isAllowed(absPath string) bool {
	if len(m.allowed) == 0 {
		return true
	}
	for _, root := range m.allowed {
		absRoot, _ := filepath.Abs(root)
		if strings.HasPrefix(absPath, absRoot) {
			return true
		}
	}
	return false
}
