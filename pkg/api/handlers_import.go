package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/config"
)

// ─── Skill Import Handler (ADDED 2026-09-19) ──────────────────────────────

// ImportSkillHandler handles POST /api/skills/import — bulk import skills from JSON.
func (h *APIHandler) ImportSkillHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var skills []config.Skill
	if err := json.NewDecoder(r.Body).Decode(&skills); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(skills) == 0 {
		http.Error(w, "no skills provided", http.StatusBadRequest)
		return
	}

	skillsDir := h.App.Registry.GetConfigPath()
	skillsDir = filepath.Join(filepath.Dir(skillsDir), "workspace/skills/")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create skills dir: %v", err), http.StatusInternalServerError)
		return
	}

	imported := 0
	for _, sk := range skills {
		if sk.ID == "" {
			continue
		}
		safeID := filepath.Base(sk.ID)
		if safeID != sk.ID || strings.Contains(sk.ID, "..") {
			continue
		}
		skillPath := filepath.Join(skillsDir, safeID+".json")
		skillData, err := json.MarshalIndent(sk, "", "  ")
		if err != nil {
			continue
		}
		if err := os.WriteFile(skillPath, skillData, 0644); err != nil {
			continue
		}
		h.App.Registry.Mu.Lock()
		h.App.Registry.Skills[sk.ID] = &sk
		h.App.Registry.Mu.Unlock()
		imported++
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"imported": imported})
}

// ─── MCP Import Handler (ADDED 2026-09-19) ────────────────────────────────

// ImportMCPHandler handles POST /api/mcps/import — bulk import MCPs from JSON.
func (h *APIHandler) ImportMCPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var mcps []config.MCPDef
	if err := json.NewDecoder(r.Body).Decode(&mcps); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(mcps) == 0 {
		http.Error(w, "no MCPs provided", http.StatusBadRequest)
		return
	}

	imported := 0
	for _, mcpDef := range mcps {
		if mcpDef.ID == "" || (mcpDef.URL == "" && mcpDef.Command == "") {
			continue
		}
		mcpJSON, _ := json.Marshal(mcpDef)
		ops := []config.PatchOp{
			{Op: config.OpAdd, Path: "/mcps/" + mcpDef.ID, Value: json.RawMessage(mcpJSON)},
		}
		if err := h.App.Registry.CommitOps(ops, "api_import", ""); err != nil {
			continue
		}
		imported++
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"imported": imported})
}
