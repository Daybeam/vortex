package config

// AntiSlopConfig controls the Anti-Slop quality interceptor.
type AntiSlopConfig struct {
	Enabled bool     `json:"enabled"`
	Strict  bool     `json:"strict"`
	MinHits int      `json:"min_hits"`
	Phrases []string `json:"phrases,omitempty"`
}
