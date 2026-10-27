package config

import (
	"sync"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Registry is the in-memory, thread-safe representation of config.json.
type Registry struct {
	Mu                       sync.RWMutex
	configPath               string
	DefaultProvider          string
	DefaultEmbeddingProvider string
	Providers                map[string]*ProviderConfig
	Skills                   map[string]*Skill
	PendingSkills            map[string]*Skill // staging for unverified external skills (TODO-4: not discoverable until promoted)
	MCPs                     map[string]*MCPDef
	Roles                    map[string]*Role
	SOPs                     map[string]*schemas.SOP
	RoleGroups               map[string]*RoleGroup
	System                   SystemSettings
	ProviderHealth           map[string]*ProviderHealth
	ExternalRuntimes         ExternalRuntimes
	EnableDynamicRoleGen     bool
	EnableEphemeralRoleGen   bool
	RequirePlanReview        bool
	SwarmModeEnabled         bool
	StepPlan                 StepPlanConfig
	RoleCookbookSource       string
	RoleCookbookSources      map[string]string
	SkillSyncSources         map[string]string // trusted repos for periodic skill sync, e.g. {"general": "mattpocock/skills"}
	SkillsDir                string
	RolesDir                 string
	DynamicMCPs              map[string]*MCPDef // In-memory JIT tools
	Secrets                  SecretProvider
	EnvCapabilities          []string
	PromptOverrides          PromptOverrides

	splitLayout   *SplitLayout
	splitLayoutMu sync.RWMutex

	// reloadCallbacks are invoked after a successful config hot-reload
	// (loadWithFallback). Used to re-wire config-dependent components
	// like the System One reranker without restarting the process.
	reloadCallbacks []func()

	// envReloadHook is called by StartWatcher when the .env file changes,
	// before loadWithFallback. The hook should re-read .env into os.Environ
	// (e.g. loadDotEnvForReload). Must be set before StartWatcher starts.
	// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md §change 5.
	envReloadHook func()

	loadedModTime    time.Time
	loadedSize       int64
	loadedSnapshotMu sync.RWMutex

	wal           *WAL
	compactor     *Compactor
	walCheckpoint string // last WAL commit ID reflected in the loaded/persisted snapshot; see Config.WALCheckpoint
}

// OnReload registers a callback to be invoked after a successful config
// hot-reload. Callbacks run in registration order, without holding Mu
// (they must not mutate the registry directly). Safe to call before
// StartWatcher; callbacks fire on the watcher goroutine.
func (r *Registry) OnReload(f func()) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.reloadCallbacks = append(r.reloadCallbacks, f)
}

// SetEnvReloadHook sets a function called by StartWatcher when the .env
// file changes, before loadWithFallback. The hook should re-read .env
// into os.Environ (e.g. loadDotEnvForReload). Must be called before
// StartWatcher starts.
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md §change 5.
func (r *Registry) SetEnvReloadHook(hook func()) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.envReloadHook = hook
}

// ProviderHealth tracks the reliability of a specific provider.
type ProviderHealth struct {
	LastFailure     time.Time
	FailureCount    int
	CircuitOpened   bool
	CooldownUntil   time.Time
	WasCoolingDown  bool // tracks cooldown→recovery transition (admin lifecycle §3.2)
}

// GetWorkspacePolicy returns the WorkspacePolicy for the given owner.
// When per-owner policy exists, it merges allowed roots; otherwise returns
// the global policy. Returns nil when workspace policy is disabled.
func (r *Registry) GetWorkspacePolicy(ownerID string) *WorkspacePolicyConfig {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	if !r.System.WorkspacePolicy.Enabled {
		return nil
	}
	// Return a copy so callers can't mutate the registry's config.
	policy := r.System.WorkspacePolicy
	return &policy
}
