package api

import (
	"encoding/json"
	"net/http"

	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/providers"
)

// ListModelsHandler handles GET /api/providers/models?provider=<id>
//
// Calls the provider's ListModels() method (if it implements interfaces.ModelLister)
// to fetch the%2C the live model list from the upstream API. The frontend uses this to
// proactively refresh available models and let users check/uncheck which to show.
//
// Response:
//
//	200: {"provider": "<id>", "models": [{id, name, context_window, ...}], "source": "live"}
//	200: {"provider": "<id>", "models": [...], "source": "fallback", "warning": "..."}
//	400: {"error": "missing 'provider' query parameter"}
//	404: {"error": "provider not found: <id>"}
//	501: {"error": "provider <id> does not support model listing"}
func (h *APIHandler) ListModelsHandler(w http.ResponseWriter, r *http.Request) {
	providerID := r.URL.Query().Get("provider")
	if providerID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing 'provider' query parameter")
		return
	}

	// Look up provider config from registry
	h.App.Registry.Mu.RLock()
	cfg, ok := h.App.Registry.Providers[providerID]
	h.App.Registry.Mu.RUnlock()

	if !ok || cfg == nil {
		writeJSONError(w, http.StatusNotFound, "provider not found: "+providerID)
		return
	}

	// Get or create provider instance
	provider, err := providers.Get(cfg, h.App.Registry.ExternalRuntimes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create provider instance: "+err.Error())
		return
	}

	// Check if provider supports model listing
	lister, ok := provider.(interfaces.ModelLister)
	if !ok {
		writeJSONError(w, http.StatusNotImplemented, "provider "+providerID+" does not support model listing")
		return
	}

	// Call ListModels
	models, err := lister.ListModels(r.Context())
	if err != nil {
		// If ListModels returns an error but also some models (2-value return with fallback),
		// we still return what we got with a warning.
		if len(models) > 0 {
			writeJSON(w, map[string]any{
				"provider": providerID,
				"models":   models,
				"source":   "fallback",
				"warning":  err.Error(),
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "ListModels failed: "+err.Error())
		return
	}

	writeJSON(w, map[string]any{
		"provider": providerID,
		"models":   models,
		"source":   "live",
	})
}

// writeJSONError writes a JSON error response with a custom HTTP status code.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
