package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ConfigHandler returns the current full configuration.
func (h *APIHandler) ConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// audit P-MED-6: Marshal under RLock produces a consistent byte snapshot.
	// All post-processing (unmarshal, masking, response encoding) is done
	// outside the lock to minimize hold time. Deep-copying the Registry's 7+
	// maps of pointers for marshal-outside-lock would add O(n) copy cost under
	// the lock with marginal benefit since RLock already allows concurrent readers.
	h.App.Registry.Mu.RLock()
	data, _ := json.Marshal(h.App.Registry)
	h.App.Registry.Mu.RUnlock()

	var cfg map[string]any
	json.Unmarshal(data, &cfg)

	// Registry struct has no JSON tags, so json.Marshal produces PascalCase
	// keys (Providers, MCPs, etc.). Frontend expects lowercase/snake_case.
	// Build a response with the correct key names.
	result := map[string]any{
		"default_provider":           cfg["DefaultProvider"],
		"default_embedding_provider": cfg["DefaultEmbeddingProvider"],
		"providers":                  cfg["Providers"],
		"skills":                     cfg["Skills"],
		"mcps":                       cfg["MCPs"],
		"roles":                      cfg["Roles"],
		"sops":                       cfg["SOPs"],
		"role_groups":                cfg["RoleGroups"],
		"system":                     cfg["System"],
		"swarm_mode_enabled":         cfg["SwarmModeEnabled"],
		"require_plan_review":        cfg["RequirePlanReview"],
		"enable_dynamic_role_gen":    cfg["EnableDynamicRoleGen"],
		"enable_ephemeral_role_gen":  cfg["EnableEphemeralRoleGen"],
		"provider_health":            cfg["ProviderHealth"],
	}

	// audit O24-3: this used to mask providers.*.api_key and nothing else, so
	// MCP env credentials, named API keys and the system tokens were returned
	// verbatim by an endpoint public-tier callers reach.
	config.RedactConfigSecrets(result)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// RolesHandler returns the current roles.
func (h *APIHandler) RolesHandler(w http.ResponseWriter, r *http.Request) {
	h.App.Registry.Mu.RLock()
	defer h.App.Registry.Mu.RUnlock()
	var roles []config.Role
	for _, r := range h.App.Registry.Roles {
		if r != nil {
			roles = append(roles, *r)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(roles)
}

// SkillsHandler returns the current skills.
func (h *APIHandler) SkillsHandler(w http.ResponseWriter, r *http.Request) {
	h.App.Registry.Mu.RLock()
	defer h.App.Registry.Mu.RUnlock()
	var skills []config.Skill
	for _, s := range h.App.Registry.Skills {
		if s != nil {
			skills = append(skills, *s)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(skills)
}

// UpdateRoleHandler handles /api/config/update-role (POST).
func (h *APIHandler) UpdateRoleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var role config.Role
	if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if role.ID == "" {
		http.Error(w, "Role ID is required", http.StatusBadRequest)
		return
	}

	roleJSON, _ := json.Marshal(role)
	ops := []config.PatchOp{
		{Op: config.OpAdd, Path: "/roles/" + role.ID, Value: json.RawMessage(roleJSON)},
	}

	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// UpdateSkillHandler handles /api/config/update-skill (POST).
func (h *APIHandler) UpdateSkillHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var sk config.Skill
	if err := json.NewDecoder(r.Body).Decode(&sk); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if sk.ID == "" {
		http.Error(w, "Skill ID is required", http.StatusBadRequest)
		return
	}

	// Determine the correct skills directory
	skillsDir := h.App.Registry.GetConfigPath()
	skillsDir = filepath.Join(filepath.Dir(skillsDir), "workspace/skills/")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create skills dir: %v", err), http.StatusInternalServerError)
		return
	}

	// Save skill to individual JSON file
	safeID := filepath.Base(sk.ID)
	skillPath := filepath.Join(skillsDir, safeID+".json")
	skillData, err := json.MarshalIndent(sk, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(skillPath, skillData, 0644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Update in-memory registry
	h.App.Registry.Mu.Lock()
	h.App.Registry.Skills[sk.ID] = &sk
	h.App.Registry.Mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// UpdateMCPHandler handles /api/config/update-mcp (POST).
func (h *APIHandler) UpdateMCPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var mcpDef config.MCPDef
	if err := json.NewDecoder(r.Body).Decode(&mcpDef); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if mcpDef.ID == "" || (mcpDef.URL == "" && mcpDef.Command == "") {
		http.Error(w, "MCP ID and at least one transport (url or command) are required", http.StatusBadRequest)
		return
	}

	mcpJSON, _ := json.Marshal(mcpDef)
	ops := []config.PatchOp{
		{Op: config.OpAdd, Path: "/mcps/" + mcpDef.ID, Value: json.RawMessage(mcpJSON)},
	}

	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// UpdateProviderHandler handles /api/config/update-provider (POST).
func (h *APIHandler) UpdateProviderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
		config.ProviderConfig
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.ProviderConfig.Provider == "" || req.ProviderConfig.Model == "" {
		http.Error(w, "Provider name, type, and model are required", http.StatusBadRequest)
		return
	}

	providerJSON, _ := json.Marshal(req.ProviderConfig)
	ops := []config.PatchOp{
		{Op: config.OpAdd, Path: "/providers/" + req.Name, Value: json.RawMessage(providerJSON)},
	}

	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// UpdateGroupHandler handles /api/config/update-group (POST).
func (h *APIHandler) UpdateGroupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var group config.RoleGroup
	if err := json.NewDecoder(r.Body).Decode(&group); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if group.ID == "" {
		http.Error(w, "Group ID is required", http.StatusBadRequest)
		return
	}

	groupJSON, _ := json.Marshal(group)
	ops := []config.PatchOp{
		{Op: config.OpAdd, Path: "/role_groups/" + group.ID, Value: json.RawMessage(groupJSON)},
	}

	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// UpdateSOPHandler handles /api/config/update-sop (POST).
func (h *APIHandler) UpdateSOPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var sop schemas.SOP
	if err := json.NewDecoder(r.Body).Decode(&sop); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if sop.ID == "" {
		http.Error(w, "SOP ID is required", http.StatusBadRequest)
		return
	}

	// Determine the correct sops directory
	sopsDir := h.App.Registry.GetConfigPath()
	sopsDir = filepath.Join(filepath.Dir(sopsDir), "workspace/sops/")
	if err := os.MkdirAll(sopsDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create sops dir: %v", err), http.StatusInternalServerError)
		return
	}

	// Save SOP to individual JSON file
	safeID := filepath.Base(sop.ID)
	sopPath := filepath.Join(sopsDir, safeID+".json")
	sopData, err := json.MarshalIndent(sop, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(sopPath, sopData, 0644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Update in-memory registry
	h.App.Registry.Mu.Lock()
	h.App.Registry.SOPs[sop.ID] = &sop
	h.App.Registry.Mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// UpdateSystemHandler handles /api/config/update-system (POST).
func (h *APIHandler) UpdateSystemHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		ConfidenceThreshold  float64               `json:"confidence_threshold"`
		MaxDecisionOutcomes  int                   `json:"max_decision_outcomes"`
		MaxContextKeep       int                   `json:"max_context_keep"`
		DefaultJITTTL        int                   `json:"default_jit_ttl"`
		SwarmModeEnabled     bool                  `json:"swarm_mode_enabled"`
		SwarmFallbackDelay   int                   `json:"swarm_fallback_delay"`
		Bookmarks            []string              `json:"bookmarks"`
		ExternalInterceptors []config.PluginConfig `json:"external_interceptors"`
		DelegationMode       bool                  `json:"delegation_mode"`
		RequirePlanReview    bool                  `json:"require_plan_review"`
		APIKeys              []config.NamedAPIKey  `json:"api_keys"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Unmarshalling into the struct above leaves every key that the caller
	// omitted at its zero value, so ops have to be keyed off the set of fields
	// actually present in the body. Regression (2026-08-21 on the MCP side,
	// still live here until now): patching unconditionally let a one-field POST
	// zero out every other production setting.
	var present map[string]json.RawMessage
	if err := json.Unmarshal(body, &present); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// O24-3 follow-up: named keys now have a supported write path (they were
	// hand-edit-only, and edits outside CommitOps are neither attributable nor
	// durable). This route sits on adminMux in the web edition, so public-tier
	// keys cannot reach it there; the desktop edition has one mux and treats
	// every accepted key as the operator (O24-2, pre-existing).
	_, writesKeys := present["api_keys"]
	if writesKeys {
		if err := validateNamedAPIKeys(req.APIKeys); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	var ops []config.PatchOp
	addField := func(key, path string, val any) {
		if _, ok := present[key]; !ok {
			return
		}
		b, _ := json.Marshal(val)
		ops = append(ops, config.PatchOp{Op: config.OpReplace, Path: path, Value: json.RawMessage(b)})
	}

	addField("confidence_threshold", "/system/confidence_threshold", req.ConfidenceThreshold)
	addField("max_decision_outcomes", "/system/max_decision_outcomes", req.MaxDecisionOutcomes)
	addField("max_context_keep", "/system/max_context_keep", req.MaxContextKeep)
	addField("default_jit_ttl", "/system/default_jit_ttl", req.DefaultJITTTL)
	addField("swarm_fallback_delay", "/system/swarm_fallback_delay", req.SwarmFallbackDelay)
	addField("bookmarks", "/system/bookmarks", req.Bookmarks)
	addField("external_interceptors", "/system/external_interceptors", req.ExternalInterceptors)
	addField("swarm_mode_enabled", "/swarm_mode_enabled", req.SwarmModeEnabled)
	addField("delegation_mode", "/system/delegation_mode", req.DelegationMode)
	addField("require_plan_review", "/require_plan_review", req.RequirePlanReview)
	addField("api_keys", "/system/api_keys", req.APIKeys)

	if len(ops) > 0 {
		if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	resp := map[string]any{"ok": true}
	if writesKeys {
		// Both auth middlewares snapshot the named-key slice when they are
		// built (main_web.go:741/750, auth_desktop.go:118) and keep no lock of
		// their own, so a key written now is durable but refused until restart.
		resp["restart_required"] = true
		resp["note"] = "named API keys are read at startup; restart the engine for the new set to be accepted"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// validateNamedAPIKeys rejects entries the middleware would silently ignore and
// the redaction sentinel, which is what a GET→POST round-trip sends back.
// Duplicate keys are refused because lookup is first-match.
func validateNamedAPIKeys(keys []config.NamedAPIKey) error {
	seen := make(map[string]bool, len(keys))
	for i, k := range keys {
		if k.Key == "" {
			return fmt.Errorf("api_keys[%d]: key is required", i)
		}
		if k.Key == config.RedactedSecret {
			return fmt.Errorf("api_keys[%d]: key is the placeholder GET /api/config returns; send the real key", i)
		}
		switch k.Tier {
		case "", "admin", "public":
		default:
			return fmt.Errorf("api_keys[%d]: tier must be \"admin\" or \"public\", got %q", i, k.Tier)
		}
		if seen[k.Key] {
			return fmt.Errorf("api_keys[%d]: duplicate key", i)
		}
		seen[k.Key] = true
	}
	return nil
}

// MCPsHandler handles GET /api/mcps — list all MCPs (static + dynamic).
func (h *APIHandler) MCPsHandler(w http.ResponseWriter, r *http.Request) {
	h.App.Registry.Mu.RLock()
	defer h.App.Registry.Mu.RUnlock()

	type mcpInfo struct {
		ID             string   `json:"id"`
		URL            string   `json:"url,omitempty"`
		Command        string   `json:"command,omitempty"`
		AvailableTools []string `json:"available_tools,omitempty"`
		Source         string   `json:"source"`
		Trusted        bool     `json:"trusted"`
		Domain         string   `json:"domain,omitempty"`
	}

	var mcps []mcpInfo
	for id, m := range h.App.Registry.MCPs {
		if m == nil {
			continue
		}
		mcps = append(mcps, mcpInfo{
			ID: id, URL: m.URL, Command: m.Command,
			AvailableTools: m.AvailableTools, Source: "static",
			Trusted: m.Trusted, Domain: m.Domain,
		})
	}
	for id, m := range h.App.Registry.DynamicMCPs {
		if m == nil {
			continue
		}
		mcps = append(mcps, mcpInfo{
			ID: id, URL: m.URL, Command: m.Command,
			AvailableTools: m.AvailableTools, Source: "dynamic",
			Trusted: m.Trusted, Domain: m.Domain,
		})
	}
	if mcps == nil {
		mcps = []mcpInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mcps)
}
