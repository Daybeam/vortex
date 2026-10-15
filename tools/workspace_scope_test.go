package tools

import (
	"context"
	"testing"
)

// ─── Workspace Scope Unit Tests ───────────────────────────────────────────

func TestResolveWorkspaceRoot_LocalNoOwner(t *testing.T) {
	// Local deployment: no API key, no owner → trust LLM input.
	ctx := context.Background() // no AuthOwnerKey set → owner=""
	policy := &WorkspacePolicy{SessionSandboxBase: "/tmp/sessions"}

	got, err := ResolveWorkspaceRoot(ctx, policy, "/custom/workspace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/custom/workspace" {
		t.Errorf("got %q, want /custom/workspace (local trusts LLM)", got)
	}
}

func TestResolveWorkspaceRoot_AdminTier(t *testing.T) {
	// Admin tier: explicitly delegated trust → trust LLM input.
	ctx := context.WithValue(context.Background(), AuthOwnerKey, "admin-user")
	ctx = context.WithValue(ctx, AuthTierKey, TierAdmin)
	policy := &WorkspacePolicy{SessionSandboxBase: "/tmp/sessions"}

	got, err := ResolveWorkspaceRoot(ctx, policy, "/any/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/any/path" {
		t.Errorf("got %q, want /any/path (admin trusts LLM)", got)
	}
}

func TestResolveWorkspaceRoot_PublicTier_SilentlyIgnoresRequested(t *testing.T) {
	// Public tier: workspace_root from LLM is silently ignored.
	// We can't easily set a real MCP session in a unit test, so we expect
	// the "requires session context" error — the key assertion is that
	// the requested path is NOT returned.
	ctx := context.WithValue(context.Background(), AuthOwnerKey, "tenant-a")
	ctx = context.WithValue(ctx, AuthTierKey, TierPublic)
	policy := &WorkspacePolicy{SessionSandboxBase: "/tmp/sessions"}

	got, _ := ResolveWorkspaceRoot(ctx, policy, "/etc/passwd")
	// The requested path must NOT be returned for public tier.
	if got == "/etc/passwd" {
		t.Error("public tier returned LLM-requested path — security violation")
	}
}

func TestResolveWorkspaceRoot_PublicTier_NoSandboxBase(t *testing.T) {
	ctx := context.WithValue(context.Background(), AuthOwnerKey, "tenant-a")
	ctx = context.WithValue(ctx, AuthTierKey, TierPublic)

	_, err := ResolveWorkspaceRoot(ctx, nil, "/some/path")
	if err == nil {
		t.Error("expected error for nil policy with public tier")
	}
}

func TestResolveWorkspaceRoot_PublicTier_EmptySandboxBase(t *testing.T) {
	ctx := context.WithValue(context.Background(), AuthOwnerKey, "tenant-a")
	ctx = context.WithValue(ctx, AuthTierKey, TierPublic)
	policy := &WorkspacePolicy{SessionSandboxBase: ""}

	_, err := ResolveWorkspaceRoot(ctx, policy, "/some/path")
	if err == nil {
		t.Error("expected error for empty sandbox base with public tier")
	}
}

func TestIsWithinAllowed(t *testing.T) {
	allowed := []string{"/tmp/sessions", "/var/data"}

	tests := []struct {
		path    string
		want    bool
	}{
		{"/tmp/sessions", true},           // exact match
		{"/tmp/sessions/tenant-a", true},  // nested
		{"/tmp/sessions/tenant-a/sub", true}, // deeply nested
		{"/var/data", true},               // exact match
		{"/var/data/file.txt", true},      // nested
		{"/etc/passwd", false},            // outside
		{"/tmp/session", false},           // prefix but not separator (would be /tmp/sessions)
		{"/tmp/sessions2", false},         // sibling, not nested
	}

	for _, tt := range tests {
		got := isWithinAllowed(tt.path, allowed)
		if got != tt.want {
			t.Errorf("isWithinAllowed(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestResolveWorkspaceRoot_LocalEmptyPolicy(t *testing.T) {
	// Local with nil policy → still trusts LLM (backward compat).
	ctx := context.Background()
	got, err := ResolveWorkspaceRoot(ctx, nil, "/my/workspace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/my/workspace" {
		t.Errorf("got %q, want /my/workspace", got)
	}
}

func TestResolveWorkspaceRoot_AdminEmptyPolicy(t *testing.T) {
	// Admin with nil policy → still trusts LLM.
	ctx := context.WithValue(context.Background(), AuthOwnerKey, "admin")
	ctx = context.WithValue(ctx, AuthTierKey, TierAdmin)
	got, err := ResolveWorkspaceRoot(ctx, nil, "/my/workspace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/my/workspace" {
		t.Errorf("got %q, want /my/workspace", got)
	}
}
