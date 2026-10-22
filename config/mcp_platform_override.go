package config

// MCPPlatformOverride overrides an MCP's launch configuration for a specific
// platform (e.g. a different command/args on Windows). Split out of types.go to
// keep that file within its size budget.
type MCPPlatformOverride struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Dir     string            `json:"dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}
