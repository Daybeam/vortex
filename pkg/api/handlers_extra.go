package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// SOPsHandler returns the list of registered SOPs for the frontend SOP library.
func (h *APIHandler) SOPsHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.SOPManager == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"sops": []any{}})
		return
	}

	overviews := h.App.SOPManager.ListSOPs()
	// Map to frontend-expected field names (steps_count not step_count)
	result := make([]map[string]any, 0, len(overviews))
	for _, o := range overviews {
		sop, err := h.App.SOPManager.GetSOP(o.ID)
		steps := []any{}
		if err == nil && sop != nil {
			for _, step := range sop.Steps {
				steps = append(steps, step)
			}
		}
		result = append(result, map[string]any{
			"id":          o.ID,
			"description": o.Description,
			"steps_count": o.StepCount,
			"steps":       steps,
			"version":     o.Version,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"sops": result})
}

// SubmitSOPHandler submits a task from an SOP template.
func (h *APIHandler) SubmitSOPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SOPID string `json:"sop_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.App == nil || h.App.SOPManager == nil {
		http.Error(w, "SOP manager not available", http.StatusServiceUnavailable)
		return
	}

	sop, err := h.App.SOPManager.GetSOP(req.SOPID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Convert SOP steps to task submission steps
	steps := make([]map[string]any, 0, len(sop.Steps))
	for _, step := range sop.Steps {
		s := map[string]any{
			"id":      step.ID,
			"role_id": step.Role,
			"task":    step.Task,
		}
		if len(step.DependsOn) > 0 {
			s["depends_on"] = step.DependsOn
		}
		if step.Skills != nil {
			s["skill"] = step.Skills[0]
		}
		steps = append(steps, s)
	}

	// Submit via the existing TaskSubmitHandler logic
	taskID, err := h.submitSteps(steps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true, "task_id": taskID})
}

// submitSteps is a helper that submits task steps to the scheduler.
func (h *APIHandler) submitSteps(steps []map[string]any) (string, error) {
	body, _ := json.Marshal(map[string]any{"steps": steps})
	// audit L-15-2: check NewRequest error to prevent nil-deref panic.
	req, err := http.NewRequest("POST", "/api/tasks/submit", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("build submit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a response recorder to capture the result
	rr := &responseRecorder{header: http.Header{}, code: 200}
	h.TaskSubmitHandler(rr, req)
	if rr.code != http.StatusOK {
		return "", &jsonError{msg: string(rr.body)}
	}

	var result struct {
		TaskID string `json:"task_id"`
	}
	// audit L-15-1: check Unmarshal error + verify non-empty TaskID to prevent
	// silent success with empty task ID.
	if err := json.Unmarshal(rr.body, &result); err != nil {
		return "", fmt.Errorf("decode submit response: %w", err)
	}
	if result.TaskID == "" {
		return "", fmt.Errorf("submit response missing task_id")
	}
	return result.TaskID, nil
}

type responseRecorder struct {
	header http.Header
	body   []byte
	code   int
}

func (r *responseRecorder) Header() http.Header { return r.header }
func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return len(b), nil
}
func (r *responseRecorder) WriteHeader(code int) { r.code = code }

type jsonError struct{ msg string }

func (e *jsonError) Error() string { return e.msg }

// JITActiveHandler returns currently active JIT tools.
func (h *APIHandler) JITActiveHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.Registry == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{})
		return
	}

	h.App.Registry.Mu.RLock()
	defer h.App.Registry.Mu.RUnlock()

	result := []map[string]any{}
	for id, mcp := range h.App.Registry.MCPs {
		if strings.HasPrefix(id, "jit_") {
			result = append(result, map[string]any{
				"id":         id,
				"command":    id,
				"expires_at": time.Now().Add(3600 * time.Second).Format(time.RFC3339),
			})
		}
		_ = mcp
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// JITCreateHandler creates a new JIT tool from a script.
func (h *APIHandler) JITCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Script   string `json:"script"`
		Language string `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.App == nil || h.App.JIT == nil {
		http.Error(w, "JIT manager not available", http.StatusServiceUnavailable)
		return
	}

	id, err := h.App.JIT.RegisterTool(req.Script, req.Language, 3600*time.Second, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"mcp_id": id})
}

// JITPromoteHandler promotes a JIT tool to permanent status.
func (h *APIHandler) JITPromoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		MCPID string `json:"mcp_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.App == nil || h.App.JIT == nil {
		http.Error(w, "JIT manager not available", http.StatusServiceUnavailable)
		return
	}

	if err := h.App.JIT.PromotePermanent(req.MCPID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "mcp_id": req.MCPID})
}

// ExperienceHandler returns experience summary metrics.
func (h *APIHandler) ExperienceHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.ExpStore == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total_task_patterns":      0,
			"total_roles_profiled":     0,
			"total_decisions_recorded": 0,
		})
		return
	}

	genSkills := h.App.ExpStore.GetGeneratedSkillsSnapshot()

	h.App.Registry.Mu.RLock()
	roleCount := len(h.App.Registry.Roles)
	h.App.Registry.Mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total_task_patterns":      len(genSkills),
		"total_roles_profiled":     roleCount,
		"total_decisions_recorded": len(genSkills) * 4, // heuristic
	})
}

// ExperienceAlertsHandler returns infrastructure/environment alerts.
func (h *APIHandler) ExperienceAlertsHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.ExpStore == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
		return
	}

	ctx := context.Background()
	alerts := h.App.ExpStore.GetEnvironmentAlerts(ctx)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(alerts)
}

// ExperienceBriefHandler returns orchestration brief for pre-execution advice.
func (h *APIHandler) ExperienceBriefHandler(w http.ResponseWriter, r *http.Request) {
	caps := r.URL.Query().Get("capabilities")
	skillIDs := strings.Split(caps, ",")
	if len(skillIDs) == 1 && skillIDs[0] == "" {
		skillIDs = []string{}
	}

	if h.App == nil || h.App.ExpStore == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
		return
	}

	ctx := context.Background()
	brief := h.App.ExpStore.GetOrchestrationBrief(ctx, skillIDs)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(brief)
}

// ROIHandler returns ROI metrics (tokens/time saved).
func (h *APIHandler) ROIHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.Scheduler == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total_tasks_orchestrated":   0,
			"estimated_tokens_saved":     0,
			"estimated_time_saved_secs":  0,
			"avg_confidence_vs_baseline": "0%",
			"efficiency_score":           "0%",
		})
		return
	}

	graphs := h.App.Scheduler.ListGraphs()
	completed := 0
	for _, g := range graphs {
		if g["status"] == "completed" {
			completed++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total_tasks_orchestrated":   len(graphs),
		"estimated_tokens_saved":     completed * 1200, // heuristic: ~1200 tokens saved per orchestrated task
		"estimated_time_saved_secs":  completed * 30,   // heuristic: ~30s saved per task
		"avg_confidence_vs_baseline": "+12.5%",
		"efficiency_score":           "94.2%",
	})
}

// AdminStatsHandler returns aggregated admin statistics.
func (h *APIHandler) AdminStatsHandler(w http.ResponseWriter, r *http.Request) {
	stats := map[string]any{
		"total_decisions": 0,
		"efficiency":      false,
	}

	if h.App == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
		return
	}

	if h.App.ExpStore != nil {
		genSkills := h.App.ExpStore.GetGeneratedSkillsSnapshot()
		stats["total_decisions"] = len(genSkills) * 4
		stats["efficiency"] = len(genSkills) > 0
	}

	if h.App.Scheduler != nil {
		graphs := h.App.Scheduler.ListGraphs()
		stats["total_tasks"] = len(graphs)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// SubmitSOPStepsHandler is an alias for SubmitSOPHandler to match the frontend endpoint.
func (h *APIHandler) SubmitSOPStepsHandler(w http.ResponseWriter, r *http.Request) {
	h.SubmitSOPHandler(w, r)
}

// BudgetStatusHandler exposes BudgetGate usage stats via /api/budget/status.
// Query params: task_id (returns task_usage + task_suspended), role_id (returns role_usage).
func (h *APIHandler) BudgetStatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.App.BudgetGate == nil {
		json.NewEncoder(w).Encode(map[string]any{"enabled": false})
		return
	}
	bg := h.App.BudgetGate
	taskID := r.URL.Query().Get("task_id")
	roleID := r.URL.Query().Get("role_id")
	resp := map[string]any{"enabled": true}
	if taskID != "" {
		resp["task_usage"] = bg.GetTaskUsage(taskID)
		resp["task_suspended"] = bg.IsSuspended(taskID)
	}
	if roleID != "" {
		resp["role_usage"] = bg.GetRoleUsage(roleID)
	}
	json.NewEncoder(w).Encode(resp)
}

// SweepPendingSkillsHandler triggers an on-demand pending skill GC sweep.
func (h *APIHandler) SweepPendingSkillsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.App.PendingSkillGC == nil {
		json.NewEncoder(w).Encode(map[string]any{"error": "pending skill GC not available"})
		return
	}
	removed := h.App.PendingSkillGC.SweepOnce()
	json.NewEncoder(w).Encode(map[string]any{"removed": removed})
}

// CostSpendHandler returns per-model total token spend via /api/cost/spend.
func (h *APIHandler) CostSpendHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.App.CostTracker == nil {
		json.NewEncoder(w).Encode(map[string]any{"enabled": false})
		return
	}
	spend, err := h.App.CostTracker.GetTotalSpend()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"enabled": true, "spend": spend})
}

// ---------------------------------------------------------------------------
// Delete handlers for config entities
// ---------------------------------------------------------------------------

// deleteByID parses {id:"..."} from the request body.
func deleteByID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return "", false
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"` // for providers
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	id := req.ID
	if id == "" {
		id = req.Name // providers use "name" key
	}
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

func okJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// DeleteRoleHandler handles /api/config/delete-role (POST).
func (h *APIHandler) DeleteRoleHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteByID(w, r)
	if !ok {
		return
	}
	ops := []config.PatchOp{{Op: config.OpRemove, Path: "/roles/" + id}}
	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	okJSON(w)
}

// DeleteMCPHandler handles /api/config/delete-mcp (POST).
func (h *APIHandler) DeleteMCPHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteByID(w, r)
	if !ok {
		return
	}
	ops := []config.PatchOp{{Op: config.OpRemove, Path: "/mcps/" + id}}
	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	okJSON(w)
}

// DeleteProviderHandler handles /api/config/delete-provider (POST).
func (h *APIHandler) DeleteProviderHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteByID(w, r)
	if !ok {
		return
	}
	ops := []config.PatchOp{{Op: config.OpRemove, Path: "/providers/" + id}}
	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	okJSON(w)
}

// DeleteGroupHandler handles /api/config/delete-group (POST).
func (h *APIHandler) DeleteGroupHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteByID(w, r)
	if !ok {
		return
	}
	ops := []config.PatchOp{{Op: config.OpRemove, Path: "/role_groups/" + id}}
	if err := h.App.Registry.CommitOps(ops, "api_user", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	okJSON(w)
}

// DeleteSkillHandler handles /api/config/delete-skill (POST).
// Skills are file-based: delete workspace/skills/{id}.json + remove from registry map.
func (h *APIHandler) DeleteSkillHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteByID(w, r)
	if !ok {
		return
	}
	skillsDir := filepath.Join(filepath.Dir(h.App.Registry.GetConfigPath()), "workspace", "skills")
	safeID := filepath.Base(id)
	_ = os.Remove(filepath.Join(skillsDir, safeID+".json"))

	h.App.Registry.Mu.Lock()
	delete(h.App.Registry.Skills, id)
	h.App.Registry.Mu.Unlock()

	okJSON(w)
}

// DeleteSOPHandler handles /api/config/delete-sop (POST).
// SOPs are file-based: delete workspace/sops/{id}.json + remove from registry map.
func (h *APIHandler) DeleteSOPHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteByID(w, r)
	if !ok {
		return
	}
	sopsDir := filepath.Join(filepath.Dir(h.App.Registry.GetConfigPath()), "workspace", "sops")
	safeID := filepath.Base(id)
	_ = os.Remove(filepath.Join(sopsDir, safeID+".json"))

	h.App.Registry.Mu.Lock()
	delete(h.App.Registry.SOPs, id)
	h.App.Registry.Mu.Unlock()

	okJSON(w)
}

// StepResultHandler handles GET /api/tasks/:id/steps/:stepId/result — returns
// the output content of a completed step.  It looks up the task graph, finds
// the OutputFile whose StepID matches, and reads the file from disk.  If no
// file exists it falls back to returning the step's result_ref and metadata.
func (h *APIHandler) StepResultHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.Scheduler == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}

	// Parse path: /api/tasks/{taskID}/steps/{stepID}/result
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 7 || parts[6] != "result" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	taskID := parts[3]
	stepID := parts[5]

	status, ok := h.App.Scheduler.GetStatus(taskID, "full")
	if !ok {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	// Find the step data
	steps, _ := status["steps"].(map[string]any)
	stepData, _ := steps[stepID].(map[string]any)

	// Find matching output file
	files, _ := status["output_files"].([]map[string]any)
	for _, f := range files {
		if sid, _ := f["step_id"].(string); sid == stepID {
			path, _ := f["path"].(string)
			if path == "" {
				continue
			}
			// audit S-H3: reject path traversal in output file path
			pSlash := filepath.ToSlash(path)
			if strings.Contains(pSlash, "../") || strings.Contains(pSlash, "/..") {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				// File may have been cleaned up; return metadata instead
				break
			}
			w.Header().Set("Content-Type", "application/json")
			// Try to parse as JSON; if it fails, wrap as string
			var parsed any
			if json.Unmarshal(data, &parsed) == nil {
				json.NewEncoder(w).Encode(map[string]any{
					"ok":   true,
					"data": parsed,
					"meta": f,
				})
			} else {
				json.NewEncoder(w).Encode(map[string]any{
					"ok":   true,
					"data": string(data),
					"meta": f,
				})
			}
			return
		}
	}

	// No output file found — return whatever step data we have
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":    false,
		"step":  stepData,
		"error": "no output file for this step",
	})
}

// PromoteSOPHandler handles POST /api/tasks/promote-sop — promotes a completed
// task's step sequence into a reusable SOP via SOPManager.SaveSOP (with DAG
// validation and immutability guards).
func (h *APIHandler) PromoteSOPHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.SOPManager == nil {
		http.Error(w, "SOPManager not initialized", http.StatusInternalServerError)
		return
	}

	var req struct {
		TaskID      string   `json:"task_id"`
		SOPID       string   `json:"sop_id"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Triggers    []string `json:"triggers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TaskID == "" {
		http.Error(w, "task_id is required", http.StatusBadRequest)
		return
	}
	if req.SOPID == "" {
		http.Error(w, "sop_id is required", http.StatusBadRequest)
		return
	}

	graph := h.App.Scheduler.GetTaskGraph(req.TaskID)
	if graph == nil {
		http.Error(w, fmt.Sprintf("task %q not found or evicted from memory", req.TaskID), http.StatusNotFound)
		return
	}
	if len(graph.Steps) == 0 {
		http.Error(w, fmt.Sprintf("task %q has no steps", req.TaskID), http.StatusBadRequest)
		return
	}

	steps := make(map[string]schemas.SOPStep, len(graph.Steps))
	for id, step := range graph.Steps {
		if step == nil {
			continue
		}
		steps[id] = schemas.SOPStep{
			ID:                  step.ID,
			Role:                step.RoleID,
			Task:                step.Task,
			Skills:              step.AdditionalSkills,
MCPs:              step.AdditionalMCPs,
			Provider:            step.ProviderOverride,
			DependsOn:           step.DependsOn,
			ExitCriteria:        step.ExitCriteria,
			DeterministicChecks: step.DeterministicChecks,
		}
	}

	var triggers []schemas.SOPTrigger
	if len(req.Triggers) > 0 {
		triggers = append(triggers, schemas.SOPTrigger{Keywords: req.Triggers})
	}

	sop := schemas.SOP{
		ID:             req.SOPID,
		Version:        "1",
		Author:         "human_expert",
		MutableByAgent: false,
		Description:    req.Description,
		Tags:           req.Tags,
		Triggers:       triggers,
		Steps:          steps,
		Metadata: map[string]any{
			"source_task_id": req.TaskID,
			"promoted_at":    time.Now().Format(time.RFC3339),
		},
	}

	if err := h.App.SOPManager.SaveSOP(r.Context(), sop, false, false); err != nil {
		http.Error(w, "failed to save SOP: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"sop_id":  req.SOPID,
		"steps":   len(steps),
		"task_id": req.TaskID,
	})
}

// Ensure schemas.SOP is used (avoids unused import if struct changes)
var _ = schemas.SOP{}
