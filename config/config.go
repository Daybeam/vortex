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
	RoleCookbookSource       string
	RoleCookbookSources      map[string]string
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

// ProviderHealth tracks the reliability of a specific provider.
type ProviderHealth struct {
	LastFailure   time.Time
	FailureCount  int
	CircuitOpened bool
	CooldownUntil time.Time
}
