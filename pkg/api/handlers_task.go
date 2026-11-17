package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/tools"
)

// TaskCancelHandler handles POST /api/tasks/cancel — cooperatively cancels a running task.
func (h *APIHandler) TaskCancelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.Scheduler == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.App.Scheduler.CancelTask(req.TaskID); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "cancelled"})
}

// TaskResumeHandler handles POST /api/tasks/resume — resumes a task.
// If the task is suspended (GraphSuspended), it resumes execution via ResumeSuspendedTask.
// If the task is blocked (GraphBlocked), it resolves the pending decision with "resume_more_turns".
func (h *APIHandler) TaskResumeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.Scheduler == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	status, ok := h.App.Scheduler.GetStatus(req.TaskID, "full")
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "task not found"})
		return
	}

	// Branch: if the task is suspended, resume from suspension.
	if graphStatus, _ := status["status"].(string); graphStatus == string(schemas.GraphSuspended) {
		if err := h.App.Scheduler.ResumeSuspendedTask(req.TaskID); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "resumed"})
		return
	}

	// Default: resume a blocked task by resolving its pending decision.
	decisions, _ := status["pending_decisions"].([]map[string]any)
	if len(decisions) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no pending decisions to resume"})
		return
	}
	decID, _ := decisions[0]["id"].(string)
	if decID == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "decision has no id"})
		return
	}
	if err := h.App.Scheduler.SubmitDecision(req.TaskID, decID, "resume_more_turns"); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "resumed"})
}

// TaskSuspendHandler handles POST /api/tasks/suspend — suspends a running task.
// This is a Control Plane operation (human/system-facing), NOT exposed to LLMs.
func (h *APIHandler) TaskSuspendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.Scheduler == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.App.Scheduler.SuspendTask(req.TaskID); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "suspended"})
}

// TaskSubmitHandler handles POST /api/tasks/submit — submits a task graph for execution.
func (h *APIHandler) TaskSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.Scheduler == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Steps         []schemas.StepInput `json:"steps"`
		Task          string              `json:"task"`
		SessionID     string              `json:"session_id"`
		WorkspaceRoot string              `json:"workspace_root"`
		InputDir      string              `json:"input_dir"`
		Timeout       int                 `json:"timeout"`
		TokenBudget   int64               `json:"token_budget"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Ported logic from orchestrator_submit_task MCP tool
	inputs := req.Steps
	if len(inputs) == 0 && req.Task != "" {
		inputs = []schemas.StepInput{{
			ID:       "s1",
			Task:     req.Task,
			InputDir: req.InputDir, // attachments seeded via input_dir (see seedTaskInputs)
		}}
	}

	if len(inputs) == 0 {
		http.Error(w, "steps or task is required", http.StatusBadRequest)
		return
	}

	id, err := h.App.Scheduler.SubmitWithSessionIR(inputs, nil, nil, nil, "", req.SessionID, req.WorkspaceRoot, req.Timeout, req.TokenBudget, tools.CallerIdentity(r.Context()))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error(), "draft_id": id})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"task_id": id,
		"status":  "submitted",
	})
}

// TaskDecisionHandler handles POST /api/tasks/:id/decision — submits a human-in-the-loop decision.
func (h *APIHandler) TaskDecisionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract TaskID from path /api/tasks/:id/decision
	pathParts := strings.Split(r.URL.Path, "/")
	if len(pathParts) < 4 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	taskID := pathParts[3]

	var req struct {
		DecisionID string `json:"decision_id"`
		Choice     string `json:"choice"`
		Output     string `json:"output"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Q4-5's routing rule, shared with the MCP tool (register O41): before this,
	// a mere non-empty `output` sent the call down FulfillStep, which only answers
	// delegations — so a payload-bearing retry of a step_failed decision came back
	// with a delegation error, and the same choice on the same decision resolved
	// differently depending on which transport carried it. The admin resolver in
	// the else-branch is untouched (c8e0fb5a4): who may answer a decision is a
	// separate question from which engine entry point answers it.
	toFulfill, outputIgnored := tools.ResolveDecision(h.App.Scheduler, taskID, req.DecisionID, req.Choice, req.Output)
	if toFulfill {
		if req.Output == "" {
			http.Error(w, "choice='fulfill' or output payload requires 'output' field with the result JSON", http.StatusBadRequest)
			return
		}
		if err := h.App.Scheduler.FulfillStep(taskID, req.DecisionID, req.Output); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		if err := h.App.Scheduler.SubmitDecisionWithResolver(taskID, req.DecisionID, req.Choice, req.Output, core.Resolver{IsAdmin: true, Channel: "web"}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	resp := map[string]any{"ok": true}
	if outputIgnored {
		// Parity with orchestrator_submit_decision: the drop is reported, not silent.
		resp["output_ignored"] = true
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// PublicTaskDecisionHandler handles POST /api/tasks/:id/decision from a
// public-tier (non-admin) caller — the task OWNER self-approving their own
// owner-level human_approval_required decision via the web UI. Only
// approve/reject are accepted; admin-level decisions are refused by the engine
// check (checkApprovalAuthority). Registered on publicMux (Stage 4, 2026-10-24).
func (h *APIHandler) PublicTaskDecisionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/decision") {
		http.NotFound(w, r)
		return
	}
	pathParts := strings.Split(r.URL.Path, "/")
	if len(pathParts) < 4 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	taskID := pathParts[3]
	var req struct {
		DecisionID string `json:"decision_id"`
		Choice     string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Choice != "approve" && req.Choice != "reject" {
		http.Error(w, `only "approve" or "reject" are accepted from the public endpoint`, http.StatusBadRequest)
		return
	}
	if err := h.App.Scheduler.SubmitDecisionWithResolver(taskID, req.DecisionID, req.Choice, "", core.Resolver{IsOwner: true, Channel: "web"}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
