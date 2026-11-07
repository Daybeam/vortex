package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// ObservabilityEventsHandler returns filtered log events for the observability panel.
// Query params:
//
//	event_type — filter by event type (e.g. "http_request", "provider_call_started", "tool_call")
//	task_id    — filter by task ID
//	date       — log date in YYYY-MM-DD format (defaults to today UTC)
//	limit      — max events to return (default 200, max 1000)
//	offset     — pagination offset
//
// GET /api/observability/events
func (h *APIHandler) ObservabilityEventsHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.Logger == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "error": "logger not available"})
		return
	}

	eventType := r.URL.Query().Get("event_type")
	taskID := r.URL.Query().Get("task_id")
	date := r.URL.Query().Get("date")

	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	events, err := h.App.Logger.ReadAllLogs(date, eventType, taskID, limit, offset)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"events": events,
		"count":  len(events),
		"filter": map[string]any{
			"event_type": eventType,
			"task_id":    taskID,
			"date":       date,
			"limit":      limit,
			"offset":     offset,
		},
	})
}

// ObservabilityStatsHandler returns summary stats for the observability panel.
// Counts events by type from today's logs.
// GET /api/observability/stats
func (h *APIHandler) ObservabilityStatsHandler(w http.ResponseWriter, r *http.Request) {
	if h.App == nil || h.App.Logger == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"http_requests":  0,
			"provider_calls": 0,
			"tool_calls":     0,
			"total_events":   0,
		})
		return
	}

	// Read all events (up to 1000) and count by type.
	allEvents, _ := h.App.Logger.ReadAllLogs("", "", "", 1000, 0)

	stats := map[string]any{
		"http_requests":  0,
		"provider_calls": 0,
		"tool_calls":     0,
		"total_events":   len(allEvents),
	}

	httpCount, providerCount, toolCount := 0, 0, 0
	for _, ev := range allEvents {
		evType, _ := ev["event"].(string)
		switch evType {
		case "http_request":
			httpCount++
		case "provider_call_started":
			providerCount++
		case "tool_call":
			toolCount++
		}
	}
	stats["http_requests"] = httpCount
	stats["provider_calls"] = providerCount
	stats["tool_calls"] = toolCount

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// QualityTrendHandler returns sliding-window success-rate trends per role.
// Query params:
//
//	role   — role ID to query (default "global")
//	window — time window (default "7d"; supports "1h", "24h", "7d", "30d")
//
// GET /api/observability/quality
func (h *APIHandler) QualityTrendHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.App == nil || h.App.QualityTracker == nil {
		json.NewEncoder(w).Encode(map[string]any{"error": "quality tracker not available"})
		return
	}
	role := r.URL.Query().Get("role")
	if role == "" {
		role = "global"
	}
	window := 7 * 24 * time.Hour // default 7d
	if w := r.URL.Query().Get("window"); w != "" {
		window = parseWindow(w, window)
	}
	trend := h.App.QualityTracker.GetTrend(role, window)
	json.NewEncoder(w).Encode(trend)
}

// parseWindow parses a duration string like "1h", "24h", "7d", "30d".
func parseWindow(s string, fallback time.Duration) time.Duration {
	if len(s) == 0 {
		return fallback
	}
	unit := s[len(s)-1]
	num, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || num <= 0 {
		return fallback
	}
	switch unit {
	case 'h':
		return time.Duration(num) * time.Hour
	case 'd':
		return time.Duration(num) * 24 * time.Hour
	default:
		return fallback
	}
}
