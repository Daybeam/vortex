package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/tools"
)

// ─── Schedule Management Handlers (ADDED 2026-09-19) ──────────────────────

// SchedulesHandler handles GET /api/schedules — list all schedules.
func (h *APIHandler) SchedulesHandler(w http.ResponseWriter, r *http.Request) {
	if h.App.SStore == nil {
		http.Error(w, "schedule store not available", http.StatusServiceUnavailable)
		return
	}
	schedules := h.App.SStore.ListVisible(tools.CallerOwnerID(r.Context()), tools.ResourceScopeIsUnrestricted(r.Context(), h.App))
	if schedules == nil {
		schedules = []*schemas.Schedule{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(schedules)
}

// ScheduleInboxHandler exposes durable fallbacks without claiming or removing
// them. Authenticated owners see only their own entries; ownerless local mode
// sees the full operator inbox.
func (h *APIHandler) ScheduleInboxHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App == nil || h.App.Schedule == nil {
		http.Error(w, "schedule manager not available", http.StatusServiceUnavailable)
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 100
	}
	entries, err := h.App.Schedule.ListPendingInboxForScope(r.Context(), tools.CallerOwnerID(r.Context()), tools.ResourceScopeIsUnrestricted(r.Context(), h.App), limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read schedule inbox: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"items": entries})
}

// ScheduleCreateHandler handles POST /api/schedules/create.
func (h *APIHandler) ScheduleCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.SStore == nil {
		http.Error(w, "schedule store not available", http.StatusServiceUnavailable)
		return
	}

	var s schemas.Schedule
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Name == "" {
		http.Error(w, "schedule name is required", http.StatusBadRequest)
		return
	}
	if len(s.TaskInputs) == 0 {
		http.Error(w, "task_inputs is required", http.StatusBadRequest)
		return
	}
	if err := schemas.ValidateDeliverTarget(s.Deliver); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Deliver.Mode == "session_current" && s.Deliver.OnMissing == "" {
		s.Deliver.OnMissing = "inbox"
	}
	s.ID = "sched_" + uuid.New().String()[:8]
	s.Status = schemas.ScheduleActive
	owner, err := tools.RequireResourceOwner(r.Context(), h.App)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	s.OwnerID = owner
	s.SubmitterIsAdmin = tools.EffectiveTier(r.Context(), h.App) == tools.TierAdmin
	s.CreatedAt = time.Now()
	if s.NextRun.IsZero() {
		s.NextRun = time.Now().Add(1 * time.Minute)
	}
	if err := h.App.SStore.Set(&s); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	if err := h.App.SStore.Save(); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": s.ID})
}

// SchedulePauseHandler handles POST /api/schedules/pause — toggle pause/resume.
func (h *APIHandler) SchedulePauseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.SStore == nil {
		http.Error(w, "schedule store not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s := tools.GetVisibleSchedule(r.Context(), h.App, req.ID)
	if s == nil {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	if s.Status == schemas.ScheduleActive {
		s.Status = schemas.SchedulePaused
	} else if s.Status == schemas.SchedulePaused {
		s.Status = schemas.ScheduleActive
	}
	if err := h.App.SStore.Set(s); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	if err := h.App.SStore.Save(); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": s.Status})
}

// ScheduleDeleteHandler handles POST /api/schedules/delete.
func (h *APIHandler) ScheduleDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.SStore == nil {
		http.Error(w, "schedule store not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s := tools.GetVisibleSchedule(r.Context(), h.App, req.ID)
	if s == nil {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	s.Status = schemas.ScheduleCompleted
	if err := h.App.SStore.Set(s); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	if err := h.App.SStore.Save(); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// ScheduleUpdateHandler handles POST /api/schedules/update — update an existing schedule.
func (h *APIHandler) ScheduleUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.SStore == nil {
		http.Error(w, "schedule store not available", http.StatusServiceUnavailable)
		return
	}

	var s schemas.Schedule
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.ID == "" {
		http.Error(w, "schedule id is required", http.StatusBadRequest)
		return
	}
	existing := tools.GetVisibleSchedule(r.Context(), h.App, s.ID)
	if existing == nil {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	if err := schemas.ValidateDeliverTarget(s.Deliver); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Deliver.Mode == "session_current" && s.Deliver.OnMissing == "" {
		s.Deliver.OnMissing = "inbox"
	}
	// Preserve runtime state that shouldn't be overwritten from the edit form.
	createdAt := existing.CreatedAt
	status := existing.Status
	runCount := existing.RunCount
	lastRun := existing.LastRun
	nextRun := existing.NextRun

	// Apply updates
	existing.Name = s.Name
	existing.Type = s.Type
	existing.RunAt = s.RunAt
	existing.Interval = s.Interval
	existing.CronExpr = s.CronExpr
	if len(s.TaskInputs) > 0 {
		existing.TaskInputs = s.TaskInputs
	}
	existing.OnFailure = s.OnFailure
	existing.MaxRuns = s.MaxRuns
	existing.Deliver = s.Deliver

	// Restore preserved fields
	existing.CreatedAt = createdAt
	existing.Status = status
	existing.RunCount = runCount
	existing.LastRun = lastRun
	existing.NextRun = nextRun

	if err := h.App.SStore.Set(existing); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	if err := h.App.SStore.Save(); err != nil {
		http.Error(w, fmt.Sprintf("failed to save: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": existing.ID})
}

// ScheduleRunNowHandler handles POST /api/schedules/run — manually trigger a
// schedule outside its cron/interval window. Returns the submitted task ID.
func (h *APIHandler) ScheduleRunNowHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.App.Schedule == nil {
		http.Error(w, "schedule manager not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "schedule id is required", http.StatusBadRequest)
		return
	}

	taskID, err := h.App.Schedule.RunNowForOwner(req.ID, tools.CallerOwnerID(r.Context()), tools.ResourceScopeIsUnrestricted(r.Context(), h.App))
	if err != nil {
		if tools.GetVisibleSchedule(r.Context(), h.App, req.ID) == nil {
			http.Error(w, "schedule not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to run schedule: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "task_id": taskID})
}
