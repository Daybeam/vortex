package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/server"
)

// ─── Tier-Gated Workspace Scope ───────────────────────────────────────────
//
// Design ref: docs/completed/2026-10-21/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md
//
// Defends against the Stochastic Deputy Problem (arXiv:2609.14780): when
// tool parameters (workspace_root) are LLM-sampled, indirect prompt injection
// can trick the LLM into accessing other tenants' directories.
//
// Policy matrix:
//   - Local (no API key, owner="")  → trust LLM (backward compat)
//   - Admin tier                    → trust LLM (explicitly delegated)
//   - Public tier (multi-tenant)    → force session sandbox, ignore LLM input

// WorkspacePolicy describes the allowed workspace_root set for an owner.
type WorkspacePolicy struct {
	OwnerID            string   // the owner this policy applies to
	AllowedRoots       []string // whitelist of absolute paths
	SessionSandboxBase string   // e.g. "outputs/sessions"; Public tier forced to use
}

// sessionIDFromContext extracts the MCP session ID from the request context.
// Returns "" if no session is bound (e.g. stdio transport).
func sessionIDFromContext(ctx context.Context) string {
	if cs := server.ClientSessionFromContext(ctx); cs != nil {
		return cs.SessionID()
	}
	return ""
}

// ResolveWorkspaceRoot applies the tier-gated strategy and returns the
// effective workspace_root.
//
// Contract: Public tier callers cannot escalate via LLM parameters.
//   - Local (owner=="")  → requested (backward compat)
//   - Admin tier         → requested (explicitly delegated trust)
//   - Public tier        → derived from session sandbox; requested is silently ignored
func ResolveWorkspaceRoot(ctx context.Context, policy *WorkspacePolicy, requested string) (string, error) {
	owner := CallerOwnerID(ctx)

	// Case 1: No owner (local single-machine deployment) → trust LLM input.
	if owner == "" {
		return requested, nil
	}

	// Case 2: Admin tier → trust LLM input (admin = host user's credential).
	tier, _ := ctx.Value(AuthTierKey).(string)
	if tier == TierAdmin {
		return requested, nil
	}

	// Case 3: Public tier (multi-tenant) → force session sandbox.
	// Per §3.4: silently ignore requested workspace_root (fail-soft, not error).
	if policy == nil || policy.SessionSandboxBase == "" {
		return "", fmt.Errorf("no sandbox base configured for public tier")
	}

	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return "", fmt.Errorf("public tier requires session context")
	}

	derived := filepath.Join(policy.SessionSandboxBase, owner, sessionID)

	// Whitelist check (if configured).
	if len(policy.AllowedRoots) > 0 {
		if !isWithinAllowed(derived, policy.AllowedRoots) {
			return "", fmt.Errorf("workspace %s not in allowed whitelist", derived)
		}
	}

	return derived, nil
}

// isWithinAllowed checks if path is equal to or nested under one of the
// allowed roots. Uses filepath.Clean to normalize before comparison.
func isWithinAllowed(path string, allowed []string) bool {
	cleanPath := filepath.Clean(path)
	for _, root := range allowed {
		cleanRoot := filepath.Clean(root)
		if cleanPath == cleanRoot ||
			strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
