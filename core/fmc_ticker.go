package core

import (
	"context"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/store"
)

// FMCBatchTicker periodically runs ExperienceStore.RunFMCBatch to backfill
// structured failure-mode labels + critiques on failure nodes that haven't
// been classified yet (FMC Phase 2, "Batch Path (Independent, Weak Model,
// Cron-Triggered)" -- see
// docs/completed/2026-09-06/architecture/FMC_DESIGN.md §4.2/§8).
//
// Opt-in by design, matching this project's established convention for any
// feature that spends real money/tokens on a background timer (e.g.
// EnableAutoRepair): does nothing at all unless
// registry.System.FMCWeakModel names a real, registered provider config.
// A cheap/fast model is intended here (e.g. GPT-4o-mini, Gemini Flash), not
// the primary task-execution provider -- RunFMCBatch classifies potentially
// many pending nodes with one sequential LLM call each, so an expensive
// model here would be a real, ongoing cost surprise.
//
// RunFMCBatch is a concrete-only method on *store.ExperienceStore (not on
// IExperienceStore, matching the existing convention already established by
// resolveFailureModeProfile's own type-assertion -- keeping the interface
// narrow for test doubles), so runOnce type-asserts with a graceful,
// logged-and-skipped fallback rather than widening the interface.
type FMCBatchTicker struct {
	reg      *config.Registry
	runtimes config.ExternalRuntimes
	expStore store.IExperienceStore
	logger   *Logger
	interval time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
	ctx      context.Context // audit M11: lifecycle ctx so runOnce cancels on Stop
	cancel   context.CancelFunc
}

// NewFMCBatchTicker constructs a ticker. interval defaults to 6 hours if
// registry.System.FMCBatchIntervalHours is unset/zero, matching the design
// doc's guidance.
func NewFMCBatchTicker(reg *config.Registry, runtimes config.ExternalRuntimes, expStore store.IExperienceStore, logger *Logger) *FMCBatchTicker {
	interval := 6 * time.Hour
	reg.Mu.RLock()
	if reg.System.FMCBatchIntervalHours > 0 {
		interval = time.Duration(reg.System.FMCBatchIntervalHours) * time.Hour
	}
	reg.Mu.RUnlock()
	ctx, cancel := context.WithCancel(context.Background()) // audit M11: lifecycle ctx
	return &FMCBatchTicker{
		reg:      reg,
		runtimes: runtimes,
		expStore: expStore,
		logger:   logger,
		interval: interval,
		stopCh:   make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start begins the periodic loop in a background goroutine. A no-op if
// registry.System.FMCWeakModel is not configured. Safe to call once; call
// Stop to end the loop.
func (t *FMCBatchTicker) Start() {
	t.reg.Mu.RLock()
	weakModel := t.reg.System.FMCWeakModel
	t.reg.Mu.RUnlock()
	if weakModel == "" {
		return
	}
	go t.loop()
}

// Stop ends the background loop. Safe to call multiple times or without a
// prior Start.
func (t *FMCBatchTicker) Stop() {
	t.stopOnce.Do(func() {
		if t.cancel != nil {
			t.cancel() // audit M11: cancel in-flight runOnce
		}
		close(t.stopCh)
	})
}

func (t *FMCBatchTicker) loop() {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.runOnce()
		}
	}
}

// runOnce resolves the configured weak-model provider and runs one FMC
// batch pass. Every failure path (provider not found, resolution error,
// wrong concrete ExperienceStore type, RunFMCBatch itself erroring) is
// logged and skipped rather than propagated -- this is a background
// maintenance job, not something that should ever be able to crash the
// process or block anything else.
func (t *FMCBatchTicker) runOnce() {
	t.reg.Mu.RLock()
	weakModel := t.reg.System.FMCWeakModel
	cfg, ok := t.reg.Providers[weakModel]
	t.reg.Mu.RUnlock()

	if !ok || cfg == nil {
		if t.logger != nil {
			t.logger.Log("EventFMCBatchSkipped", "", "", map[string]any{
				"reason": "configured FMCWeakModel provider not found in Registry.Providers",
				"name":   weakModel,
			})
		}
		return
	}

	concreteES, isConcrete := t.expStore.(*store.ExperienceStore)
	if !isConcrete {
		if t.logger != nil {
			t.logger.Log("EventFMCBatchSkipped", "", "", map[string]any{
				"reason": "expStore is not a *store.ExperienceStore (test double or unimplemented backend)",
			})
		}
		return
	}

	provider, err := providers.Get(cfg, t.runtimes)
	if err != nil {
		if t.logger != nil {
			t.logger.Log("EventFMCBatchFailed", "", "", map[string]any{
				"reason": "failed to resolve FMCWeakModel provider",
				"name":   weakModel,
				"error":  err.Error(),
			})
		}
		return
	}

	ctx, cancel := context.WithTimeout(t.ctx, 10*time.Minute) // audit M11: derive from lifecycle ctx
	defer cancel()

	n, err := concreteES.RunFMCBatch(ctx, provider, cfg.Model)
	detail := map[string]any{"classified": n, "model": cfg.Model}
	if err != nil {
		detail["error"] = err.Error()
		if t.logger != nil {
			t.logger.Log("EventFMCBatchFailed", "", "", detail)
		}
		return
	}
	if t.logger != nil {
		t.logger.Log("EventFMCBatchCompleted", "", "", detail)
	}
}
