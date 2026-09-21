package schemas

// ToolCall represents a single tool call from a provider.
type ToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	CallID    string         `json:"call_id"`
}

// SecretProvider is the interface for resolving API keys and other secrets.
type SecretProvider interface {
	GetSecret(key string) (string, error)
}

// CompleteRequest defines the parameters for a completion request.
// The Block-based fields split content into Protected (zero-compression,
// verbatim — e.g. SOP rules, system prompt, artifact contracts) and
// Volatile (compressible — e.g. tool outputs, raw logs, upstream history).
// This is the architectural fix for the "Compaction Cliff" (arXiv:2608.22752):
// without it, Audit+Squeeze apply dedup/pruning uniformly to System and
// User, quietly eroding the rules layer when long task logs push the
// context window past its budget.
type CompleteRequest struct {
	System string
	User   string

	// Legacy block form (Volatile only). Still supported for backward compat.
	SystemBlocks []ContentBlock
	UserBlocks   []ContentBlock

	// Protected Zones (Anti-Cliff, ADDED 2026-08-27). These blocks are NEVER
	// compressed by ContextManager.Squeeze at any level (Light/Aggressive/
	// Critical); they survive verbatim. Only Volatile content is compressed.
	ProtectedSystemBlocks []ContentBlock
	ProtectedUserBlocks   []ContentBlock
	VolatileSystemBlocks  []ContentBlock
	VolatileUserBlocks    []ContentBlock

	Model            string
	MaxTokens        int
	Temperature      *float32
	FrequencyPenalty *float32
	MCPServers       []MCPServerDef
	Attachments      []Attachment
	Secrets          SecretProvider
	ForceToolCall    bool
	Constraints      map[string]any // Field Name -> Value (e.g., "grammar" -> "...")
	CompressionHint  string
}

// ContentBlock represents a segment of content with optional caching instructions.
type ContentBlock struct {
	Type         string         `json:"type"` // "text", "image" (future)
	Text         string         `json:"text,omitempty"`
	CacheControl string         `json:"cache_control,omitempty"` // e.g. "ephemeral" for Anthropic
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// ToolDefinition represents a single tool's schema.
type ToolDefinition struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`

	// LlamaHub-inspired metadata for standardized SAV loops
	VerificationMethod []string `json:"verification_method,omitempty"` // e.g. ["read_file", "ls"]
	RiskLevel          string   `json:"risk_level,omitempty"`          // e.g. "low", "medium", "high"
}

// MCPServerDef defines an MCP server to be used by the provider.
type MCPServerDef struct {
	Name         string           `json:"name"`
	URL          string           `json:"url"`
	AllowedTools []string         `json:"allowed_tools,omitempty"`
	Tools        []ToolDefinition `json:"tools,omitempty"` // Full schemas for the model
}

// ProviderResponse is the structured response from an AI provider.
type ProviderResponse struct {
	Text             string
	ToolCalls        []ToolCall
	StopReason       string
	PromptTokens     int
	CompletionTokens int
	CacheWriteTokens int // Tokens written to cache this turn
	CacheReadTokens  int // Tokens read from cache this turn
}
