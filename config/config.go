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

	loadedModTime    time.Time
	loadedSize       int64
	loadedSnapshotMu sync.RWMutex

	wal           *WAL
	compactor     *Compactor
	walCheckpoint string // last WAL commit ID reflected in the loaded/persisted snapshot; see Config.WALCheckpoint
}

// ProviderHealth tracks the reliability of a specific provider.
type ProviderHealth struct {
	LastFailure   time.Time
	FailureCount  int
	CircuitOpened bool
	CooldownUntil time.Time
}
