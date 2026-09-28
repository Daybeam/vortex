package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/store"
)

// Exporter handles synchronizing experience data to a remote endpoint.
type Exporter struct {
	config  config.TelemetryConfig
	exp     store.IExperienceStore
	client  *http.Client
}

func NewExporter(cfg config.TelemetryConfig, exp store.IExperienceStore) *Exporter {
	return &Exporter{
		config: cfg,
		exp:    exp,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Sync performs a one-time synchronization of experience data.
func (e *Exporter) Sync(ctx context.Context) error {
	if !e.config.Enabled || e.config.Endpoint == "" {
		return nil
	}

	snapshot := e.exp.GetTelemetrySnapshot(e.config.DetailedFeedback)

	// Redact sensitive info before sending (ADDED 2026-08-28)
	if e.config.DetailedFeedback {
		snapshot = observability.RedactMap(snapshot)
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal telemetry snapshot: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.config.Endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create sync request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Orchestrator-MCP/1.0 (Telemetry)")

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("execute sync request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("sync endpoint returned error: %s", resp.Status)
	}

	log.Printf("[telemetry] Experience sync successful: %d bytes sent", len(data))
	return nil
}

// StartBackgroundSync runs a ticker that periodically syncs experience data.
func (e *Exporter) StartBackgroundSync(ctx context.Context) {
	if !e.config.Enabled || e.config.Endpoint == "" {
		log.Printf("[telemetry] Background sync disabled (enabled=%v, endpoint=%q)", e.config.Enabled, e.config.Endpoint)
		return
	}

	interval := time.Duration(e.config.SyncIntervalMins) * time.Minute
	if interval < 5*time.Minute {
		interval = 5 * time.Minute // Minimum interval
	}

	log.Printf("[telemetry] Starting background sync every %v to %s", interval, e.config.Endpoint)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := e.Sync(ctx); err != nil {
				log.Printf("[telemetry] Sync failed: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}
