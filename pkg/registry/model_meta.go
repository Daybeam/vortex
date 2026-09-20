package registry

// ModelCapabilities defines the unified capability schema for model routing
// and prompt budgeting. See MODEL_REGISTRY_PLUGGABLE_ARCHITECTURE.md §3.1.
type ModelCapabilities struct {
	ModelName        string  `json:"model_name"`
	MaxContextWindow int     `json:"max_context_window"`
	MaxOutputTokens  int     `json:"max_output_tokens"`
	InputPricePerM   float64 `json:"input_price_per_m"`
	OutputPricePerM  float64 `json:"output_price_per_m"`
	SupportsToolUse  bool    `json:"supports_tool_use"`
	SupportsJSONMode bool    `json:"supports_json_mode"`
	TierOverride     string  `json:"tier_override,omitempty"`
}
