package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ─── Integration Test: Tier-Gated Workspace Scope — Public Tier Injection ───
//
// Design ref: docs/completed/2026-10-21/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §8
// "Integration test: Public tier LLM injects workspace_root → rejected"
//
// This test verifies the entry-layer defense (I1 + I2): when a Public tier
// caller's LLM is tricked into providing a malicious workspace_root (e.g.
// "/etc/passwd" or another tenant's directory), ResolveWorkspaceRoot silently
// ignores it and derives the workspace from the session sandbox.

// mockClientSession implements server.ClientSession for testing.
type mockClientSession struct {
	sessionID string
}

func (m *mockClientSession) Initialize()                                         {}
func (m *mockClientSession) Initialized() bool                                   { return true }
func (m *mockClientSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return nil }
func (m *mockClientSession) SessionID() string                                   { return m.sessionID }

// TestIntegration_PublicTier_WorkspaceRootInjection_Rejected verifies that
// a Public tier caller cannot escalate via workspace_root. The LLM may be
// tricked by indirect prompt injection into providing a malicious path, but
// ResolveWorkspaceRoot forces the workspace to the session sandbox.
func TestIntegration_PublicTier_WorkspaceRootInjection_Rejected(t *testing.T) {
	srv := &server.MCPServer{}
	session := &mockClientSession{sessionID: "sess-123"}
	ctx := srv.WithContext(context.Background(), session)
	ctx = context.WithValue(ctx, AuthTierKey, TierPublic)
	ctx = context.WithValue(ctx, AuthOwnerKey, "tenant-public")

	// Malicious workspace_root that the LLM was tricked into providing.
	maliciousRoots := []string{
		"/etc/passwd",
		"/root/.ssh",
		"../../../etc",
		filepath.Join(t.TempDir(), "other-tenant", "secret-data"),
	}

	sandboxBase := filepath.Join(t.TempDir(), "sessions")
	policy := &WorkspacePolicy{
		OwnerID:            "tenant-public",
		SessionSandboxBase: sandboxBase,
	}

	expectedBase := filepath.Join(sandboxBase, "tenant-public", "sess-123")

	for _, malicious := range maliciousRoots {
		t.Run("injection="+malicious, func(t *testing.T) {
			resolved, err := ResolveWorkspaceRoot(ctx, policy, malicious)
			if err != nil {
				t.Fatalf("ResolveWorkspaceRoot failed: %v", err)
			}

			// The resolved root must NOT be the malicious path.
			if resolved == malicious {
				t.Errorf("Public tier workspace_root injection succeeded: resolved=%q == malicious=%q",
					resolved, malicious)
			}

			// The resolved root must be the session sandbox path.
			if resolved != expectedBase {
				t.Errorf("resolved = %q, want %q", resolved, expectedBase)
			}

			// Must be under the sandbox base.
			if !strings.HasPrefix(resolved, sandboxBase) {
				t.Errorf("resolved %q is not under sandbox base %q", resolved, sandboxBase)
			}
		})
	}
}

// TestIntegration_AdminTier_WorkspaceRoot_Honored verifies that an Admin tier
// caller's workspace_root is honored (admin = host user's credential, explicitly
// delegated trust). This is the positive counterpart to the Public tier test.
func TestIntegration_AdminTier_WorkspaceRoot_Honored(t *testing.T) {
	srv := &server.MCPServer{}
	session := &mockClientSession{sessionID: "sess-admin"}
	ctx := srv.WithContext(context.Background(), session)
	ctx = context.WithValue(ctx, AuthTierKey, TierAdmin)
	ctx = context.WithValue(ctx, AuthOwnerKey, "admin-user")

	requestedRoot := "/custom/admin/workspace"
	policy := &WorkspacePolicy{
		OwnerID:            "admin-user",
		SessionSandboxBase: "/sessions",
	}

	resolved, err := ResolveWorkspaceRoot(ctx, policy, requestedRoot)
	if err != nil {
		t.Fatalf("ResolveWorkspaceRoot failed: %v", err)
	}

	if resolved != requestedRoot {
		t.Errorf("Admin tier workspace_root should be honored: resolved=%q, want %q",
			resolved, requestedRoot)
	}
}

// TestIntegration_LocalMode_WorkspaceRoot_Honored verifies that local mode
// (no owner, no API key) trusts the LLM input for backward compatibility.
func TestIntegration_LocalMode_WorkspaceRoot_Honored(t *testing.T) {
	ctx := context.Background() // no tier, no owner

	requestedRoot := "/any/path"
	resolved, err := ResolveWorkspaceRoot(ctx, nil, requestedRoot)
	if err != nil {
		t.Fatalf("ResolveWorkspaceRoot failed: %v", err)
	}

	if resolved != requestedRoot {
		t.Errorf("Local mode workspace_root should be honored: resolved=%q, want %q",
			resolved, requestedRoot)
	}
}
