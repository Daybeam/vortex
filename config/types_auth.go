package config

import "os"

type NamedAPIKey struct {
	Key     string `json:"key"`
	OwnerID string `json:"owner_id"`
	Tier    string `json:"tier"` // "admin" | "public"
}

// AuthConfig controls the three-mode auth switch: none (dev/CI), apikey
// (existing tieredBearerAuth), oauth (OAuth 2.1 + PKCE + JWT).
// See docs/plan/OAUTH_AUTHORIZATION_SERVER_DESIGN.md.
type AuthConfig struct {
	Mode string `json:"auth_mode"` // "none" | "apikey" | "oauth"

	// APIKey mode (existing tieredBearerAuth)
	AdminKey  string        `json:"admin_key"`
	PublicKey string        `json:"public_key"`
	APIKeys   []NamedAPIKey `json:"api_keys"`

	// OAuth mode (OAuth 2.1 Authorization Server)
	OAuth OAuthConfig `json:"oauth"`
}

// OAuthConfig holds settings for the embedded OAuth 2.1 Authorization Server.
type OAuthConfig struct {
	Issuer         string `json:"issuer"`          // e.g. "https://orch.example.com"
	SigningKey     string `json:"signing_key"`     // HS256 symmetric signing key
	DCREnabled     bool   `json:"dcr_enabled"`     // DCR (RFC 7591) endpoint toggle
	FallbackAPIKey bool   `json:"fallback_apikey"` // OAuth fail → degrade to APIKey
}

// LoadAuthConfig builds AuthConfig from environment variables.
// Defaults to Mode="none" (backward-compatible: no auth, direct passthrough).
func LoadAuthConfig() AuthConfig {
	cfg := AuthConfig{
		Mode: getenv("VORTEX_AUTH_MODE", "none"),
	}

	// APIKey mode
	cfg.AdminKey = getenv("VORTEX_ADMIN_KEY", getenv("VORTEX_ADMIN_KEY", ""))
	cfg.PublicKey = getenv("VORTEX_PUBLIC_KEY", getenv("VORTEX_PUBLIC_KEY", ""))

	// OAuth mode
	cfg.OAuth = OAuthConfig{
		Issuer:         os.Getenv("VORTEX_OAUTH_ISSUER"),
		SigningKey:     os.Getenv("VORTEX_OAUTH_SIGNING_KEY"),
		DCREnabled:     os.Getenv("VORTEX_OAUTH_DCR_ENABLED") == "true",
		FallbackAPIKey: os.Getenv("VORTEX_OAUTH_FALLBACK_APIKEY") == "true",
	}

	return cfg
}

// getenv returns the value of the environment variable named by the key,
// or def if it is empty.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
