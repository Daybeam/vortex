package config

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/daybeam/vortex/pkg/filters"
	"github.com/daybeam/vortex/schemas"
)

func (p *ProviderConfig) ResolveAPIKey(sp schemas.SecretProvider) string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		if sp != nil {
			val, _ := sp.GetSecret(p.APIKeyEnv)
			return val
		}
		return os.Getenv(p.APIKeyEnv)
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

	Filter filters.ToolFilter `json:"-"`
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

type FewShotExample struct {
	UserIntent string         `json:"user_intent"`
	ToolCall   map[string]any `json:"tool_call"`
}

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
}

type MCPPlatformOverride struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Dir     string            `json:"dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

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
	MCPID        string   `json:"mcp_id"`
	AllowedTools []string `json:"allowed_tools"`
}

func (b *MCPBinding) IsRestricted() bool { return len(b.AllowedTools) > 0 }

type RoleFallback struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type Role struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	BaseCapability      string            `json:"base_capability"`
	BoundSkills         []string          `json:"bound_skills"`
	BoundMCPBindings    []MCPBinding      `json:"bound_mcp_bindings"`
	Requires            []string          `json:"requires,omitempty"`
	Optional            []string          `json:"optional,omitempty"`
	Generatable         bool              `json:"generatable,omitempty"`
	AllowDynamicSkills  bool              `json:"allow_dynamic_skills"`
	AllowDynamicMCPs    bool              `json:"allow_dynamic_mcps"`
	MaxAdditionalSkills int               `json:"max_additional_skills"`
	Provider            string            `json:"provider,omitempty"`
	Model               string            `json:"model,omitempty"`
	Fallbacks           []RoleFallback    `json:"fallbacks,omitempty"`
	DisableFallback     bool              `json:"disable_fallback,omitempty"`
	Instruction         string            `json:"instruction,omitempty"`
	Rules               string            `json:"rules,omitempty"`
	Extends             string            `json:"extends,omitempty"`
	Purpose             string            `json:"purpose,omitempty"`
	BestFor             string            `json:"best_for,omitempty"`
	Examples            []string          `json:"examples,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
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

type SystemSettings struct {
	ConfidenceThreshold       float64         `json:"confidence_threshold"`
	MaxDecisionOutcomes       int             `json:"max_decision_outcomes"`
	MaxContextKeep            int             `json:"max_context_keep"`
	MaxToolTurns              int             `json:"max_tool_turns,omitempty"`
	MaxSpawnDepth             int             `json:"max_spawn_depth,omitempty"`
	DefaultJITTTL             int             `json:"default_jit_ttl"`
	SandboxedMemoryMB         int             `json:"sandboxed_memory_mb"`
	SandboxedCPUSecs          int             `json:"sandboxed_cpu_secs"`
	MaxLogSizeMB              int             `json:"max_log_size_mb"`
	CookbookSyncIntervalHours int             `json:"cookbook_sync_interval_hours,omitempty"`
	CookbookCacheEnabled      bool            `json:"cookbook_cache_enabled,omitempty"`
	Bookmarks                 []string        `json:"bookmarks,omitempty"`
	ExternalInterceptors      []string        `json:"external_interceptors,omitempty"`
	SwarmFallbackDelay        int             `json:"swarm_fallback_delay,omitempty"`
	SwarmAgentCount           int             `json:"swarm_agent_count,omitempty"` // audit L1: was hardcoded 5
	DelegationMode            bool            `json:"delegation_mode,omitempty"`
	RefBasedHandoffThreshold  int64           `json:"ref_based_handoff_threshold,omitempty"`
	AntiSlop                  AntiSlopConfig  `json:"anti_slop,omitempty"`
	StagingEnabled            bool            `json:"staging_enabled,omitempty"`
	Telemetry                 TelemetryConfig `json:"telemetry,omitempty"`
	DefaultTaskTokenBudget    int64           `json:"default_task_token_budget,omitempty"`
	ExperienceGraphBudget     int             `json:"experience_graph_budget,omitempty"`
	StagingRetentionHours     int             `json:"staging_retention_hours,omitempty"`
	// MaxConcurrentSteps caps the number of steps that may execute in parallel
	// within a single task graph (audit finding M4). Defaults to 10 if unset.
	MaxConcurrentSteps      int           `json:"max_concurrent_steps,omitempty"`
	MaxTaskChars            int           `json:"max_task_chars,omitempty"`
	ToolRepetitionThreshold int           `json:"tool_repetition_threshold,omitempty"`
	Sandbox                 SandboxConfig `json:"sandbox,omitempty"`
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
	Retention             RetentionSettings  `json:"retention,omitempty"`
	// OutputDir overrides the default "outputs" directory for task output.
	// When empty (the default), "outputs" is used. Relative to project root.
	OutputDir string `json:"output_dir,omitempty"`
}

// OutputDirOrDefault returns the configured output directory or "outputs" if unset.
func (s *SystemSettings) OutputDirOrDefault() string {
	if s != nil && s.OutputDir != "" {
		return s.OutputDir
	}
	return "outputs"
}

type NotificationRoute struct {
	Name      string   `json:"name"`
	TaskType  string   `json:"task_type"`
	TriggerOn []string `json:"trigger_on"`
	TargetURL string   `json:"target_url"`
	SecretEnv string   `json:"secret_env"`
}

type NotificationConfig struct {
	Enabled         bool                `json:"enabled"`
	DefaultEndpoint string              `json:"default_endpoint"`
	Routes          []NotificationRoute `json:"routes"`
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
}
