package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/pkg/env"
	"github.com/daybeam/vortex/schemas"
)

type SecretProvider = schemas.SecretProvider

type EnvSecretProvider struct{}

func (s *EnvSecretProvider) GetSecret(key string) (string, error) {
	return os.Getenv(key), nil
}

func NewRegistry(configPath string) (*Registry, error) {
	r := &Registry{
		configPath:  configPath,
		Providers:   make(map[string]*ProviderConfig),
		Skills:      make(map[string]*Skill),
		MCPs:        make(map[string]*MCPDef),
		Roles:       make(map[string]*Role),
		SOPs:        make(map[string]*schemas.SOP),
		RoleGroups:  make(map[string]*RoleGroup),
		DynamicMCPs: make(map[string]*MCPDef),
		Secrets:     &EnvSecretProvider{},
	}
	if err := r.loadWithFallback(); err != nil {
		return nil, err
	}

	if os.Getenv("VORTEX_WAL_ENABLED") == "true" {
		walPath := r.configPath + ".wal"
		r.wal = NewWAL(walPath)
		// FIX (2026-09-07): replay only commits AFTER r.walCheckpoint (set from
		// the just-loaded snapshot's WALCheckpoint field), not the entire log.
		// Replaying everything unconditionally is unsafe if the previous process
		// crashed between a snapshot write (which already reflects the WAL up to
		// some point) and the matching wal.Clear() -- see wal.go's ReplaySince
		// doc comment for the full reasoning.
		if err := r.wal.ReplaySince(r, r.walCheckpoint); err != nil {
			log.Printf("[Registry] WAL Replay failed (%v). Quarantining...", err)
			badPath := walPath + ".bad"
			_ = os.Rename(walPath, badPath)
			r.wal = NewWAL(walPath)
		} else {
			r.compactor = NewCompactor(r)
		}
	}
	return r, nil
}

func (r *Registry) Load() error {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	return r.loadLocked()
}

func (r *Registry) LoadRaw() ([]byte, error) {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	return os.ReadFile(r.configPath)
}

func (r *Registry) loadWithFallback() error {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	return r.loadWithFallbackLocked()
}

func (r *Registry) loadWithFallbackLocked() error {
	bakPath := r.configPath + ".bak"
	if _, bakErr := os.Stat(bakPath); os.IsNotExist(bakErr) {
		info, _ := os.Stat(r.configPath)
		if info != nil && info.Size() > 0 {
			if data, readErr := os.ReadFile(r.configPath); readErr == nil {
				_ = os.WriteFile(bakPath, data, 0644)
			}
		}
	}

	loadErr := r.loadLocked()
	if loadErr == nil {
		return nil
	}

	errStr := loadErr.Error()
	isCorrupt := strings.Contains(errStr, "parsing config") ||
		strings.Contains(errStr, "invalid character") ||
		strings.Contains(errStr, "syntax error")

	if !isCorrupt {
		return loadErr
	}

	badPath := r.configPath + ".bad"
	_ = os.Rename(r.configPath, badPath)

	if _, err := os.Stat(bakPath); err == nil {
		if restoreErr := os.Rename(bakPath, r.configPath); restoreErr == nil {
			if retryErr := r.loadLocked(); retryErr == nil {
				return nil
			}
		}
	}

	r.bootstrapDefaultsLocked()
	return nil
}

func (r *Registry) loadLocked() error {
	data, err := os.ReadFile(r.configPath)
	if err != nil {
		r.bootstrapDefaultsLocked()
		return nil
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	if err := checkSplitLayoutVersion(cfg.SplitLayout); err != nil {
		return err
	}
	loadSplitFiles(r.configPath, cfg.SplitLayout, &cfg)
	r.setSplitLayout(cfg.SplitLayout)
	if info, statErr := os.Stat(r.configPath); statErr == nil {
		r.setLoadedSnapshot(info.ModTime(), info.Size())
	}

	r.DefaultProvider = cfg.DefaultProvider
	if r.DefaultProvider == "" {
		// FIX (2026-09-03): restored, dropped during the loader.go split.
		r.DefaultProvider = "default"
	}
	r.DefaultEmbeddingProvider = cfg.DefaultEmbeddingProvider
	r.ExternalRuntimes = cfg.ExternalRuntimes
	r.System = cfg.System
	r.applySystemDefaults()
	r.EnableDynamicRoleGen = cfg.EnableDynamicRoleGen
	r.EnableEphemeralRoleGen = cfg.EnableEphemeralRoleGen
	r.RequirePlanReview = cfg.RequirePlanReview
	r.SwarmModeEnabled = cfg.SwarmModeEnabled
	r.RoleCookbookSource = cfg.RoleCookbookSource
	r.RoleCookbookSources = cfg.RoleCookbookSources
	r.SkillsDir = cfg.SkillsDir
	r.RolesDir = cfg.RolesDir
	r.walCheckpoint = cfg.WALCheckpoint

	r.EnvCapabilities = env.DetectRuntimes()

	// Providers
	keepProviders := make(map[string]bool)
	for name, pc := range cfg.Providers {
		if name == "" {
			// FIX (2026-09-03): restored, dropped during the loader.go split --
			// skip empty-ID entries rather than letting a ghost provider into
			// the registry (the same "ghost ID" class this project has fixed
			// for roles/MCPs/etc. multiple times before, e.g. F1).
			continue
		}
		p := pc
		if p.PoolID == "" {
			p.PoolID = name
		}
		r.Providers[name] = &p
		keepProviders[name] = true
	}
	pruneStale(r.Providers, keepProviders)
	// FIX (2026-09-03): restored, dropped during the loader.go split -- if a
	// config file exists but ends up with zero providers (e.g. all removed),
	// fall back to bootstrap defaults rather than leaving Providers empty
	// with no safety net.
	if len(r.Providers) == 0 {
		r.bootstrapDefaultsLocked()
	}

	// Skills
	skillsDir := cfg.SkillsDir
	if skillsDir == "" {
		skillsDir = filepath.Join(ConfigDir(r.configPath), "workspace/skills/")
	}
	keepSkills := make(map[string]bool)
	files, _ := os.ReadDir(skillsDir)
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".json") {
			skillData, err := os.ReadFile(filepath.Join(skillsDir, file.Name()))
			if err == nil {
				var s Skill
				if err := json.Unmarshal(skillData, &s); err == nil {
					s.InitFilter()
					r.Skills[s.ID] = &s
					keepSkills[s.ID] = true
				}
			}
		}
	}
	for i := range cfg.Skills {
		s := cfg.Skills[i]
		s.InitFilter()
		r.Skills[s.ID] = &s
		keepSkills[s.ID] = true
	}
	pruneStale(r.Skills, keepSkills)

	// SOPs
	keepSOPs := make(map[string]bool)
	for i := range cfg.SOPs {
		s := cfg.SOPs[i]
		if s.ID == "" {
			continue // FIX (2026-09-03): restored empty-ID skip, dropped during the loader.go split
		}
		r.SOPs[s.ID] = &s
		keepSOPs[s.ID] = true
	}
	pruneStale(r.SOPs, keepSOPs)

	r.PromptOverrides = loadPromptOverrides(r.configPath)

	// MCPs
	keepMCPs := make(map[string]bool)
	for i := range cfg.MCPs {
		m := cfg.MCPs[i]
		if m.ID == "" {
			continue // FIX (2026-09-03): restored empty-ID skip, dropped during the loader.go split
		}
		m.InitFilter()
		r.MCPs[m.ID] = &m
		keepMCPs[m.ID] = true
	}
	pruneStale(r.MCPs, keepMCPs)

	// Roles
	rolesDir := cfg.RolesDir
	if rolesDir == "" {
		rolesDir = filepath.Join(ConfigDir(r.configPath), "workspace/roles/")
	}
	folderRoles, _ := loadFolderRoles(rolesDir)
	parsed := make(map[string]*Role)
	for i := range cfg.Roles {
		role := cfg.Roles[i]
		if role.ID == "" {
			continue // FIX (2026-09-03): restored empty-ID skip, dropped during the loader.go split
		}
		parsed[role.ID] = &role
	}
	for id, fr := range folderRoles {
		parsed[id] = fr
	}
	keepRoles := make(map[string]bool)
	for _, role := range parsed {
		if role.Extends != "" {
			if parent, ok := parsed[role.Extends]; ok {
				role.BoundSkills = mergeUnique(parent.BoundSkills, role.BoundSkills)
				role.BoundMCPBindings = mergeMCPBindings(parent.BoundMCPBindings, role.BoundMCPBindings)
			}
		}
		r.Roles[role.ID] = role
		keepRoles[role.ID] = true
	}
	pruneStale(r.Roles, keepRoles)

	// Prompt Governance (§4.1): config-time validation of Role.Instruction
	// length. Emits a Warning (not an error) for any role whose Instruction
	// exceeds MaxRoleInstructionChars — the runtime PromptBudgetEnforcer
	// handles structural compression, but this catches the problem at load
	// time so authors can fix the source.
	for _, w := range ValidateAllRoleInstructions(r.Roles) {
		log.Printf("[prompt-governance] WARNING: %s", w)
	}

	// RoleGroups
	keepRoleGroups := make(map[string]bool)
	for i := range cfg.RoleGroups {
		g := cfg.RoleGroups[i]
		if g.ID == "" {
			continue // FIX (2026-09-03): restored empty-ID skip, dropped during the loader.go split
		}
		r.RoleGroups[g.ID] = &g
		keepRoleGroups[g.ID] = true
	}
	pruneStale(r.RoleGroups, keepRoleGroups)

	return nil
}

func (r *Registry) StartCompactor(ctx context.Context) {
	if r.compactor != nil {
		go r.compactor.Start(ctx)
	}
}

func (r *Registry) StartWatcher(ctx context.Context) {
	go func() {
		info, err := os.Stat(r.configPath)
		var lastMod time.Time
		if err == nil {
			lastMod = info.ModTime()
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			info, err := os.Stat(r.configPath)
			if err == nil && info.ModTime().After(lastMod) {
				lastMod = info.ModTime()
				if err := r.loadWithFallback(); err != nil {
					log.Printf("CRITICAL: config auto-reload failed: %v", err)
				}
			}
		}
	}()
}

func (r *Registry) Persist() error {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	return r.persistLocked()
}

func (r *Registry) persistLocked() error {
	if err := r.checkNotStaleForPersist(); err != nil {
		return err
	}

	providers := make(map[string]ProviderConfig)
	for k, v := range r.Providers {
		providers[k] = *v
	}
	skills := make([]Skill, 0)
	for id, s := range r.Skills {
		if id == "" || s.ID == "" {
			continue // FIX (2026-09-03): restored ghost-entry filtering on write-back, dropped during the loader.go split
		}
		skills = append(skills, *s)
	}
	mcps := make([]MCPDef, 0)
	for id, m := range r.MCPs {
		if id == "" || m.ID == "" {
			continue
		}
		mcps = append(mcps, *m)
	}
	roles := make([]Role, 0)
	for id, role := range r.Roles {
		if id == "" || role.ID == "" {
			continue
		}
		roles = append(roles, *role)
	}
	roleGroups := make([]RoleGroup, 0)
	for id, g := range r.RoleGroups {
		if id == "" || g.ID == "" {
			continue
		}
		roleGroups = append(roleGroups, *g)
	}
	sops := make([]schemas.SOP, 0)
	for id, s := range r.SOPs {
		if id == "" || s.ID == "" {
			continue
		}
		sops = append(sops, *s)
	}

	// FIX (2026-09-07): record which WAL commit (if any) this snapshot
	// already reflects. r's in-memory state always includes every commit
	// appended so far (CommitOps applies to r synchronously before/alongside
	// the WAL append), so "the current last WAL commit" is always safe to
	// record as this snapshot's checkpoint -- see wal.go's ReplaySince.
	walCheckpoint := ""
	if r.wal != nil {
		if id, err := r.wal.LastCommitID(); err == nil {
			walCheckpoint = id
		}
	}

	cfg := Config{
		DefaultProvider:        r.DefaultProvider,
		Providers:              providers,
		Skills:                 skills,
		MCPs:                   mcps,
		Roles:                  roles,
		RoleGroups:             roleGroups,
		SOPs:                   sops,
		ExternalRuntimes:       r.ExternalRuntimes,
		System:                 r.System,
		EnableDynamicRoleGen:   r.EnableDynamicRoleGen,
		EnableEphemeralRoleGen: r.EnableEphemeralRoleGen,
		SwarmModeEnabled:       r.SwarmModeEnabled,
		RoleCookbookSource:     r.RoleCookbookSource,
		RoleCookbookSources:    r.RoleCookbookSources,
		SkillsDir:              r.SkillsDir,
		RolesDir:               r.RolesDir,
		WALCheckpoint:          walCheckpoint,
	}
	// NOTE: deliberately not writing r.walCheckpoint here -- persistLocked is
	// called under varying lock types (Persist() holds only an RLock;
	// PersistForceSnapshot/Compactor.Compact hold a full Lock), so mutating a
	// Registry field here would be unsafe when only an RLock is held. This is
	// fine: r.walCheckpoint is only ever consulted once, in NewRegistry right
	// after loadLocked runs (itself always under a full Lock), so there is no
	// same-process reader that needs this update to be reflected immediately.
	return r.persistAndSnapshot(cfg)
}

func (r *Registry) bootstrapDefaults() {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.bootstrapDefaultsLocked()
}

func (r *Registry) bootstrapDefaultsLocked() {
	r.DefaultProvider = "default"
	r.Providers["default"] = &ProviderConfig{
		Provider:  "anthropic",
		Model:     "claude-sonnet-4-20250514",
		APIKeyEnv: "ANTHROPIC_API_KEY",
		PoolID:    "default",
	}
	r.System = SystemSettings{
		ConfidenceThreshold:    0.7,
		MaxDecisionOutcomes:    2000,
		MaxContextKeep:         100,
		DefaultJITTTL:          3600,
		SandboxedMemoryMB:      256,
		SandboxedCPUSecs:       30,
		MaxLogSizeMB:           100,
		DefaultTaskTokenBudget: 200000,
	}
	r.EnableDynamicRoleGen = true
	r.EnableEphemeralRoleGen = true
	// FIX (2026-09-03): restored, dropped during the loader.go split -- without
	// this, EnableDynamicRoleGen/EnableEphemeralRoleGen being true by default
	// on a fresh install has no cookbook source to actually generate a role
	// from, reverting a fresh install to the pre-F16 "no cookbook source
	// configured" failure state that the 07-31 addendum specifically verified
	// fixed end-to-end.
	r.RoleCookbookSources = map[string]string{
		"claude":  "github:anthropics/anthropic-cookbook",
		"gemini":  "github:google-gemini/cookbook",
		"default": "github:anthropics/anthropic-cookbook",
	}
	r.registerStandardSkillsLocked()
}

func (r *Registry) applySystemDefaults() {
	if r.System.ConfidenceThreshold == 0 {
		r.System.ConfidenceThreshold = 0.7
	}
	if r.System.MaxDecisionOutcomes == 0 {
		r.System.MaxDecisionOutcomes = 2000
	}
	if r.System.MaxContextKeep == 0 {
		r.System.MaxContextKeep = 100
	}
	if r.System.DefaultJITTTL == 0 {
		r.System.DefaultJITTTL = 3600
	}
	if r.System.SandboxedMemoryMB == 0 {
		r.System.SandboxedMemoryMB = 256
	}
	if r.System.SandboxedCPUSecs == 0 {
		r.System.SandboxedCPUSecs = 30
	}
	// FIX (2026-09-03): restored, dropped during the loader.go split.
	if r.System.MaxLogSizeMB == 0 {
		r.System.MaxLogSizeMB = 100
	}
	if r.System.SwarmFallbackDelay == 0 {
		r.System.SwarmFallbackDelay = 10
	}
	if r.System.Bookmarks == nil {
		r.System.Bookmarks = []string{}
	}
	if r.System.ExternalInterceptors == nil {
		r.System.ExternalInterceptors = []string{}
	}
	if r.System.Telemetry.SyncIntervalMins == 0 {
		r.System.Telemetry.SyncIntervalMins = 60
	}

	// ── Environment Variable Overrides ────────────────────────────────
	// ADDED (2026-09-06): allow operators to toggle DelegationMode without
	// editing config.json. This is essential for SSE deployments where the
	// operator wants the Vortex to act as a "prompt compiler" that
	// returns pre-assembled system_prompt + user_prompt to the caller
	// (Main Agent / Gateway) rather than calling the LLM provider itself.
	// Env var takes precedence over config.json: setting it to "true" or
	// "1" forces delegation on; setting it to "false" or "0" forces it off
	// (useful for overriding a config that has it on). Unset = use config
	// value as-is.
	if v := os.Getenv("VORTEX_DELEGATION_MODE"); v != "" {
		r.System.DelegationMode = (v == "true" || v == "1")
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func detectFamily(modelID string) string {
	m := strings.ToLower(modelID)
	m = strings.TrimPrefix(m, "models/")
	m = strings.TrimPrefix(m, "google/")

	families := map[string]string{
		"claude":    "claude",
		"gpt":       "gpt",
		"gemini":    "gemini",
		"gemma":     "gemini",
		"llama":     "llama",
		"mistral":   "mistral",
		"qwen":      "qwen",
		"deepseek":  "deepseek",
		"sensenova": "gpt", // FIX (2026-09-03): restored, dropped during the loader.go split -- SenseNova uses GPT-style/OpenAI-compatible prompts
		"sentimes":  "gpt", // FIX (2026-09-03): restored, dropped during the loader.go split -- Sentimes uses GPT-style/OpenAI-compatible prompts
		"glm":       "gpt", // FIX (2026-09-03): restored, dropped during the loader.go split -- GLM often uses OpenAI-compatible structures
	}
	for prefix, family := range families {
		if strings.HasPrefix(m, prefix) {
			return family
		}
	}
	return "default"
}

func mergeUnique(base, extra []string) []string {
	seen := make(map[string]bool)
	for _, s := range base {
		seen[s] = true
	}
	result := append([]string{}, base...)
	for _, s := range extra {
		if !seen[s] {
			result = append(result, s)
			seen[s] = true
		}
	}
	return result
}

func mergeMCPBindings(base, extra []MCPBinding) []MCPBinding {
	seen := make(map[string]bool)
	result := append([]MCPBinding{}, base...)
	for _, b := range base {
		seen[b.MCPID] = true
	}
	for _, b := range extra {
		if !seen[b.MCPID] {
			result = append(result, b)
			seen[b.MCPID] = true
		}
	}
	return result
}

func loadFolderRoles(rolesDir string) (map[string]*Role, error) {
	roles := make(map[string]*Role)
	entries, err := os.ReadDir(rolesDir)
	if err != nil {
		// FIX (2026-09-03): restored the distinction (dropped during the
		// loader.go split) between "directory doesn't exist" (a normal,
		// silent no-op -- most installs never use folder-based roles) and a
		// genuine error (e.g. permissions), which should propagate rather
		// than be silently swallowed.
		if os.IsNotExist(err) {
			return roles, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		roleID := entry.Name()
		dirPath := filepath.Join(rolesDir, roleID)
		role := &Role{ID: roleID}
		metaPath := filepath.Join(dirPath, "role.json")
		if data, err := os.ReadFile(metaPath); err == nil {
			_ = json.Unmarshal(data, role)
		}
		// FIX (2026-09-03): restored agent.md/rules.md reading, dropped during
		// the loader.go split -- a folder-based role's Instruction/Rules
		// content was silently lost (role.json alone doesn't carry it), even
		// though these fields are exactly what motivated the folder-based
		// role format over the older single-JSON-file format in the first
		// place.
		agentPath := filepath.Join(dirPath, "agent.md")
		if data, err := os.ReadFile(agentPath); err == nil {
			role.Instruction = strings.TrimSpace(string(data))
		}
		rulesPath := filepath.Join(dirPath, "rules.md")
		if data, err := os.ReadFile(rulesPath); err == nil {
			role.Rules = strings.TrimSpace(string(data))
		}
		roles[roleID] = role
	}
	return roles, nil
}

func ConfigDir(configPath string) string {
	return filepath.Dir(configPath)
}

func (r *Registry) registerStandardSkillsLocked() {
	// FIX (2026-09-03): restored the real implementation, dropped during the
	// loader.go split -- this had become a literal no-op stub
	// ("// ... (Implementation shortened for brevity in this example)"),
	// meaning a fresh install (no existing config file, going through
	// bootstrapDefaultsLocked) registered zero standard skills.
	standardSkills := []*Skill{
		{
			ID:          "read_file",
			Name:        "Read File",
			Capability:  "code",
			Description: "Read content of a file from disk.",
			Implementations: map[string]SkillImplementation{
				"default": {
					SystemPrompt: "When you need to see the content of a file, use the available read_file or similar tool. Analyze the content carefully.",
				},
			},
		},
		{
			ID:          "code_search",
			Name:        "Code Search",
			Capability:  "code",
			Description: "Search for symbols or text across the project.",
			Implementations: map[string]SkillImplementation{
				"default": {
					SystemPrompt: "Use code search tools to find relevant files and definitions before making changes.",
				},
			},
		},
	}

	for _, s := range standardSkills {
		s.InitFilter()
		r.Skills[s.ID] = s
	}
}
