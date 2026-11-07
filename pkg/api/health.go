package api

import (
	"encoding/json"
	"net/http"

	"github.com/daybeam/vortex/pkg/bootmetrics"
)

// HealthResponse is served by /health on both the admin and public muxes.
// Boot carries the per-phase startup breakdown so a slow boot can be
// attributed from outside the process.
type HealthResponse struct {
	Status   string              `json:"status"`
	UptimeMs int64               `json:"uptimeMs"`
	Boot     []bootmetrics.Phase `json:"boot,omitempty"`
}

// HealthHandler answers /health without touching the DB or scheduler, so it
// stays cheap enough for liveness probes.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(HealthResponse{
		Status:   "ok",
		UptimeMs: bootmetrics.SinceMs(),
		Boot:     bootmetrics.Snapshot(),
	})
}
