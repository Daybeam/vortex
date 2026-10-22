package config

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/pkg/filters"
	"github.com/daybeam/vortex/schemas"
)

// envAPIKeyCache caches os.Getenv results for API key env vars.
// audit PERF-HIGH-2 (2026-10-08): eliminates per-LLM-request syscall.
var envAPIKeyCache sync.Map

// InvalidateAllEnvAPIKeys clears all cached env API keys. Called after a
// config reload (loadLocked) so ResolveAPIKey re-reads os.Getenv, picking
// up .env changes when StartWatcher triggers a reload.
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md §change 4.
func InvalidateAllEnvAPIKeys() {
	envAPIKeyCache.Range(func(key, _ any) bool {
		envAPIKeyCache.Delete(key)
		return true
	})
}

func (p *ProviderConfig) ResolveAPIKey(sp schemas.SecretProvider) string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		if sp != nil {
			val, _ := sp.GetSecret(p.APIKeyEnv)
			return val
		}
		// audit PERF-HIGH-2: cache os.Getenv — called on every LLM request.
		if v, ok := envAPIKeyCache.Load(p.APIKeyEnv); ok {
			return v.(string)
		}
		val := os.Getenv(p.APIKeyEnv)
		envAPIKeyCache.Store(p.APIKeyEnv, val)
		return val
	}
	return ""
}

func (p *ProviderConfig) HasCapability(cap string) bool {
	for _, c := range p.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func (p *ProviderConfig) SupportsVisionHeuristic() bool {
	if p.HasCapability(CapVision) {
		return true
	}
	if p.SupportsVision != nil {
		return *p.SupportsVision
	}
	m := strings.ToLower(p.Model)
	if strings.HasPrefix(m, "claude-3") {
		return true
	}
	if strings.Contains(m, "gpt-4o") || strings.Contains(m, "vision") {
		return true
	}
	if strings.Contains(m, "gemini") {
		return true
	}
	if strings.Contains(m, "llama-3.2") && strings.Contains(m, "vision") {
		return true
	}
	return false
}

type ModelArchetype string

const (
	ArchetypeAR        ModelArchetype = "ar"
	ArchetypeDiffusion ModelArchetype = "diffusion"
)

const (
	CapVision = "vision"
	CapT2I    = "t2i"
	CapT2V    = "t2v"
	CapCoding = "coding"
	CapTools  = "tools"
)

type SourceType string

const (
	SourceInternalStatic  SourceType = "internal_static"
	SourceExternalDynamic SourceType = "external_dynamic"
)

const (
	DomainGeneral   = "general"
	DomainCoding    = "coding"
	DomainResearch  = "research"
	DomainDevOps    = "devops"
	DomainFinancial = "financial"
	DomainMedia     = "media"
)

type ProviderConfig struct {
	Provider               string             `json:"provider"`
	Protocol               string             `json:"protocol,omitempty"`
	Model                  string             `json:"model"`
	Family                 string             `json:"family,omitempty"`
	EmbeddingModel         string             `json:"embedding_model,omitempty"`
	Archetype              ModelArchetype     `json:"archetype,omitempty"`
	APIKey                 string             `json:"api_key,omitempty"`
	APIKeyEnv              string             `json:"api_key_env,omitempty"`
	APIKeys                []string           `json:"api_keys,omitempty"`
	BaseURL                string             `json:"base_url,omitempty"`
	Temperature            *float32           `json:"temperature,omitempty"`
	FrequencyPenalty       *float32           `json:"frequency_penalty,omitempty"`
	MaxContextWindow       int                `json:"max_context_window,omitempty"`
	EffectiveContextWindow int                `json:"effective_context_window,omitempty"`
	TokenLimit             int                `json:"token_limit,omitempty"`
	ReserveTokens          int                `json:"reserve_tokens,omitempty"`
	RateLimit              *RateLimit         `json:"rate_limit,omitempty"`
	Extra                  map[string]any     `json:"extra,omitempty"`
	Capabilities           []string           `json:"capabilities,omitempty"`
	SupportsVision         *bool              `json:"supports_vision,omitempty"`
	Instances              []ProviderInstance `json:"instances,omitempty"`
	PoolID                 string             `json:"pool_id,omitempty"`
	// FallbackChain is an ordered list of provider configs to try as fallbacks
	// when the primary provider fails. Each is created via the factory and
	// wrapped with its own retry/rate-limit. The FallbackProvider tries
	// providers sequentially: primary → fallback1 → fallback2 → ...
	// This is complementary to the ProviderRouter (which does automatic
	// pool-based round-robin + health detection). Use FallbackChain for
	// explicit ordered failover (e.g., cloud → local → dummy).
	FallbackChain []ProviderConfig `json:"fallback_chain,omitempty"`
}

type ProviderInstance struct {
	ID        string `json:"id"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
}

type RateLimit struct {
	RequestsPerMinute int   `json:"requests_per_minute"`
	RetryWaitSeconds  []int `json:"retry_wait_seconds"`
}

func (s *Skill) InitFilter() {
	keywords := strings.FieldsFunc(
		strings.ToLower(fmt.Sprintf("%s %s %s %s", s.Name, s.Capability, s.Domain, s.Description)),
		func(r rune) bool {
			return r == ' ' || r == ',' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':' || r == '(' || r == ')' || r == '[' || r == ']'
		},
	)
	s.Filter = filters.NewCuckooToolFilter(uint(len(keywords)))
	for _, k := range keywords {
		if len(k) >= 3 {
			s.Filter.Add(k)
		}
	}
}

func (s *Skill) GetPrompt(modelID string, familyOverride ...string) (string, error) {
	family := ""
	if len(familyOverride) > 0 && familyOverride[0] != "" {
		family = familyOverride[0]
	} else {
		family = detectFamily(modelID)
	}
	if impl, ok := s.Implementations[family]; ok {
		return impl.SystemPrompt, nil
	}
	if impl, ok := s.Implementations["default"]; ok {
		return impl.SystemPrompt, nil
	}
	return "", fmt.Errorf("skill %q has no implementation for family %q and no default fallback", s.ID, family)
}

type ExecutionMode string

const (
	ExecutionModeFlat      ExecutionMode = "flat"
	ExecutionModeDirectory ExecutionMode = "directory"
)

type SkillImplementation struct {
	SystemPrompt  string `json:"system_prompt"`
	PromptVersion string `json:"prompt_version"`
}

type Skill struct {
	ID              string                         `json:"id"`
	Name            string                         `json:"name"`
	Capability      string                         `json:"capability"`
	Domain          string                         `json:"domain,omitempty"`
	Description     string                         `json:"description"`
	InputSchema     map[string]any                 `json:"input_schema"`
	OutputSchema    map[string]any                 `json:"output_schema"`
	TokenEstimate   int                            `json:"token_estimate"`
	Requires        []string                       `json:"requires"`
	Incompatible    []string                       `json:"incompatible"`
	Source          SourceType                     `json:"source,omitempty"`
	Implementations map[string]SkillImplementation `json:"implementations"`

	// Extended fields for directory-based skills
	ExecutionMode ExecutionMode `json:"execution_mode,omitempty"`
	SkillDir      string        `json:"skill_dir,omitempty"`
	PromptsDir    string        `json:"prompts_dir,omitempty"`
	ToolsDir      string        `json:"tools_dir,omitempty"`

	// Multi-capability tags (symmetric with MCP.Provides).
	Provides []string `json:"provides,omitempty"`
	// MountPolicy gates skill granting by submitter tier (open/approval/
	// admin_only; default open — skills are prompt-level, see mount_policy.go).
	MountPolicy string `json:"mount_policy,omitempty"`
	// Directory content manifest — path list only, no file contents.
	Manifest *SkillManifest `json:"manifest,omitempty"`

	// agentskills.io standard frontmatter fields (docs/plan/AGENTIC_SKILL_EXPLORATION_DESIGN.md).
	// Parsed at import; surfaced as prompt hints at activation. Non-functional
	// for execution — compatibility warns, allowed-tools is advisory only.
	Compatibility string            `json:"compatibility,omitempty"`
	AllowedTools  []string          `json:"allowed_tools,omitempty"`
	License       string            `json:"license,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`

	// StagedAt records when the skill was placed into PendingSkills (staging).
	// Used by PendingSkillGC to expire un-promoted external skills after TTL.
	// Zero value means the skill was not staged (native or already promoted).
	StagedAt time.Time `json:"staged_at,omitempty"`

	Filter filters.ToolFilter `json:"-"`
}

// SkillManifest is a path-only listing of a skill's directory contents.
// It stores relative paths (e.g. "references/xxx.md"), never file contents.
type SkillManifest struct {
	References []string `json:"references,omitempty"` // "references/xxx.md"
	Scripts    []string `json:"scripts,omitempty"`    // "scripts/xxx.sh"
	Assets     []string `json:"assets,omitempty"`     // "assets/xxx.png"
}

func (m *MCPDef) ResolveForPlatform() (command string, args []string, dir string, env map[string]string) {
	command, args, dir, env = m.Command, m.Args, m.Dir, m.Env
	if m.PlatformOverrides == nil {
		return
	}
	ov, ok := m.PlatformOverrides[runtime.GOOS]
	if !ok {
		return
	}
	if ov.Command != "" {
		command = ov.Command
	}
	if len(ov.Args) > 0 {
		args = ov.Args
	}
	if ov.Dir != "" {
		dir = ov.Dir
	}
	if len(ov.Env) > 0 {
		env = ov.Env
	}
	return
}

func (m *MCPDef) InitFilter() {
	expected := uint(len(m.AvailableTools))
	if expected == 0 {
		expected = 100
	}
	if m.Command != "" {
		m.ToolFilter = filters.NewBloomToolFilter(expected)
	} else {
		m.ToolFilter = filters.NewCuckooToolFilter(expected)
	}
	for _, t := range m.AvailableTools {
		m.ToolFilter.Add(t)
	}
}

const (
	DiscoveryPending = "pending"
	DiscoveryOK      = "ok"
	DiscoveryFailed  = "failed"
	DiscoveryNoAuth  = "no_auth"
	DiscoveryNoURL   = "no_url"
)

// MCPDef + FewShotExample moved to mcp_def.go (file size ratchet).

// ── Unified Capability Driver Pattern ──────────────────────────────────────
//
// Implements docs/architecture/UNIFIED_CAPABILITY_DISCOVERY_DESIGN.md.
// Decouples capability discovery from execution: whether a capability is a
// remote SSE endpoint or (future) a local binary, the orchestrator interacts
// with it through the same CapabilityDriver lifecycle.

// CapabilityTransport declares how a capability is reached.
type CapabilityTransport string

const (
	TransportRemoteSSE    CapabilityTransport = "remote_sse"    // Phase 1: remote SSE/HTTP MCP
	TransportLocalProcess CapabilityTransport = "local_process" // Phase 2 (future): local binary
	TransportSandboxJIT   CapabilityTransport = "sandbox_jit"   // Phase 2 (future): sandboxed script
)

// CapabilityDef is the transport-agnostic declaration of one external capability.
// For TransportRemoteSSE, Endpoint is the SSE URL. For future transports,
// Endpoint is the local binary path or script reference.
type CapabilityDef struct {
	ID        string              `json:"id"`
	Transport CapabilityTransport `json:"transport"`
	Endpoint  string              `json:"endpoint"`
	Manifest  map[string]any      `json:"manifest,omitempty"`
}

func (r *Role) UnmarshalJSON(data []byte) error {
	type Alias Role
	aux := &struct {
		RoleID string `json:"role_id"`
		Role   string `json:"role"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ID == "" {
		if aux.RoleID != "" {
			r.ID = aux.RoleID
		} else if aux.Role != "" {
			r.ID = aux.Role
		}
	}
	return nil
}

func (r *Role) BoundMCPIDs() []string {
	ids := make([]string, len(r.BoundMCPBindings))
	for i, b := range r.BoundMCPBindings {
		ids[i] = b.MCPID
	}
	return ids
}

func (r *Role) GetBinding(mcpID string) *MCPBinding {
	for i := range r.BoundMCPBindings {
		if r.BoundMCPBindings[i].MCPID == mcpID {
			return &r.BoundMCPBindings[i]
		}
	}
	return nil
}

type MCPBinding struct {
	MCPID         string   `json:"mcp_id"`
	AllowedTools  []string `json:"allowed_tools"`
	AlwaysInclude []string `json:"always_include,omitempty"`
}

func (b *MCPBinding) IsRestricted() bool { return len(b.AllowedTools) > 0 }

type RoleFallback struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type Role struct {
	ID                  string       `json:"id"`
	Version             string       `json:"version,omitempty"`
	Author              string       `json:"author,omitempty"`
	MutableByAgent      bool         `json:"mutable_by_agent,omitempty"`
	Name                string       `json:"name"`
	BaseCapability      string       `json:"base_capability"`
	BoundSkills         []string     `json:"bound_skills"`
	BoundMCPBindings    []MCPBinding `json:"bound_mcp_bindings"`
	Requires            []string     `json:"requires,omitempty"`
	Optional            []string     `json:"optional,omitempty"`
	Generatable         bool         `json:"generatable,omitempty"`
	AllowDynamicSkills  bool         `json:"allow_dynamic_skills"`
	AllowDynamicMCPs    bool         `json:"allow_dynamic_mcps"`
	MaxAdditionalSkills int          `json:"max_additional_skills"`
	// CoreTools narrows the built-in _core tool surface for this role:
	//   absent (nil)   -> every core tool (the DEFAULT — core tools are part
	//                     of the engine's operating mechanism: step hand-offs
	//                     flow through the shared output directory via
	//                     write_file/read_file, and verification methods name
	//                     read_file; A6 flip to "absent = none" was reverted
	//                     by product decision 2026-10-22)
	//   []             -> none (task_planner's case)
	//   ["read_file"]  -> only the named ones
	//   ["*"]          -> every core tool (explicit, self-documenting form)
	//
	// Added 2026-10-19 for the task_planner. Every spawn gets the _core server
	// attached by default (spawner_mcp.go buildMCPServers), which hands a planner
	// write_file and execute_code — so a planner that decides to "just do the
	// task" already can, and its instruction claiming "you have no tools" is
	// simply false. Hiding a tool from the advertised schema is not enough
	// either: spawner_tool_exec.go dispatches on CoreToolNames alone, with no
	// role check, so a named call would still run. This field is therefore
	// enforced at BOTH ends — schema assembly and dispatch.
	CoreTools []string `json:"core_tools,omitempty"`
	// MinimalPrompt opts the role out of the framework's own framing. When
	// true, the prompt assembler omits two fragments it otherwise appends to
	// every role:
	//
	//   (a) "## Core File & Code Tools" — the claim that write_file, read_file
	//       and execute_code are available. For a role with core_tools: [] that
	//       claim is false, and a model told it can write files will try to.
	//   (b) the SubagentOutput contract (status/confidence/result/attachments)
	//       plus its "you may use <thought> blocks before the JSON" allowance.
	//       A role that defines its own output shape in its instruction — the
	//       task planner emitting a bare {"steps": [...]} — is pulled in two
	//       directions by the generic contract, and the <thought> allowance
	//       silently spends the response budget before the real answer starts.
	//
	// Measured 2026-10-19 (gemini-3-flash, AppWorld 4fab96f_3): the planner
	// wrote a long <thought> preamble, then began the plan, and the answer was
	// cut off mid-tool-name; thought-stripping left a 572-byte fragment that
	// failed as "unknown tool name venmo__".
	MinimalPrompt   bool              `json:"minimal_prompt,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	Model           string            `json:"model,omitempty"`
	Fallbacks       []RoleFallback    `json:"fallbacks,omitempty"`
	DisableFallback bool              `json:"disable_fallback,omitempty"`
	Instruction     string            `json:"instruction,omitempty"`
	Rules           string            `json:"rules,omitempty"`
	Extends         string            `json:"extends,omitempty"`
	Purpose         string            `json:"purpose,omitempty"`
	BestFor         string            `json:"best_for,omitempty"`
	Examples        []string          `json:"examples,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// ─── RoleGroup Types ─────────────────────────────────────────────────────

type GroupPolicy string

const (
	GroupPolicySequential     GroupPolicy = "sequential"
	GroupPolicyParallel       GroupPolicy = "parallel"
	GroupPolicyVoting         GroupPolicy = "voting"
	GroupPolicyChainOfThought GroupPolicy = "chain_of_thought"
	GroupPolicySOP            GroupPolicy = "sop"
)

type GroupMember struct {
	RoleID           string  `json:"role_id"`
	TaskTemplate     string  `json:"task_template,omitempty"`
	ProviderOverride string  `json:"provider,omitempty"`
	Weight           float64 `json:"weight,omitempty"`
	Optional         bool    `json:"optional,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling for GroupMember to support
// "role" as an alias for "role_id", mirroring Role.UnmarshalJSON above.
func (m *GroupMember) UnmarshalJSON(data []byte) error {
	type Alias GroupMember
	aux := &struct {
		Role string `json:"role"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if m.RoleID == "" && aux.Role != "" {
		m.RoleID = aux.Role
	}
	return nil
}

type RoleGroup struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Members          []GroupMember     `json:"members"`
	Policy           GroupPolicy       `json:"policy"`
	SOPRef           string            `json:"sop_ref,omitempty"`
	AggregatorRoleID string            `json:"aggregator_role_id,omitempty"`
	MaxParallelism   int               `json:"max_parallelism,omitempty"`
	Domain           string            `json:"domain,omitempty"`
	Purpose          string            `json:"purpose,omitempty"`
	BestFor          string            `json:"best_for,omitempty"`
	Examples         []string          `json:"examples,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// ─── System Types ────────────────────────────────────────────────────────

type SandboxConfig struct {
	Mode              string   `json:"mode,omitempty"`
	AllowedWorkspaces []string `json:"allowed_workspaces,omitempty"`
	DockerImage       string   `json:"docker_image,omitempty"`   // image for docker mode (default: python:3.12-slim)
	DockerNetwork     string   `json:"docker_network,omitempty"` // "none" (default) | "host" | custom
}

// RetentionSettings controls dual-mode (headless vs GUI) storage compaction
// and lifecycle retention. See docs/architecture/STORAGE_COMPACTION_DESIGN.md.
type RetentionSettings struct {
	Mode                   string `json:"mode,omitempty"`
	TaskHistoryDays        int    `json:"task_history_days,omitempty"`
	StagingTTLHours        int    `json:"staging_ttl_hours,omitempty"`
	MaxCompletedTasks      int    `json:"max_completed_tasks,omitempty"`
	MaxPromotionAuditLogs  int    `json:"max_promotion_audit_logs,omitempty"`
	AutoVacuumIntervalDays int    `json:"auto_vacuum_interval_days,omitempty"`
	VacuumOnStartup        bool   `json:"vacuum_on_startup,omitempty"`
}

// FeatureFlags holds behavior toggles that were previously env-var-only
// (sync.OnceValue cached, no hot-reload). Migrating them into config.json
// enables hot-reload via StartWatcher. Env vars still override config values
// for backward compatibility (see applySystemDefaults).
//
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md.
type FeatureFlags struct {
	Debug                  bool `json:"debug,omitempty"`
	CoreToolsOrderLast     bool `json:"core_tools_order_last,omitempty"`
	DisableForceTool       bool `json:"disable_force_tool,omitempty"`
	DisableEgressRedaction bool `json:"disable_egress_redaction,omitempty"`
	DisableCgroupSandbox   bool `json:"disable_cgroup_sandbox,omitempty"`
	DisablePatternKill     bool `json:"disable_pattern_kill,omitempty"`
	DisableThinking        bool `json:"disable_thinking,omitempty"`
	EvalObs                bool `json:"eval_obs,omitempty"`
	FSOnlyStreakLimit      int  `json:"fs_only_streak_limit,omitempty"`
	// ZeroEffectNudge enables the "declared ok but zero side effects" gate
	// (ADDED 2026-10-21, eval v14). Off by default: the guard is intentionally
	// noisy for tasks whose correct answer IS a read-only query, so it is opt-in
	// per deployment. See core/prompt_constants.go SpawnerZeroEffectMsg.
	ZeroEffectNudge bool `json:"zero_effect_nudge,omitempty"`
	// MidAbandonNudge enables the mid-abandonment gate (A5, eval FINDINGS.md
	// §A5, 2026-10-22): the model started working (read-only calls in the
	// trace), then emitted a PARSEABLE partial and stopped while turns remain.
	// This is the highest-frequency small-model failure mode ("worked to the
	// middle, gave up") that noToolCallsYet and the zero-effect gate are both
	// blind to. Off by default, opt-in per deployment like ZeroEffectNudge.
	// See core/prompt_constants.go SpawnerMidAbandonMsg.
	MidAbandonNudge bool `json:"mid_abandon_nudge,omitempty"`
}

// ProviderRoutingConfig configures provider routing strategy and telemetry.
// See docs/completed/2026-10-21/PROVIDER_ROUTING_TELEMETRY_DESIGN.md.
type ProviderRoutingConfig struct {
	Strategy              string `json:"strategy,omitempty"`                 // "coherence_first" (default) or "latency_first"
	SlidingWindowSize     int    `json:"sliding_window_size,omitempty"`      // e.g. 100
	P95DegradeThresholdMS int    `json:"p95_degrade_threshold_ms,omitempty"` // e.g. 3000
	CooldownSeconds       int    `json:"cooldown_seconds,omitempty"`         // 429 cooldown in seconds
}

type SystemSettings struct {
	ConfidenceThreshold float64 `json:"confidence_threshold"`
	MaxDecisionOutcomes int     `json:"max_decision_outcomes"`
	MaxContextKeep      int     `json:"max_context_keep"`
	MaxToolResultChars  int     `json:"max_tool_result_chars,omitempty"` // 0 = dynamic from model context window; >0 = explicit override
	// MaxToolSchemaBudgetTokens caps the total size of the tool schemas the
	// router exposes in one step. 0 = router default
	// (core.defaultToolSchemaBudgetTokens). Expressed in tokens rather than a
	// tool count because schema size, not entry count, is what consumes the
	// context window.
	MaxToolSchemaBudgetTokens int            `json:"max_tool_schema_budget_tokens,omitempty"`
	MaxToolTurns              int            `json:"max_tool_turns,omitempty"`
	MaxSpawnDepth             int            `json:"max_spawn_depth,omitempty"`
	DefaultJITTTL             int            `json:"default_jit_ttl"`
	SandboxedMemoryMB         int            `json:"sandboxed_memory_mb"`
	SandboxedCPUSecs          int            `json:"sandboxed_cpu_secs"`
	MaxLogSizeMB              int            `json:"max_log_size_mb"`
	CookbookSyncIntervalHours int            `json:"cookbook_sync_interval_hours,omitempty"`
	CookbookCacheEnabled      bool           `json:"cookbook_cache_enabled,omitempty"`
	SkillSyncIntervalHours    int            `json:"skill_sync_interval_hours,omitempty"` // periodic skill sync interval (default 24h)
	Bookmarks                 []string       `json:"bookmarks,omitempty"`
	ExternalInterceptors      []PluginConfig `json:"external_interceptors,omitempty"`
	SwarmFallbackDelay        int            `json:"swarm_fallback_delay,omitempty"`
	SwarmAgentCount           int            `json:"swarm_agent_count,omitempty"` // audit L1: was hardcoded 5
	DelegationMode            bool           `json:"delegation_mode,omitempty"`
	// NonInteractive enables unattended mode: when a step would block on a
	// decision (step failure, low confidence, max-turns, autonomous abort),
	// the engine auto-selects a conservative default instead of blocking
	// forever. Required for CI/batch eval where no human can resolve
	// decisions. Default false = fully interactive (zero behavior change).
	// human_approval_required is NEVER auto-resolved (deliberate safety gate).
	// E5 fix (2026-10-02).
	NonInteractive bool `json:"non_interactive,omitempty"`
	// AllowCriticalAutoResolve (ADDED 2026-10-17): when NonInteractive is on
	// and this is true, RiskTierCritical decisions are auto-resolved by the
	// decider / conservative defaults instead of hanging forever. Escape valve
	// for CI/batch eval where no human can resolve critical decisions.
	// Default false: critical decisions always wait (safety gate, unchanged).
	// See docs/completed/2026-10-17/CRITICAL_TIER_AUTO_RESOLVE_OVERRIDE_DESIGN.md.
	AllowCriticalAutoResolve bool `json:"allow_critical_auto_resolve,omitempty"`
	// AllowHumanApprovalAutoResolve (ADDED 2026-10-17): when true,
	// human_approval_required decisions in NonInteractive mode are resolved
	// by the decider / conservative default ("abort") instead of hanging.
	// Stricter override than AllowCriticalAutoResolve — bypasses a legal gate.
	// Default false.
	AllowHumanApprovalAutoResolve bool `json:"allow_human_approval_auto_resolve,omitempty"`
	// MemoryBankEnabled controls whether MemoryBank injection (ChatHarness
	// system prompt) and auto-extraction (ReflectionEngine) are active.
	// Default true (backward compatible). Set to false to disable.
	// ADDED 2026-10-17. See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md.
	MemoryBankEnabled bool `json:"memory_bank_enabled"`
	// DecisionDeciderRole: when NonInteractive is on and this is set, the engine
	// spawns this role to evaluate each blocked decision and choose an option,
	// instead of falling back to hardcoded conservative defaults. The decider
	// role's Instruction carries domain-specific decision logic. Empty = use
	// conservative defaults (defaultNonInteractiveChoice). E5 delegate strategy.
	DecisionDeciderRole string `json:"decision_decider_role,omitempty"`
	// MaxConsecutiveSkips caps how many decisions in a row can be auto-resolved
	// as "skip" before "skip" is filtered out of options. Prevents silent
	// cascade where every dependent step gets skipped, making decisions
	// effectively useless. 0 = default 3. Only applies when NonInteractive=true.
	MaxConsecutiveSkips int `json:"max_consecutive_skips,omitempty"`
	// ChatMemoryWindowSize controls how many recent turns RollingWindowMemory
	// keeps raw before compressing older turns. 0 = default 10.
	ChatMemoryWindowSize int `json:"chat_memory_window_size,omitempty"`
	// ChatMemoryMaxTokens sets an optional token budget for chat history.
	// When > 0, oldest messages are dropped when the budget is exceeded.
	// 0 = no token budget (disabled).
	ChatMemoryMaxTokens int `json:"chat_memory_max_tokens,omitempty"`
	// ReadinessGateSoft softens the delegation readiness gate language from
	// "NEVER blindly delegate" to a suggestion. Set true for weak models (≤7B)
	// that over-apply the hard rule and ask excessive clarifying questions
	// instead of acting. Default false = strict (for strong models).
	ReadinessGateSoft bool `json:"readiness_gate_soft,omitempty"`
	// RequireWriteReason makes the write-reflection gate (#19) a hard veto
	// instead of a nudge.
	//
	// The gate's purpose (see core/spawner.go) is to nudge the model into
	// reflecting before it writes. Measured on a 4B model (eval §8.51.4-7,
	// airline task 7): three rounds of prompt rewriting failed to get the
	// model to supply a well-formed `_reason` argument, and the cost of
	// continuing to block was that the write NEVER happened (Write Actions
	// 0/3 in every arm). Default false = record the nudge, let the write
	// proceed. Set true to restore hard blocking (e.g. when an external
	// orchestrator does the authorization).
	RequireWriteReason bool `json:"require_write_reason,omitempty"`
	// DedupHardCap is the reason-blind ceiling for identical calls inside one
	// step (#11). Default 5 when unset.
	//
	// #11's reflection gate lets a model that supplies `_reason` retry, on the
	// theory that articulating why interrupts blind repetition. That is
	// guidance, and guidance must not be able to remove the ceiling it is
	// attached to: on 4fab96f_3 a model that learned `_reason` re-read the same
	// page 24 times in one step with no check firing at all. This cap counts
	// canonical arguments (control args excluded) and cannot be lifted by any
	// argument the model writes. See docs/54 §5.18.
	DedupHardCap             int             `json:"dedup_hard_cap,omitempty"`
	RefBasedHandoffThreshold int64           `json:"ref_based_handoff_threshold,omitempty"`
	AntiSlop                 AntiSlopConfig  `json:"anti_slop,omitempty"`
	StagingEnabled           bool            `json:"staging_enabled,omitempty"`
	Telemetry                TelemetryConfig `json:"telemetry,omitempty"`
	DefaultTaskTokenBudget   int64           `json:"default_task_token_budget,omitempty"`
	ExperienceGraphBudget    int             `json:"experience_graph_budget,omitempty"`
	StagingRetentionHours    int             `json:"staging_retention_hours,omitempty"`
	// MaxConcurrentSteps caps the number of steps that may execute in parallel
	// within a single task graph (audit finding M4). Defaults to 10 if unset.
	MaxConcurrentSteps      int `json:"max_concurrent_steps,omitempty"`
	MaxTaskChars            int `json:"max_task_chars,omitempty"`
	ToolRepetitionThreshold int `json:"tool_repetition_threshold,omitempty"`
	// Reranker configures the optional System One experience reranker.
	// When BaseURL is empty, the reranker is disabled and retrieval
	// uses coarse tier ranking only (graceful degradation).
	Reranker RerankerConfig `json:"reranker,omitempty"`
	// SystemOne configures the optional System One decision model for
	// decision gate, tool router, and failure classification. When BaseURL
	// is empty, all three points use their existing fallback logic
	// (graceful degradation — zero behavior change).
	SystemOne SystemOneConfig `json:"systemone,omitempty"`
	// AllowedOrigins restricts CORS to these origins (audit S-M4). If empty,
	// defaults to "*" for backward compat (single-machine). Set to explicit
	// origins (e.g. ["https://app.example.com"]) for multi-tenant deployments.
	AllowedOrigins []string      `json:"allowed_origins,omitempty"`
	Sandbox        SandboxConfig `json:"sandbox,omitempty"`
	// EnableAutoRepair controls the VDA self-healing path in HealthCheckInterceptor.
	// When false (default), missing MCP dependencies are logged and degraded
	// without executing any local fix scripts. When true, the interceptor will
	// attempt to run scripts/fix/fix_<cmd>.{sh,bat} if present.
	// This is a safety gate: auto-executing shell scripts in the routine task
	// path is the same risk class as F25/F26 (unattended code execution).
	EnableAutoRepair bool `json:"enable_auto_repair,omitempty"`
	// FMCWeakModel names a registered provider (a key in Registry.Providers)
	// to use as the "weak model" for FMC (Failure Mode Classification) batch
	// backfilling (ExperienceStore.RunFMCBatch). Opt-in: when empty (the
	// default), the FMC batch ticker (core/fmc_ticker.go) does not run at
	// all. A cheap/fast model is intended here (e.g. GPT-4o-mini, Gemini
	// Flash), not the primary task-execution provider -- see
	// docs/completed/2026-09-06/architecture/FMC_DESIGN.md §4.2/§8 (Phase 2).
	FMCWeakModel string `json:"fmc_weak_model,omitempty"`
	// FMCBatchIntervalHours controls how often the FMC batch ticker runs.
	// Defaults to 6 hours if unset/zero, matching the design doc's guidance.
	FMCBatchIntervalHours int                `json:"fmc_batch_interval_hours,omitempty"`
	Notifications         NotificationConfig `json:"notifications,omitempty"`
	BudgetGate            BudgetGateConfig   `json:"budget_gate,omitempty"`
	Retention             RetentionSettings  `json:"retention,omitempty"`
	// OutputDir overrides the default "outputs" directory for task output.
	// When empty (the default), "outputs" is used. Relative to project root.
	OutputDir string `json:"output_dir,omitempty"`

	// WarmContextBudgetTokens (ADDED 2026-10-10): token budget for the
	// Warm layer in PromptAssembler (ActiveContextAssembler). Default 2000.
	// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §5.
	WarmContextBudgetTokens int `json:"warm_context_budget_tokens,omitempty"`
	// WarmContextEmbedEnabled (ADDED 2026-10-10): when true, the Warm layer
	// uses vector recall via the embedding client from ContextHub. When
	// false, degrades to keyword matching. Default true.
	WarmContextEmbedEnabled bool `json:"warm_context_embed_enabled,omitempty"`

	// APIKeys enables named-key authentication with per-key owner identity.
	// When empty (the default), behavior is identical to legacy env-var keys:
	// every caller resolves to OwnerID="" and all ownership checks are no-ops.
	APIKeys []NamedAPIKey `json:"api_keys,omitempty"`

	// Hooks are config-driven automated habits/rules that fire at specific
	// lifecycle points (chat and DAG). See docs/completed/2026-09-28/UNIFIED_HOOK_ARCHITECTURE_DESIGN.md.
	// Each hook declares a point, script path, timeout, and optional filter.
	Hooks []HookConfig `json:"hooks,omitempty"`

	// WorkspacePolicy (ADDED 2026-10-13): tier-gated workspace scope.
	// When Enabled, Public tier callers have workspace_root forced to
	// a session sandbox; Admin/local callers are unaffected.
	// See docs/completed/2026-10-21/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md.
	WorkspacePolicy WorkspacePolicyConfig `json:"workspace_policy,omitempty"`

	// FeatureFlags (ADDED 2026-10-17): behavior toggles migrated from env vars.
	// Hot-reloadable via StartWatcher. Env vars override for backward compat.
	// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md.
	FeatureFlags FeatureFlags `json:"feature_flags,omitempty"`

	// CriticalRiskKeywords / ModerateRiskKeywords (ADDED 2026-10-18):
	// override the keyword lists used by schemas.DetermineRiskTier.
	// Empty = use engine defaults (browser/image/generate/... for critical,
	// patch/refactor/build/test for moderate). Set explicitly to customize
	// risk classification per deployment without engine changes.
	// See schemas.SetRiskTierKeywords.
	CriticalRiskKeywords []string `json:"critical_risk_keywords"`
	ModerateRiskKeywords []string `json:"moderate_risk_keywords"`
	// SanitizerEnabled (ADDED 2026-10-18): enables bidirectional PII
	// tokenization on the provider path. When true, secrets in prompts
	// (API keys, tokens) are replaced with [REDACTED_N] before sending
	// to the provider, and restored in responses. Required when the
	// provider routes through an untrusted third-party relay pool.
	// Default false: pass-through (no masking, backward compat).
	SanitizerEnabled bool `json:"sanitizer_enabled,omitempty"`

	// EnablePlannerFirst (ADDED 2026-10-18): enables tier 1(b) planning —
	// a `task_planner` spawn BEFORE any step exists, whose DAG replaces the
	// caller's single step. Off by default; see docs 52-planner-wiring-design.md
	// §8.5 for why AppWorld must keep it off until the tool-catalog question
	// is settled.
	EnablePlannerFirst bool `json:"enable_planner_first,omitempty"`

	// LazyDiscovery (ADDED 2026-10-20): when true, tools/list discovery for
	// stdio MCPs runs lazily inside ToolRouter.Route on first access (when
	// AvailableTools is empty), instead of eagerly in spawner_prepare.go.
	// Default false: eager discovery (backward compatible).
	// See docs/plan/TWO_LAYER_SCOPED_DISCOVERY_DESIGN.md §10 Phase 2.
	LazyDiscovery bool `json:"lazy_discovery,omitempty"`

	// ProviderRouting (ADDED 2026-10-21): configures provider routing strategy
	// and telemetry. See docs/completed/2026-10-21/PROVIDER_ROUTING_TELEMETRY_DESIGN.md.
	ProviderRouting ProviderRoutingConfig `json:"provider_routing,omitempty"`

	// RejectSimpleTasks (ADDED 2026-10-20): when true, validateTaskComplexity
	// rejects single-step local tasks that have no additional skills or MCPs.
	// Default false (opt-in): the Smart Routing Fast-Path
	// (scheduler_submit.go:249) already handles single-step dependency-free
	// tasks with near-zero scheduling overhead (minimal graph entry + Spawn,
	// no DAG construction, no manifest, no run() loop), so the previous
	// hardcoded rejection blocked those tasks from reaching FastPath. The
	// guard is preserved as an opt-in for local (llama.cpp/vLLM) deployments
	// where operators want to prevent orchestrator misuse as a plain shell
	// (e.g. submitting {"execute_code": "rm -rf /"}). Remote API deployments
	// are never affected by this flag regardless of setting.
	// See §3.4 of docs/plan/TWO_LAYER_SCOPED_DISCOVERY_DESIGN.md.
	RejectSimpleTasks bool `json:"reject_simple_tasks,omitempty"`
	// PlannerTimeoutSecs bounds the planner spawn (0 => 90s default).
	// A 60s hard limit proved too tight for a planner reading a large tool
	// catalog on a slow endpoint.
	PlannerTimeoutSecs int `json:"planner_timeout_secs,omitempty"`
	// PlannerMaxSteps bounds the produced DAG (0 => parser default 12).
	PlannerMaxSteps int `json:"planner_max_steps,omitempty"`
	// PlannerMaxOutputTokens bounds the planner's own answer length
	// (0 => 8192). The plan is a structured document, so it needs markedly more
	// room than a prose answer: measured 2026-10-19, a 6-step plan hit the
	// engine's hard-coded 4096-token cap mid-tool-name, and the salvaged
	// fragment ("venmo__") failed the whole plan as an unknown tool.
	PlannerMaxOutputTokens int `json:"planner_max_output_tokens,omitempty"`

	// DefaultSpawnOutputTokens overrides the response cap for step spawns that
	// do not set their own (0 => 8192).
	//
	// This cap covers EVERYTHING the model emits, and on a thinking model that
	// includes reasoning tokens, which produce no visible output at all.
	// Measured 2026-10-20 on gemini-3-flash-preview via an OpenAI-compatible
	// gateway: three responses hit the old 4096 cap at 4068/4069/3932
	// completion tokens, and the last of them emitted ZERO characters of text
	// and ZERO tool calls — the entire budget went to reasoning. The step then
	// spun on a repeated tool call until the Sieve loop guard aborted it, so a
	// cap that looked like a prose limit silently became a correctness bug.
	//
	// Raise it for models with long chains of thought; lower it for small local
	// models where an oversized cap invites rambling. Operators tuning a
	// thinking model should also check whether reasoning is a large share of
	// completion tokens — the output_truncated event reports visible_chars and
	// tool_calls so a reasoning-dominated truncation is distinguishable from a
	// genuinely long answer.
	DefaultSpawnOutputTokens int `json:"default_spawn_output_tokens,omitempty"`
}

// WorkspacePolicyConfig configures tier-gated workspace scope resolution.
// See docs/completed/2026-10-21/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §3.6.
type WorkspacePolicyConfig struct {
	Enabled            bool                      `json:"enabled,omitempty"`
	SessionSandboxBase string                    `json:"session_sandbox_base,omitempty"`
	AllowedRoots       []string                  `json:"allowed_roots,omitempty"`
	PerOwner           map[string]PerOwnerPolicy `json:"per_owner,omitempty"`
}

// PerOwnerPolicy allows per-owner workspace root overrides.
type PerOwnerPolicy struct {
	AllowedRoots     []string `json:"allowed_roots,omitempty"`
	AllowAdminBypass bool     `json:"allow_admin_bypass,omitempty"`
}

// RerankerConfig configures the optional System One experience reranker.
// When BaseURL is empty, the reranker is disabled and retrieval uses
// coarse tier ranking only (graceful degradation — zero behavior change).
//
// Example config.json:
//
//	"system": {
//	  "reranker": {
//	    "base_url": "http://127.0.0.1:8000",
//	    "model": "laya"
//	  }
//	}
type RerankerConfig struct {
	BaseURL string `json:"base_url,omitempty"` // SystemOne/Jev/Laya endpoint (e.g. http://127.0.0.1:8000)
	Model   string `json:"model,omitempty"`    // model name (e.g. "laya", "jev")
	APIKey  string `json:"api_key,omitempty"`  // bearer token (optional for local self-hosted)
}

// SystemOneConfig configures the optional System One decision model for
// three internal decision points: decision gate (NonInteractive mode),
// tool router (semantic scoring), and failure classification (transient
// refinement). Each point is independently toggleable. When BaseURL is
// empty or the specific enable flag is false, the point uses its existing
// fallback logic (graceful degradation — zero behavior change).
//
// Example config.json:
//
//	"system": {
//	  "systemone": {
//	    "base_url": "http://127.0.0.1:8000",
//	    "model": "jev",
//	    "enable_decision_gate": true,
//	    "enable_tool_router": true,
//	    "min_confidence": 0.5
//	  }
//	}
type SystemOneConfig struct {
	BaseURL               string  `json:"base_url,omitempty"`
	Model                 string  `json:"model,omitempty"`
	APIKey                string  `json:"api_key,omitempty"`
	EnableDecisionGate    bool    `json:"enable_decision_gate,omitempty"`
	EnableToolRouter      bool    `json:"enable_tool_router,omitempty"`
	EnableFailureClassify bool    `json:"enable_failure_classify,omitempty"`
	EnableSieve           bool    `json:"enable_sieve,omitempty"`
	MinConfidence         float64 `json:"min_confidence,omitempty"`
}

// HookConfig declares a single automated hook. Point determines when it fires;
// Script is the path to a .lua/.py/.js/.sh script; Filter optionally restricts
// which tasks/steps/roles trigger the hook.
type HookConfig struct {
	Point       string            `json:"point"`                 // chat_habit|chat_pre_turn|chat_post_turn|step_pre|step_post|task_completion|decision_required
	Script      string            `json:"script"`                // path to hook script
	SHA256      string            `json:"sha256,omitempty"`      // content hash; empty = warn but allow (backward compat)
	Permissions PluginPermissions `json:"permissions,omitempty"` // capability declaration
	TimeoutMs   int               `json:"timeout_ms,omitempty"`  // 0 = 10s default
	Filter      HookFilter        `json:"filter,omitempty"`
}

// HookFilter restricts when a hook fires. All conditions must match (AND).
type HookFilter struct {
	RoleID       string `json:"role_id,omitempty"`       // exact match
	TaskContains string `json:"task_contains,omitempty"` // case-insensitive substring
	TaskType     string `json:"task_type,omitempty"`     // matched against role BaseCapability
	StepIDGlob   string `json:"step_id_glob,omitempty"`  // glob match on step ID
}

// PluginPermissions declares what a plugin script is allowed to access.
// Enforced at execution time via env filtering and sandbox configuration.
type PluginPermissions struct {
	Network    []string `json:"network,omitempty"`    // allowed domains; empty = no network
	Filesystem []string `json:"filesystem,omitempty"` // "read:path/" or "write:path/"; empty = no fs
	EnvVars    []string `json:"env_vars,omitempty"`   // allowed env var names; empty = none
	Subprocess bool     `json:"subprocess,omitempty"` // allow spawning child processes
}

// PluginConfig is the unified declaration for a developer-supplied plugin script.
// Used by ExternalInterceptor, HookRunner, and ExternalScriptProvider.
// Supports backward-compatible unmarshaling from a plain string (script path).
type PluginConfig struct {
	Script      string            `json:"script"`                // path to script file
	Lang        string            `json:"lang,omitempty"`        // python|node|lua|custom; auto-detected if empty
	SHA256      string            `json:"sha256,omitempty"`      // content hash; empty = warn but allow (backward compat)
	Permissions PluginPermissions `json:"permissions,omitempty"` // capability declaration
	TimeoutMs   int               `json:"timeout_ms,omitempty"`  // 0 = 10s default
}

// UnmarshalJSON allows PluginConfig to be specified as either a plain string
// (backward compat: just the script path) or a full object with hash/permissions.
func (p *PluginConfig) UnmarshalJSON(data []byte) error {
	// Try string first (backward compat: plain path)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		p.Script = s
		return nil
	}
	// Fall back to full struct
	type alias PluginConfig
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = PluginConfig(a)
	return nil
}

// NamedAPIKey pairs a bearer token with an owner identity and tier.
// When APIKeys is configured, tieredBearerAuth resolves the presented token
// against this list and propagates OwnerID via tools.AuthOwnerKey.
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

// OutputDirOrDefault returns the configured output directory or "outputs" if unset.
func (s *SystemSettings) OutputDirOrDefault() string {
	if s != nil && s.OutputDir != "" {
		return s.OutputDir
	}
	return "outputs"
}

type NotificationRoute struct {
	Name       string   `json:"name"`
	TaskType   string   `json:"task_type,omitempty"`    // match role BaseCapability; "*" = all
	TaskIDGlob string   `json:"task_id_glob,omitempty"` // glob match on task ID ("invest_*")
	StepIDGlob string   `json:"step_id_glob,omitempty"` // glob match on step ID ("fetch_*")
	TriggerOn  []string `json:"trigger_on,omitempty"`   // empty = Tier 0 (task_completed + task_failed)
	TargetURL  string   `json:"target_url"`
	SecretEnv  string   `json:"secret_env"`
	Enabled    bool     `json:"enabled"` // per-route on/off; default false for safety
}

type NotificationConfig struct {
	Enabled         bool                `json:"enabled"`
	DefaultEndpoint string              `json:"default_endpoint"`
	Routes          []NotificationRoute `json:"routes"`
}

// BudgetGateConfig configures automatic task suspension when token usage
// exceeds a configured threshold. When Enabled is false (the default),
// no budget checking occurs and existing behavior is unchanged.
type BudgetGateConfig struct {
	Enabled       bool  `json:"enabled"`
	PerTaskTokens int64 `json:"per_task_tokens"` // 0 = no per-task limit
	PerRoleTokens int64 `json:"per_role_tokens"` // 0 = no per-role limit
	AutoResume    bool  `json:"auto_resume"`     // unused in v1; reserved for future
	WindowMinutes int   `json:"window_minutes"`  // unused in v1; 0 = lifetime cumulative
}

type TelemetryConfig struct {
	Enabled          bool   `json:"enabled"`
	SyncIntervalMins int    `json:"sync_interval_mins"`
	Endpoint         string `json:"endpoint"`
	DetailedFeedback bool   `json:"detailed_feedback"`
}

// ─── Root Config Types ───────────────────────────────────────────────────

type ExternalRuntimes struct {
	PythonPath string              `json:"python_path,omitempty"`
	NodePath   string              `json:"node_path,omitempty"`
	Runtimes   map[string][]string `json:"runtimes,omitempty"`
}

type Config struct {
	DefaultProvider          string                    `json:"default_provider"`
	DefaultEmbeddingProvider string                    `json:"default_embedding_provider,omitempty"`
	Providers                map[string]ProviderConfig `json:"providers"`
	Skills                   []Skill                   `json:"skills"`
	MCPs                     []MCPDef                  `json:"mcps"`
	Roles                    []Role                    `json:"roles"`
	RoleGroups               []RoleGroup               `json:"role_groups,omitempty"`
	SOPs                     []schemas.SOP             `json:"sops,omitempty"`
	ExternalRuntimes         ExternalRuntimes          `json:"external_runtimes"`
	System                   SystemSettings            `json:"system"`
	EnableDynamicRoleGen     bool                      `json:"enable_dynamic_role_gen"`
	EnableEphemeralRoleGen   bool                      `json:"enable_ephemeral_role_gen"`
	RequirePlanReview        bool                      `json:"require_plan_review"`
	SwarmModeEnabled         bool                      `json:"swarm_mode_enabled"`
	RoleCookbookSource       string                    `json:"role_cookbook_source,omitempty"`
	RoleCookbookSources      map[string]string         `json:"role_cookbook_sources,omitempty"`
	SkillsDir                string                    `json:"skills_dir,omitempty"`
	RolesDir                 string                    `json:"roles_dir,omitempty"`
	SplitLayout              *SplitLayout              `json:"_split_layout,omitempty"`
	// WALCheckpoint records the ID of the last WAL commit already reflected
	// in this snapshot (set automatically by persistLocked whenever a WAL is
	// active). On the next load, ReplaySince uses this to skip commits that
	// are already incorporated -- without it, a crash between a snapshot
	// write and the corresponding WAL clear would cause those commits to be
	// replayed a second time on restart, which is NOT safe for RFC 6902
	// "add"/"remove" ops on slices (insert/delete-by-index are non-idempotent).
	WALCheckpoint string `json:"wal_checkpoint,omitempty"`

	// StepPlan (ADDED 2026-10-10): configures automatic step-plan mode.
	// See docs/plan/STEP_PLAN_MODE_DESIGN.md §9.
	StepPlan StepPlanConfig `json:"step_plan,omitempty"`
}

// StepPlanConfig controls automatic step-plan mode activation.
// When EnableProbe is true, a System One provider is used to classify
// task complexity for steps in the "fuzzy zone" (keyword matching
// inconclusive). FallbackMode controls behavior when the probe is
// unavailable: "keyword" (default) or "off".
// RequireForComplexSteps auto-enables step plan for any step where
// DetermineComplexity returns true, even if the step didn't set
// EnableStepPlan explicitly.
type StepPlanConfig struct {
	EnableProbe            bool   `json:"enable_probe"`
	FallbackMode           string `json:"fallback_mode,omitempty"` // "keyword" (default) | "off"
	RequireForComplexSteps bool   `json:"require_for_complex_steps,omitempty"`
}
