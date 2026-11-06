package main

import (
	"database/sql"
	"log"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/oauthclient"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/store"
)

// syncGlobalFallback pushes the config's global fallback provider chain to the
// provider factory. Called at startup and on config hot-reload. When enabled,
// any provider without its own FallbackChain will fall back to this chain.
func syncGlobalFallback(reg *config.Registry) {
	reg.Mu.RLock()
	chain := reg.GlobalFallback
	reg.Mu.RUnlock()
	providers.SetGlobalFallback(chain)
	if len(chain) > 0 {
		names := make([]string, len(chain))
		for i, c := range chain {
			names[i] = c.Provider
		}
		log.Printf("[global-fallback] enabled: %s", strings.Join(names, " → "))
	}
}

// oauthTM is the process-level OAuth token manager for outbound MCP connections.
// Created once at startup; invalidated on config hot-reload.
var oauthTM *oauthclient.TokenManager

// initOAuthTokenManager creates the outbound OAuth token manager and wires it
// into the core package. Called once at startup. Idempotent — safe to call
// multiple times (subsequent calls are no-ops).
func initOAuthTokenManager() {
	if oauthTM != nil {
		return
	}
	oauthTM = oauthclient.NewTokenManager()
	core.SetCredentialResolver(core.NewOAuthCredentialResolver(oauthTM))
}

// invalidateOAuthTokens clears all cached OAuth tokens. Called on config
// hot-reload to force re-fetch with potentially updated credentials.
func invalidateOAuthTokens() {
	if oauthTM != nil {
		oauthTM.InvalidateAll()
	}
}

// setOAuthTokenStore attaches a DB-backed token store to the OAuth token
// manager. Called after InitDB in each entry point. If db is nil or the token
// manager hasn't been initialized yet, this is a no-op.
func setOAuthTokenStore(db *sql.DB) {
	if db == nil || oauthTM == nil {
		return
	}
	oauthTM.SetStore(store.NewOAuthTokenStore(db))
}
