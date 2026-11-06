package core

import (
	"context"
	"net/http"

	"github.com/daybeam/vortex/config"
)

// credentialResolver is the package-level credential resolver for outbound
// MCP connections. Set once at startup via SetCredentialResolver. nil means
// no auth is configured (applyMCPAuth is a no-op).
var credentialResolver CredentialResolver

// SetCredentialResolver wires the credential resolver for outbound MCP auth.
// Called once at startup from main_shared.go. Passing nil disables auth.
func SetCredentialResolver(r CredentialResolver) {
	credentialResolver = r
}

// applyMCPAuth sets the auth header on an HTTP request for a remote MCP.
//
// Delegates to the package-level CredentialResolver (OAUTH_SHARED_MCP_MULTI_
// TENANT_DESIGN.md §4 L2). The resolver handles both OAuth 2.0 (via
// TokenManager) and static API keys (via env vars), keyed per ownerID.
//
// ownerID="" means operator-level (shared credential, backward-compatible).
// Returns nil if no auth is configured. Returns an error if auth is required
// but the credential cannot be obtained.
func applyMCPAuth(ctx context.Context, req *http.Request, mcp *config.MCPDef, ownerID string) error {
	if credentialResolver == nil {
		return nil // no resolver wired — no auth (backward-compatible)
	}
	cred, err := credentialResolver.Resolve(ctx, mcp, ownerID)
	if err != nil {
		return err
	}
	cred.Apply(req)
	return nil
}

// InvalidateMCPCredential clears the cached credential for a specific MCP
// and owner. Called on 401 (R3) to force a fresh token fetch on the next
// request. No-op if no resolver is wired.
func InvalidateMCPCredential(mcpID, ownerID string) {
	if credentialResolver != nil {
		credentialResolver.Invalidate(mcpID, ownerID)
	}
}
