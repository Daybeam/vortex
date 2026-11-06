package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression test for register O6, decision Q4-2 (2026-11-04).
//
// Bug: the schedule subsystem advertised an action named "reload" whose body
// was providers.ClearCache() + Registry.Load() — it never touched the schedule
// store. An operator who edited schedules.json and invoked schedule.reload got
// {"ok":true} and stale cron rows, which is a lie rather than a limitation.
//
// Fix chosen by the user: rename, do NOT implement a real schedule reload
// (ScheduleStore.Reload is merge-only and holds ScheduleStore.Mu across up to
// 10s of backend I/O next to the 30s scan ticker). The old name now returns an
// explicit "renamed" error instead of silently doing the wrong thing.

// writeProviderConfig keeps Providers non-empty on purpose: loadLocked
// (config/loader.go:304) falls back to bootstrap defaults when a config loads
// with zero providers, and bootstrapDefaultsLocked overwrites DefaultProvider —
// which would mask the very field this test watches for a reload.
func writeProviderConfig(t *testing.T, path, provider string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"default_provider": provider,
		"providers": map[string]any{
			provider: map[string]any{"provider": "openai", "model": "test-model"},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func adminCtx() context.Context {
	return context.WithValue(context.Background(), AuthTierKey, TierAdmin)
}

func newReloadTestRegistry(t *testing.T) (*App, string) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeProviderConfig(t, configPath, "provider-before")
	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.DefaultProvider != "provider-before" {
		t.Fatalf("setup: DefaultProvider = %q, want %q", reg.DefaultProvider, "provider-before")
	}
	app := &App{Registry: reg, Tier: TierAdmin}
	registerScheduleSubsystem(app)
	return app, configPath
}

// TestScheduleReload_NoLongerReloadsTheRegistryUnderAMisleadingName pins the
// tombstone: the old name must fail loudly, and must not perform the body it
// used to perform.
func TestScheduleReload_NoLongerReloadsTheRegistryUnderAMisleadingName(t *testing.T) {
	app, configPath := newReloadTestRegistry(t)

	// A disk change that a reload would pick up.
	writeProviderConfig(t, configPath, "provider-after")

	res, err := registry.Invoke(adminCtx(), app, "schedule", "reload", map[string]any{})
	if err == nil {
		t.Fatalf("O6/Q4-2: schedule.reload still returns success for an action that does not reload schedules: result=%v", res)
	}
	if !strings.Contains(err.Error(), "reload_registry") {
		t.Errorf("renamed error must name the replacement action, got: %v", err)
	}
	if app.Registry.DefaultProvider != "provider-before" {
		t.Errorf("the tombstone must not keep performing the old body, but the registry reloaded to %q", app.Registry.DefaultProvider)
	}

	// Paired proof that the disk change above was live, so the stale value in
	// memory is the tombstone's doing and not a failed write.
	if _, err := registry.Invoke(adminCtx(), app, "schedule", "reload_registry", map[string]any{}); err != nil {
		t.Fatalf("reload_registry: %v", err)
	}
	if app.Registry.DefaultProvider != "provider-after" {
		t.Errorf("reload_registry did not pick up the disk change it is named for (still %q)", app.Registry.DefaultProvider)
	}
}

// TestScheduleReloadRegistry_ReloadsRegistryAndPromisesNothingElse checks the
// new name against both halves of its description: it does re-read config from
// disk, and it does not reload the schedule store.
func TestScheduleReloadRegistry_ReloadsRegistryAndPromisesNothingElse(t *testing.T) {
	app, configPath := newReloadTestRegistry(t)

	// A schedule row that exists only on disk, as an outside edit would leave it:
	// the store is built first, so only a real reload could ever see the row.
	schedulesPath := filepath.Join(t.TempDir(), "schedules.json")
	app.SStore = store.NewScheduleStore(schedulesPath, nil)
	if len(app.SStore.List()) != 0 {
		t.Fatalf("setup: fresh store should start empty, has %d rows", len(app.SStore.List()))
	}
	if err := os.WriteFile(schedulesPath, mustMarshalSchedules(t), 0644); err != nil {
		t.Fatalf("write schedules: %v", err)
	}

	writeProviderConfig(t, configPath, "provider-after")

	if _, err := registry.Invoke(adminCtx(), app, "schedule", "reload_registry", map[string]any{}); err != nil {
		t.Fatalf("reload_registry: %v", err)
	}
	if app.Registry.DefaultProvider != "provider-after" {
		t.Errorf("reload_registry must reload the tool registry from disk, DefaultProvider still %q", app.Registry.DefaultProvider)
	}
	if got := app.SStore.Get("on-disk-only"); got != nil {
		t.Errorf("reload_registry must not claim to reload schedules; it picked up row %q", got.ID)
	}
}

// TestScheduleReloadRegistry_IsAdminOnly guards the renamed action against
// leaking through the Tier1 allowlist: schedule.list is the only public
// schedule action.
func TestScheduleReloadRegistry_IsAdminOnly(t *testing.T) {
	app, _ := newReloadTestRegistry(t)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)
	if _, err := registry.Invoke(ctx, app, "schedule", "reload_registry", map[string]any{}); err == nil {
		t.Error("reload_registry must require Tier2 (Admin)")
	}
}

func mustMarshalSchedules(t *testing.T) []byte {
	t.Helper()
	rows := map[string]*schemas.Schedule{
		"on-disk-only": {ID: "on-disk-only", Name: "on-disk-only", Type: schemas.ScheduleInterval, Interval: "1h"},
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal schedules: %v", err)
	}
	return data
}
