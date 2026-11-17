package core

import (
	"context"

	"github.com/daybeam/vortex/config"
)

// DiscoveryKind identifies what a discovery source discovers.
type DiscoveryKind string

const (
	DiscoveryKindSkill DiscoveryKind = "skill"
	DiscoveryKindMCP   DiscoveryKind = "mcp"
)

// DiscoverySource is the unified interface for skill and MCP discovery from
// external sources. Each adapter implementation knows how to fetch and parse
// a specific source format (GitHub repo, HTTP JSON API, local directory).
//
// Ring 2: Domain Abstraction. Implementations live in Ring 3 (core/).
// The caller is responsible for ingestion (staging, gating, registration) —
// Discover only fetches and normalizes.
//
// See docs/completed/2026-11-13/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md.
type DiscoverySource interface {
	// Name returns the human-readable identifier for this source.
	Name() string

	// Kind returns "skill" or "mcp" — what this source discovers.
	Kind() DiscoveryKind

	// Discover fetches items from the source and returns them as normalized
	// descriptors. Returns an error if the source is unreachable or returns
	// malformed data.
	Discover(ctx context.Context) ([]DiscoveredItem, error)
}

// DiscoveredItem is the normalized result from any discovery source. It
// carries enough information to construct either a config.Skill or
// config.MCPDef, without being coupled to either struct directly.
//
// Skill-specific fields are populated when Kind == DiscoveryKindSkill;
// MCP-specific fields when Kind == DiscoveryKindMCP.
type DiscoveredItem struct {
	Kind DiscoveryKind `json:"kind"`
	ID   string        `json:"id"`
	Name string        `json:"name"`

	// Description is common to both skills and MCPs.
	Description string `json:"description,omitempty"`

	// Skill-specific fields (populated when Kind == DiscoveryKindSkill).
	Capability     string                              `json:"capability,omitempty"`
	Implementations map[string]config.SkillImplementation `json:"implementations,omitempty"`

	// MCP-specific fields (populated when Kind == DiscoveryKindMCP).
	URL      string            `json:"url,omitempty"`
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Provides []string          `json:"provides,omitempty"`

	// SkillDir is the local filesystem path to the skill directory, set when
	// the discovery source downloads content to disk (e.g. GitHubRepoSource
	// via FetchArchive). When non-empty, the skill is directory-based and
	// carries scripts/assets/references. Empty for prompt-only skills.
	SkillDir string `json:"skill_dir,omitempty"`

	// SourceName identifies which DiscoverySource produced this item.
	SourceName string `json:"source_name"`

	// TrustLevel controls the ingestion path:
	//   "trusted"   → directly registered (local directories, explicit trusted sources)
	//   "untrusted" → staged in PendingSkills (remote sources by default)
	// Empty defaults to "untrusted".
	TrustLevel string `json:"trust_level,omitempty"`
}

// TrustLevel constants for DiscoveredItem.
const (
	TrustLevelTrusted   = "trusted"
	TrustLevelUntrusted = "untrusted"
)

// DiscoverySourceType identifies the adapter type for a configured source.
type DiscoverySourceType string

const (
	DiscoverySourceGitHub DiscoverySourceType = "github"
	DiscoverySourceHTTP   DiscoverySourceType = "http"
	DiscoverySourceLocal  DiscoverySourceType = "local"
)
