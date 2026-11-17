package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/daybeam/vortex/store"
)

// MemoryBankHandler handles GET/POST /api/memory-bank.
// GET returns aggregated MemoryBank data.
// POST accepts a partial patch and merges it into the existing MemoryBank.
// ADDED POST 2026-10-17. See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 2.
func (h *APIHandler) MemoryBankHandler(w http.ResponseWriter, r *http.Request) {
	if h.App.MemoryBankStore == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "MemoryBankStore not initialized"})
		return
	}

	if r.Method == http.MethodGet {
		mb, err := h.App.MemoryBankStore.Load()
		if err != nil {
			// Return empty structure instead of error — memory bank may not exist yet.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mb)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mb)
		return
	}

	if r.Method == http.MethodPost {
		// Patch: only fields present in the JSON body are overwritten.
		var patch struct {
			ActiveContext  *store.ActiveContext `json:"active_context,omitempty"`
			Decisions      *[]store.Decision    `json:"decisions,omitempty"`
			ProductContext *string              `json:"product_context,omitempty"`
			Progress       *store.Progress      `json:"progress,omitempty"`
			SystemPatterns *[]string            `json:"system_patterns,omitempty"`
			UserProfile    *store.UserProfile   `json:"user_profile,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Load existing → apply patch → Save
		mb, _ := h.App.MemoryBankStore.Load()
		if mb == nil {
			mb = &store.MemoryBank{}
		}
		if patch.ActiveContext != nil {
			mb.ActiveContext = *patch.ActiveContext
		}
		if patch.Decisions != nil {
			mb.Decisions = *patch.Decisions
		}
		if patch.ProductContext != nil {
			mb.ProductContext = *patch.ProductContext
		}
		if patch.Progress != nil {
			mb.Progress = *patch.Progress
		}
		if patch.SystemPatterns != nil {
			mb.SystemPatterns = *patch.SystemPatterns
		}
		if patch.UserProfile != nil {
			mb.UserProfile = *patch.UserProfile
		}
		if err := h.App.MemoryBankStore.Save(*mb); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// UserProfileHandler handles GET/POST /api/user-profile.
// GET returns the current user profile from MemoryBank.
// POST updates the user profile (preferences, expertise, communication style).
func (h *APIHandler) UserProfileHandler(w http.ResponseWriter, r *http.Request) {
	if h.App.MemoryBankStore == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "MemoryBankStore not initialized"})
		return
	}

	if r.Method == http.MethodGet {
		mb, _ := h.App.MemoryBankStore.Load()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mb.UserProfile)
		return
	}

	if r.Method == http.MethodPost {
		var up store.UserProfile
		if err := json.NewDecoder(r.Body).Decode(&up); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := h.App.MemoryBankStore.SaveUserProfile(up); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (h *APIHandler) NotificationsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.App.Scheduler == nil {
		json.NewEncoder(w).Encode([]struct{}{})
		return
	}
	if r.Method == http.MethodPost {
		h.App.Scheduler.MarkNotificationsRead()
	}
	json.NewEncoder(w).Encode(h.App.Scheduler.GetNotifications())
}

// HandleEventsStream is the SSE endpoint for real-time observability.
// Design ref: docs/architecture/TIME_TRAVEL_AND_OBSERVABILITY_DESIGN.md
func (h *APIHandler) HandleEventsStream(w http.ResponseWriter, r *http.Request) {
	if h.App.Logger == nil {
		http.Error(w, "logger not available", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Flush headers immediately so the client receives the HTTP 200 response
	// and EventSource.onopen fires, without waiting for the first event.
	// Without this, a quiet system (no active tasks generating events) would
	// leave the SSE connection in limbo — headers buffered but never sent.
	flusher.Flush()

	ch := h.App.Logger.SubscribeEvents()
	defer h.App.Logger.UnsubscribeEvents(ch)

	for {
		select {
		case evt, alive := <-ch:
			if !alive {
				return
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
