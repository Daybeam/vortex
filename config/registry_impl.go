package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (r *Registry) GetConfigPath() string {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	return r.configPath
}

func (r *Registry) RegisterDynamicMCP(mcp *MCPDef) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	if mcp.ToolFilter == nil {
		mcp.InitFilter()
	}
	r.DynamicMCPs[mcp.ID] = mcp
}

func (r *Registry) UnregisterDynamicMCP(id string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	delete(r.DynamicMCPs, id)
}

func (r *Registry) GetMCP(id string) *MCPDef {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	if mcp, ok := r.MCPs[id]; ok {
		return mcp
	}
	return r.DynamicMCPs[id]
}

// GetProvider returns the ProviderConfig for the given provider ID, or nil if
// not found. It is the locked accessor counterpart to direct r.Providers[...]
// reads — the Providers map is mutated in place by the config file-watcher
// reload (loader.go), so any concurrent unlocked read races and can fatal-
// panic with "concurrent map read and map write" (audit H1). All hot-path
// reads MUST go through here or ResolveProviderConfig.
func (r *Registry) GetProvider(id string) *ProviderConfig {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	return r.Providers[id]
}

// DefaultProviderName returns the configured default provider ID under the
// read lock. Use together with GetProvider instead of reading the exported
// DefaultProvider field directly from concurrent goroutines (audit H1).
func (r *Registry) DefaultProviderName() string {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	return r.DefaultProvider
}

func (r *Registry) HasEnvCapability(cap string) bool {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	for _, c := range r.EnvCapabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func (r *Registry) ResolveProviderConfig(role *Role) *ProviderConfig {
	r.Mu.RLock()
	defer r.Mu.RUnlock()

	providerName := role.Provider
	if providerName == "" {
		providerName = r.DefaultProvider
	}

	pc := r.Providers[providerName]
	if pc == nil {
		pc = r.Providers["default"]
	}
	if pc == nil {
		return &ProviderConfig{
			Provider:  "anthropic",
			Model:     "claude-sonnet-4-20250514",
			APIKeyEnv: "ANTHROPIC_API_KEY",
		}
	}

	if role.Model != "" && role.Model != pc.Model {
		clone := *pc
		clone.Model = role.Model
		return &clone
	}
	return pc
}

func (r *Registry) ResolveToolBindings(role *Role, extra []MCPBinding) []MCPBinding {
	effective := make(map[string]MCPBinding)
	for _, b := range role.BoundMCPBindings {
		effective[b.MCPID] = b
	}
	if role.AllowDynamicMCPs {
		r.Mu.RLock()
		for id := range r.DynamicMCPs {
			if _, exists := effective[id]; !exists {
				effective[id] = MCPBinding{MCPID: id, AllowedTools: []string{}}
			}
		}
		r.Mu.RUnlock()
	}

	for _, b := range extra {
		if _, exists := effective[b.MCPID]; !exists {
			effective[b.MCPID] = b
		}
	}
	result := make([]MCPBinding, 0, len(effective))
	for _, b := range effective {
		result = append(result, b)
	}
	return result
}

func (r *Registry) EstimateTokens(skillIDs []string) int {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	total := 0
	for _, id := range skillIDs {
		if s, ok := r.Skills[id]; ok {
			total += s.TokenEstimate
		}
	}
	return total
}

func (r *Registry) RecordFailure(pID string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if r.ProviderHealth == nil {
		r.ProviderHealth = make(map[string]*ProviderHealth)
	}

	h := r.ProviderHealth[pID]
	if h == nil {
		h = &ProviderHealth{}
		r.ProviderHealth[pID] = h
	}

	h.LastFailure = time.Now()
	h.FailureCount++

	if h.FailureCount >= 5 {
		h.CircuitOpened = true
	}
}

func (r *Registry) RecordSuccess(pID string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if r.ProviderHealth == nil {
		return
	}

	h := r.ProviderHealth[pID]
	if h != nil {
		h.FailureCount = 0
		h.CircuitOpened = false
		h.CooldownUntil = time.Time{}
	}
}

func (r *Registry) SetCooldown(pID string, duration time.Duration) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if r.ProviderHealth == nil {
		r.ProviderHealth = make(map[string]*ProviderHealth)
	}

	h := r.ProviderHealth[pID]
	if h == nil {
		h = &ProviderHealth{}
		r.ProviderHealth[pID] = h
	}
	h.CooldownUntil = time.Now().Add(duration)
}

func (r *Registry) IsCoolingDown(pID string) (bool, time.Duration) {
	r.Mu.RLock()
	defer r.Mu.RUnlock()

	if h, ok := r.ProviderHealth[pID]; ok {
		remaining := time.Until(h.CooldownUntil)
		if remaining > 0 {
			return true, remaining
		}
	}
	return false, 0
}

func (r *Registry) IsHealthy(pID string) bool {
	r.Mu.RLock()
	defer r.Mu.RUnlock()

	if r.ProviderHealth == nil {
		return true
	}

	h := r.ProviderHealth[pID]
	if h == nil || !h.CircuitOpened {
		return true
	}

	if time.Since(h.LastFailure) > 5*time.Minute {
		return true
	}

	return false
}

func (r *Registry) ResolveCookbookSource(modelID string, familyOverride ...string) string {
	family := ""
	if len(familyOverride) > 0 && familyOverride[0] != "" {
		family = familyOverride[0]
	} else {
		family = detectFamily(modelID)
	}

	if r.RoleCookbookSources != nil {
		if src, ok := r.RoleCookbookSources[family]; ok && src != "" {
			return src
		}
		if src, ok := r.RoleCookbookSources["default"]; ok && src != "" {
			return src
		}
	}
	return r.RoleCookbookSource
}

func (r *Registry) GetHealthyModelsSummary() []map[string]any {
	r.Mu.RLock()
	defer r.Mu.RUnlock()

	var summary []map[string]any
	for id, pc := range r.Providers {
		if pc == nil {
			continue
		}
		summary = append(summary, map[string]any{
			"id":           id,
			"provider":     pc.Provider,
			"model":        pc.Model,
			"capabilities": pc.Capabilities,
			"archetype":    pc.Archetype,
		})
	}
	return summary
}

func (r *Registry) ResolveCapabilities(caps []string) ([]string, []MCPBinding) {
	if len(caps) == 0 {
		return nil, nil
	}

	r.Mu.RLock()
	defer r.Mu.RUnlock()

	var skills []string
	mcpTools := make(map[string][]string)

	for _, reqCap := range caps {
		foundSkill := false
		for _, s := range r.Skills {
			if s.Capability == reqCap {
				skills = append(skills, s.ID)
				foundSkill = true
			}
		}
		if foundSkill {
			continue
		}

		for _, m := range r.MCPs {
			for _, tool := range m.AvailableTools {
				if tool == reqCap || fmt.Sprintf("%s.%s", m.ID, tool) == reqCap {
					mcpTools[m.ID] = append(mcpTools[m.ID], tool)
				}
			}
		}

		for _, m := range r.DynamicMCPs {
			for _, tool := range m.AvailableTools {
				if tool == reqCap || fmt.Sprintf("%s.%s", m.ID, tool) == reqCap {
					mcpTools[m.ID] = append(mcpTools[m.ID], tool)
				}
			}
		}
	}

	var bindings []MCPBinding
	for mcpID, tools := range mcpTools {
		seen := make(map[string]bool)
		var uniqueTools []string
		for _, t := range tools {
			if !seen[t] {
				seen[t] = true
				uniqueTools = append(uniqueTools, t)
			}
		}
		bindings = append(bindings, MCPBinding{
			MCPID:        mcpID,
			AllowedTools: uniqueTools,
		})
	}

	return skills, bindings
}

func (r *Registry) ResolveCapabilitiesToBindings(caps []string) []MCPBinding {
	if len(caps) == 0 {
		return nil
	}

	r.Mu.RLock()
	defer r.Mu.RUnlock()

	mcpMap := make(map[string]map[string]bool)

	for _, reqCap := range caps {
		for _, m := range r.MCPs {
			matched := false

			for _, p := range m.Provides {
				if p == reqCap {
					if mcpMap[m.ID] == nil {
						mcpMap[m.ID] = make(map[string]bool)
					}
					matched = true
					break
				}
			}

			if !matched {
				prefix := m.ID + "."
				if strings.HasPrefix(reqCap, prefix) {
					toolName := strings.TrimPrefix(reqCap, prefix)
					if mcpMap[m.ID] == nil {
						mcpMap[m.ID] = make(map[string]bool)
					}
					mcpMap[m.ID][toolName] = true
					matched = true
				}
			}

			if !matched {
				for _, t := range m.AvailableTools {
					if t == reqCap {
						if mcpMap[m.ID] == nil {
							mcpMap[m.ID] = make(map[string]bool)
						}
						mcpMap[m.ID][t] = true
						matched = true
						break
					}
				}
				if !matched {
					for _, tDef := range m.FullToolDefinitions {
						if tDef.Name == reqCap {
							if mcpMap[m.ID] == nil {
								mcpMap[m.ID] = make(map[string]bool)
							}
							mcpMap[m.ID][tDef.Name] = true
							matched = true
							break
						}
					}
				}
			}
		}

		for _, m := range r.DynamicMCPs {
			for _, t := range m.AvailableTools {
				if t == reqCap {
					if mcpMap[m.ID] == nil {
						mcpMap[m.ID] = make(map[string]bool)
					}
					mcpMap[m.ID][t] = true
					break
				}
			}
		}
	}

	var result []MCPBinding
	for mcpID, tools := range mcpMap {
		binding := MCPBinding{MCPID: mcpID}
		if len(tools) > 0 {
			for t := range tools {
				binding.AllowedTools = append(binding.AllowedTools, t)
			}
			sort.Strings(binding.AllowedTools)
		}
		result = append(result, binding)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].MCPID < result[j].MCPID
	})

	return result
}

func (r *Registry) CheckCapabilities(caps []string) []string {
	if len(caps) == 0 {
		return nil
	}

	r.Mu.RLock()
	defer r.Mu.RUnlock()

	var missing []string
	for _, reqCap := range caps {
		found := false
		for _, s := range r.Skills {
			if s.Capability == reqCap {
				found = true
				break
			}
		}
		if found {
			continue
		}

		for _, m := range r.MCPs {
			for _, p := range m.Provides {
				if p == reqCap {
					found = true
					break
				}
			}
			if found {
				break
			}
			for _, tool := range m.AvailableTools {
				if tool == reqCap || fmt.Sprintf("%s.%s", m.ID, tool) == reqCap {
					found = true
					break
				}
			}
			if found {
				break
			}
		}

		if !found {
			for _, m := range r.DynamicMCPs {
				for _, tool := range m.AvailableTools {
					if tool == reqCap || fmt.Sprintf("%s.%s", m.ID, tool) == reqCap {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
		}

		if !found {
			missing = append(missing, reqCap)
		}
	}
	return missing
}

func (r *Registry) GetVerifierID(toolName string) string {
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	return ""
}

func (r *Registry) CommitOps(ops []PatchOp, actor, taskID string) error {
	r.Mu.Lock()
	if err := ApplyPatch(r, ops); err != nil {
		r.Mu.Unlock()
		return fmt.Errorf("registry: failed to apply patch: %w", err)
	}
	if r.wal != nil {
		if _, err := r.wal.Append(actor, taskID, ops); err != nil {
			r.Mu.Unlock()
			return fmt.Errorf("registry: failed to append to WAL: %w", err)
		}
		r.Mu.Unlock()
	} else {
		r.Mu.Unlock()
		return r.Persist()
	}
	return nil
}

func (r *Registry) PersistForceSnapshot() error {
	if err := r.persistLocked(); err != nil {
		return err
	}
	if r.wal != nil {
		return r.wal.Clear()
	}
	return nil
}
