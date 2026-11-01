package replay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Handler serves the read-only replay API.
type Handler struct {
	loader *Loader
}

// NewHandler builds a handler over the telemetry log directory and the task
// artifact directory.
func NewHandler(logDir, tasksDir string) *Handler {
	return &Handler{loader: NewLoader(logDir, tasksDir)}
}

// Register mounts the replay routes on a mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/replay/runs", h.RunsHandler)
	mux.HandleFunc("/api/replay/runs/", h.RunHandler)
}

// RunsHandler returns GET /api/replay/runs.
func (h *Handler) RunsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	days := queryInt(r, "days", 90, 1, 3650)
	limit := queryInt(r, "limit", 100, 1, 500)
	index, err := h.loader.Index(days, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, index)
}

// RunHandler returns GET /api/replay/runs/{task_id}.
func (h *Handler) RunHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	taskID := strings.TrimPrefix(r.URL.Path, "/api/replay/runs/")
	if taskID == "" || strings.Contains(taskID, "/") || !taskIDPattern.MatchString(taskID) {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	detail, err := h.loader.Detail(taskID)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, detail)
}

func queryInt(r *http.Request, key string, fallback, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
