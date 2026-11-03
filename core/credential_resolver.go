package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/oauthclient"
)

// Credential represents resolved auth credentials for a remote MCP request.
// It is the output of CredentialResolver.Resolve — a header to set on the
// HTTP request, not a raw token. This satisfies R6 (never expose raw token
// to callers; the Authorization header is redacted by RedactEgress).
type Credential struct {
	HeaderName  string // e.g., "Authorization"
	HeaderValue string // e.g., "Bearer <token>"
}

// Apply sets the credential header on an HTTP request. No-op if empty.
func (c Credential) Apply(req *http.Request) {
	if c.HeaderName != "" {
		req.Header.Set(c.HeaderName, c.HeaderValue)
	}
}

// CredentialResolver resolves auth credentials for a remote MCP per user
// (OAUTH_SHARED_MCP_MULTI_TENANT_DESIGN.md §4 L2).
//
// Resolve returns a Credential (header name + value) for the given MCP and
// owner. ownerID="" means operator-level (shared credential).
//
// Invalidate clears the cached credential for the given MCP and owner. Called
// on 401 to force a fresh token fetch on the next Resolve (R3).
type CredentialResolver interface {
	Resolve(ctx context.Context, mcp *config.MCPDef, ownerID string) (Credential, error)
	Invalidate(mcpID, ownerID string)
}

// oauthCredentialResolver implements CredentialResolver using TokenManager
// for OAuth 2.0 and env vars for static API keys. This is the default and
// only implementation, but the interface allows future sources (vault, etc.).
type oauthCredentialResolver struct {
	tm      *oauthclient.TokenManager // nil = OAuth not available
	configs sync.Map                  // mcpID → config.OAuthClientConfig (for Invalidate)
}

// NewOAuthCredentialResolver creates a CredentialResolver backed by the given
// TokenManager. If tm is nil, OAuth-based MCPs will fail with a clear error
// (API-key MCPs are unaffected).
func NewOAuthCredentialResolver(tm *oauthclient.TokenManager) CredentialResolver {
	return &oauthCredentialResolver{tm: tm}
}

func (r *oauthCredentialResolver) Resolve(ctx context.Context, mcp *config.MCPDef, ownerID string) (Credential, error) {
	// OAuth client takes precedence.
	if mcp.OAuthClient != nil {
		if r.tm == nil {
			return Credential{}, fmt.Errorf("mcp %q requires OAuth but token manager is not wired", mcp.ID)
		}
		// Record config for Invalidate (R3).
		r.configs.Store(mcp.ID, *mcp.OAuthClient)
		token, err := r.tm.GetToken(ctx, *mcp.OAuthClient, ownerID)
		if err != nil {
			return Credential{}, fmt.Errorf("mcp %q OAuth token error: %w", mcp.ID, err)
		}
		return Credential{HeaderName: "Authorization", HeaderValue: "Bearer " + token}, nil
	}

	// Fallback: static API key from env var.
	if mcp.AuthHeaderName == "" || mcp.APIKeyEnv == "" {
		return Credential{}, nil // no auth configured
	}
	secret := cachedGetenv(mcp.APIKeyEnv)
	if secret == "" {
		return Credential{}, fmt.Errorf("mcp %q requires env var %q for auth, but it is unset or empty", mcp.ID, mcp.APIKeyEnv)
	}
	return Credential{HeaderName: mcp.AuthHeaderName, HeaderValue: mcp.AuthHeaderPrefix + secret}, nil
}

func (r *oauthCredentialResolver) Invalidate(mcpID, ownerID string) {
	if r.tm == nil {
		return
	}
	if cfgVal, ok := r.configs.Load(mcpID); ok {
		r.tm.Invalidate(cfgVal.(config.OAuthClientConfig), ownerID)
	}
	// If no config is recorded (e.g., API-key-only MCP), there's nothing to
	// invalidate — the env var is static and will be re-read on next Resolve.
}
