package config

// OAuthClientConfig defines an outbound OAuth 2.0 client connection for an MCP
// that requires OAuth authentication (e.g., Google APIs, GitHub, Microsoft).
//
// This is the "orchestrator → external service" direction, distinct from the
// inbound OAuth 2.1 Authorization Server in pkg/oauth/server.go which handles
// "external client → orchestrator".
//
// Supported grant types:
//   - "client_credentials" (default): server-to-server, no user interaction.
//     The orchestrator exchanges client_id + client_secret for an access token.
//   - "refresh_token": personal access. The user obtains a refresh token
//     out-of-band (e.g., via a CLI login command or manual OAuth flow) and
//     stores it in the env var named by RefreshTokenEnv. The orchestrator
//     uses it to get access tokens and refreshes them when they expire.
//
// Tokens are cached with TTL and refreshed automatically. See pkg/oauth/client.go.
type OAuthClientConfig struct {
	ClientID      string   `json:"client_id"`
	ClientSecret  string   `json:"client_secret"`
	TokenEndpoint string   `json:"token_endpoint"` // e.g. "https://oauth2.googleapis.com/token"
	Scopes        []string `json:"scopes,omitempty"`

	// GrantType: "client_credentials" (default) or "refresh_token".
	GrantType string `json:"grant_type,omitempty"`

	// RefreshTokenEnv names the env var holding the initial refresh token.
	// Required for grant_type="refresh_token". The token is obtained
	// out-of-band (CLI login, manual flow, etc.).
	RefreshTokenEnv string `json:"refresh_token_env,omitempty"`
}
