package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/daybeam/vortex/tools"
)

// maxTraceInteractions is the hard cap on tool interactions returned per step
// even when the caller asks for more. Protects the UI from unbounded payloads.
const maxTraceInteractions = 500

// maxTraceResultChars is the per-interaction result preview cap. Full output
// content remains available via GET /api/tasks/:id/steps/:stepId/result.
const maxTraceResultChars = 20000

// TaskTraceHandler handles GET /api/tasks/:id/trace — returns the per-step
// execution records (StepResult) including the tool-interaction trace: every
// tool call a step made, with its arguments (input) and result (output).
//
// Query params:
//   - limit: max tool interactions per step (default 100, hard cap 500)
//
// Steps whose result has not been persisted yet (still running, or failed
// before producing output) are listed under "without_result" instead.
func (h *APIHandler) TaskTraceHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.Scheduler == nil || h.App.TaskStore == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}

	// Parse path: /api/tasks/{taskID}/trace
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 || parts[4] != "trace" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	taskID := parts[3]

	if tools.DenyIfNotOwner(r.Context(), h.App, taskID) {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	status, ok := h.App.Scheduler.GetStatus(taskID, "full")
	if !ok {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	steps, _ := status["steps"].(map[string]any)

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxTraceInteractions {
		limit = maxTraceInteractions
	}

	trace := make(map[string]any, len(steps))
	var withoutResult []string
	for sid, sv := range steps {
		sd, _ := sv.(map[string]any)
		ref, _ := sd["result_ref"].(string)
		if ref == "" {
			withoutResult = append(withoutResult, sid)
			continue
		}
		res, err := h.App.TaskStore.GetByRef(r.Context(), ref)
		if err != nil || res == nil {
			withoutResult = append(withoutResult, sid)
			continue
		}
		trace[sid] = truncateStepResult(res, limit)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"task_id":        taskID,
		"steps":          trace,
		"without_result": withoutResult,
	})
}

// truncateStepResult bounds a StepResult for transport: caps the number of
// tool interactions and replaces oversized interaction results with a
// preview + _truncated marker (full content via the step result endpoint).
func truncateStepResult(res any, limit int) any {
	// Work on a shallow copy so we never mutate the stored record.
	b, err := json.Marshal(res)
	if err != nil {
		return res
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return res
	}
	rawTrace, _ := m["trace"].([]any)
	if len(rawTrace) > limit {
		m["trace"] = rawTrace[:limit]
		m["trace_truncated"] = true
		rawTrace = m["trace"].([]any)
	}
	for _, item := range rawTrace {
		inter, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if rb, err := json.Marshal(inter["result"]); err == nil && len(rb) > maxTraceResultChars {
			inter["result"] = map[string]any{
				"_truncated": true,
				"preview":    string(rb[:maxTraceResultChars]) + "…",
			}
		}
	}
	return m
}

// TaskLogsHandler handles GET /api/tasks/:id/logs — returns the task's
// execution event log (chronological, most recent last).
//
// Query params:
//   - limit: max events to return (default 200). The most recent events are
//     kept, matching the orchestrator_get_logs MCP tool's tail semantics.
func (h *APIHandler) TaskLogsHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.Scheduler == nil || h.App.Logger == nil {
		http.Error(w, "scheduler not available", http.StatusServiceUnavailable)
		return
	}

	// Parse path: /api/tasks/{taskID}/logs
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 || parts[4] != "logs" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	taskID := parts[3]

	if tools.DenyIfNotOwner(r.Context(), h.App, taskID) {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	if _, ok := h.App.Scheduler.GetStatus(taskID, "summary"); !ok {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	events, err := h.App.Logger.ReadTaskLogs(taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"task_id": taskID,
		"events":  events,
	})
}
