package config

import (
	"time"

	"github.com/daybeam/vortex/pkg/filters"
	"github.com/daybeam/vortex/schemas"
)

// MCPDef describes a registered MCP server (pool member).
//
// Extracted from types.go to keep that file within its size ratchet — the MCP
// toggle (2026-10-22) added two fields. See docs/completed/2026-10-22/MCP_TOOL_TOGGLE_DESIGN.md.

type FewShotExample struct {
	UserIntent string         `json:"user_intent"`
	ToolCall   map[string]any `json:"tool_call"`
}

// CredentialScope declares whose identity an MCP presents to the remote side.
// See docs/plan/OAUTH_SHARED_MCP_MULTI_TENANT_DESIGN.md §4 L1.
type CredentialScope string

const (
	// CredentialOperator (default, fail-closed): the MCP authenticates using a
	// process environment variable set by whoever started the engine. The
	// remote sees the operator's identity, so any caller can act as the
	// operator. This is what every existing MCP does today.
	CredentialOperator CredentialScope = "operator"

	// CredentialPerTenant: each tenant provides their own credential (via
	// SecretRef or a future CredentialResolver). The remote sees the tenant,
	// not the operator, so the risk is tenant-scoped.
	CredentialPerTenant CredentialScope = "per_tenant"
)

type MCPDef struct {
	ID                  string                         `json:"id"`
	URL                 string                         `json:"url,omitempty"`
	Command             string                         `json:"command,omitempty"`
	Args                []string                       `json:"args,omitempty"`
	Dir                 string                         `json:"dir,omitempty"`
	Env                 map[string]string              `json:"env,omitempty"`
	Trusted             bool                           `json:"trusted"`
	Sandboxed           bool                           `json:"sandboxed,omitempty"`
	ExpiresAt           time.Time                      `json:"expires_at,omitempty"`
	AvailableTools      []string                       `json:"available_tools,omitempty"`
	Provides            []string                       `json:"provides,omitempty"`
	Domain              string                         `json:"domain,omitempty"`
	FullToolDefinitions []schemas.ToolDefinition       `json:"full_tool_definitions,omitempty"`
	RateLimit           *RateLimit                     `json:"rate_limit,omitempty"`
	Source              SourceType                     `json:"source,omitempty"`
	FewShots            map[string][]FewShotExample    `json:"few_shots,omitempty"`
	AuthHeaderName      string                         `json:"auth_header_name,omitempty"`
	AuthHeaderPrefix    string                         `json:"auth_header_prefix,omitempty"`
	APIKeyEnv           string                         `json:"api_key_env,omitempty"`
	URLAuthPlaceholder  string                         `json:"url_auth_placeholder,omitempty"`
	DiscoveryStatus     string                         `json:"discovery_status,omitempty"`
	PlatformOverrides   map[string]MCPPlatformOverride `json:"platform_overrides,omitempty"`
	ToolFilter          filters.ToolFilter             `json:"-"`
	MountPolicy         string                         `json:"mount_policy,omitempty"` // open|approval|admin_only (default admin_only); see mount_policy.go

	// Disabled removes this MCP from the model's tool surface entirely: no
	// discovery, no routing, no schema, and dispatch refuses it. Default false
	// (active). Strictly subtractive — never bypasses MountPolicy/tier.
	Disabled bool `json:"disabled,omitempty"`

	// DisabledTools is a per-MCP DENY list of tool names hidden from the model
	// and refused at dispatch. Names the MCP does not serve are ignored (never
	// widens the surface).
	DisabledTools []string `json:"disabled_tools,omitempty"`

	// Approver gates who may resolve a mount_policy=approval decision for
	// this MCP: "admin" (default; empty ⇒ admin) requires an admin-tier
	// resolver; "owner" allows the task owner (graph.OwnerID) or an admin.
	// See docs/plan/DECISION_APPROVAL_AUTHORITY_DESIGN.md §2.
	Approver string `json:"approver,omitempty"`

	// CredentialScope declares whose credential this MCP uses. Empty ⇒ operator
	// (fail-closed): the default matches today's behavior — every MCP with
	// api_key_env/url_auth_placeholder presents the operator's identity.
	// Set to "per_tenant" when each tenant has their own credential.
	// See docs/plan/OAUTH_SHARED_MCP_MULTI_TENANT_DESIGN.md §4 L1.
	CredentialScope CredentialScope `json:"credential_scope,omitempty"`
}
