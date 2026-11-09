package acp

// -----------------------------------------------------------------------------
// Standard ACP (Zed Agent Client Protocol) wire contract — RESERVED / INERT.
//
// Plan-first reservation (2026-11-09): these are pure value types + one
// interface. NOTHING in this file is imported or dispatched yet — ServeACP in
// acp.go still speaks only the legacy DSH dialect (initialize/ping/run_task/
// cancel). main*.go is not wired to a standard handler. Default runtime
// behavior is byte-for-byte unchanged.
//
// Purpose: pin the standard-ACP wire shapes now so the future additive
// implementation does not have to renegotiate field names/signatures. The
// design, dialect negotiation, session↔task mapping, obs→session/update
// streaming, phased rollout and regression plan live in
// docs/plan/STANDARD_ACP_SERVER_SUPPORT_DESIGN.md.
//
// Spec: https://agentclientprotocol.com (ACP v1, JSON-RPC 2.0 over stdio).
// Methods: initialize, session/new, session/prompt (requests);
//          session/update, session/cancel (notifications).
// -----------------------------------------------------------------------------

// StandardProtocolVersion is the ACP protocol revision this reservation targets
// (ACP v1). initialize negotiation replies with min(client, agent) version.
const StandardProtocolVersion = 1

// StopReason is the terminal reason returned by session/prompt.
type StopReason string

const (
	StopReasonEndTurn     StopReason = "end_turn"
	StopReasonMaxTokens   StopReason = "out_of_tokens"
	StopReasonCancelled   StopReason = "cancelled"
	StopReasonRefusal     StopReason = "refusal"
	StopReasonMaxTurnReqs StopReason = "max_turn_requests"
)

// StandardInitializeParams is the client→agent initialize request. The presence
// of ProtocolVersion is the dialect discriminator: a params object carrying it
// is standard ACP; the legacy DSH client sends {client, version} instead.
type StandardInitializeParams struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities,omitempty"`
}

// StandardInitializeResult is the agent→client initialize response.
type StandardInitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []any             `json:"authMethods,omitempty"`
}

// AgentCapabilities advertises what the agent supports. MVP: text-only prompts,
// no session load, no auth.
type AgentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities PromptCapabilities `json:"promptCapabilities"`
}

type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// NewSessionParams creates a session. Cwd/McpServers are accepted but not yet
// honored (see "what this doesn't solve" in the design doc).
type NewSessionParams struct {
	Cwd        string  `json:"cwd"`
	McpServers []any   `json:"mcpServers"`
}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// ContentBlock is one element of a session/prompt prompt array. MVP handles
// Type=="text" only; other block types are a documented non-goal.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type PromptResult struct {
	StopReason StopReason `json:"stopReason"`
}

// SessionUpdate is the server→client streaming notification body. SessionUpdate
// is the discriminator: "agent_message_chunk", "tool_call", "tool_call_update",
// "plan". Only the fields common to the MVP shapes are reserved here; the full
// per-discriminator union is fleshed out at implementation time.
type SessionUpdate struct {
	SessionUpdate string        `json:"sessionUpdate"`
	Content       *ContentBlock `json:"content,omitempty"`
	ToolCallID    string        `json:"toolCallId,omitempty"`
	Status        string        `json:"status,omitempty"`
}

type SessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// StandardCancelParams is the session/cancel notification body. Named with the
// Standard prefix to avoid confusion with the legacy cancelParams in acp.go.
type StandardCancelParams struct {
	SessionID string `json:"sessionId"`
}

// StandardHandler is the reserved seam a future additive implementation will
// satisfy. It is intentionally NOT implemented or referenced today. Prompt's
// emit callback is the streaming channel: the implementation forwards obs
// events for the session's task as SessionUpdateParams until a terminal event,
// then returns the PromptResult stop reason.
type StandardHandler interface {
	Initialize(params StandardInitializeParams) (StandardInitializeResult, error)
	NewSession(params NewSessionParams) (NewSessionResult, error)
	Prompt(params PromptParams, emit func(SessionUpdateParams)) (PromptResult, error)
	Cancel(params StandardCancelParams)
}