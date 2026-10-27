package config

import "github.com/daybeam/vortex/schemas"

type NotificationRoute struct {
	Name       string   `json:"name"`
	TaskType   string   `json:"task_type,omitempty"`    // match role BaseCapability; "*" = all
	TaskIDGlob string   `json:"task_id_glob,omitempty"` // glob match on task ID ("invest_*")
	StepIDGlob string   `json:"step_id_glob,omitempty"` // glob match on step ID ("fetch_*")
	TriggerOn  []string `json:"trigger_on,omitempty"`   // empty = Tier 0 (task_completed + task_failed)
	TargetURL  string   `json:"target_url"`
	SecretEnv  string   `json:"secret_env"`
	Enabled    bool     `json:"enabled"` // per-route on/off; default false for safety
	Channel    string   `json:"channel,omitempty"` // "" | "webhook" (default) | "telegram" | …
	ChatID     string   `json:"chat_id,omitempty"` // telegram chat ID (Channel="telegram")
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
