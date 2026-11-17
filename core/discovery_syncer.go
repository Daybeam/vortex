package core

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
)

// DiscoverySyncer orchestrates periodic discovery from all configured sources.
// It replaces SkillSyncer, adding MCP discovery support and multi-source
// orchestration. Sources are built from DiscoverySourceConfig entries (and
// legacy SkillSyncSources for backward compat).
//
// Lifecycle: Start() launches a background goroutine that calls SyncAll() on
// a ticker (interval from System.DiscoverySyncIntervalHours, default 24h).
// Stop() cancels the context and closes the stop channel.
//
// See docs/completed/2026-11-13/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.5.
type DiscoverySyncer struct {
	sources  []DiscoverySource
	registry *config.Registry
	logger   *obs.Logger
	stopChan chan struct{}
	stopOnce sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewDiscoverySyncer builds a DiscoverySyncer from the registry's configured
// discovery sources. It converts both DiscoverySources (new format) and
// SkillSyncSources (legacy format) into DiscoverySource adapter instances.
//
// The loader is required for GitHubRepoSource (to fetch repo contents).
// If loader is nil, GitHub-type sources are skipped with a warning.
func NewDiscoverySyncer(reg *config.Registry, loader *ResourceLoader, logger *obs.Logger) *DiscoverySyncer {
	ctx, cancel := context.WithCancel(context.Background())
	d := &DiscoverySyncer{
		registry: reg,
		logger:   logger,
		stopChan: make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}

	d.sources = d.buildSources(loader)
	return d
}

// buildSources constructs DiscoverySource adapters from DiscoverySources
// config entries. Legacy SkillSyncSources are converted to DiscoverySources
// by the config loader (config/loader.go), so this method only needs to
// read System.DiscoverySources.
func (d *DiscoverySyncer) buildSources(loader *ResourceLoader) []DiscoverySource {
	reg := d.registry
	reg.Mu.RLock()
	configSources := make([]config.DiscoverySourceConfig, len(reg.System.DiscoverySources))
	copy(configSources, reg.System.DiscoverySources)
	reg.Mu.RUnlock()

	var sources []DiscoverySource

	// Build from DiscoverySources (includes legacy SkillSyncSources converted
	// by the config loader).
	for _, cfg := range configSources {
		src, err := d.buildSource(cfg, loader)
		if err != nil {
			if d.logger != nil {
				d.logger.Log("EventDiscoverySourceError", cfg.Name, "", map[string]any{
					"error": err.Error(),
				})
			}
			continue
		}
		sources = append(sources, src)
	}

	return sources
}

// buildSource constructs a single DiscoverySource from a DiscoverySourceConfig.
func (d *DiscoverySyncer) buildSource(cfg config.DiscoverySourceConfig, loader *ResourceLoader) (DiscoverySource, error) {
	kind := DiscoveryKind(cfg.Kind)
	switch DiscoverySourceType(cfg.Type) {
	case DiscoverySourceGitHub:
		if loader == nil {
			return nil, fmt.Errorf("GitHub source %q requires a ResourceLoader", cfg.Name)
		}
		src, err := NewGitHubRepoSource(cfg.Name, kind, cfg.URL, loader)
		if err != nil {
			return nil, fmt.Errorf("building GitHub source %q: %w", cfg.Name, err)
		}
		// Override trust level if explicitly configured as trusted.
		if cfg.Trusted {
			// GitHubRepoSource defaults to untrusted; we need to stamp
			// trusted on the items after Discover. Since the adapter doesn't
			// have a trust field, we wrap it.
			return &trustedSourceWrapper{inner: src}, nil
		}
		return src, nil

	case DiscoverySourceHTTP:
		src := NewHTTPListSource(cfg.Name, kind, cfg.URL, cfg.Headers)
		if cfg.Trusted {
			return &trustedSourceWrapper{inner: src}, nil
		}
		return src, nil

	case DiscoverySourceLocal:
		// Local sources are always trusted regardless of cfg.Trusted.
		return NewLocalDirSource(cfg.Name, kind, cfg.Path, true), nil

	default:
		return nil, fmt.Errorf("unknown discovery source type %q for source %q", cfg.Type, cfg.Name)
	}
}

// trustedSourceWrapper wraps a DiscoverySource and stamps all discovered items
// with TrustLevelTrusted. This is used when a remote source is explicitly
// configured as trusted in DiscoverySourceConfig.
type trustedSourceWrapper struct {
	inner DiscoverySource
}

func (w *trustedSourceWrapper) Name() string        { return w.inner.Name() }
func (w *trustedSourceWrapper) Kind() DiscoveryKind { return w.inner.Kind() }

func (w *trustedSourceWrapper) Discover(ctx context.Context) ([]DiscoveredItem, error) {
	items, err := w.inner.Discover(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].TrustLevel = TrustLevelTrusted
	}
	return items, nil
}

// Start launches the periodic sync goroutine. Idempotent via stopOnce.
func (d *DiscoverySyncer) Start() {
	go d.loop()
}

// Stop cancels the sync context and closes the stop channel. Idempotent.
func (d *DiscoverySyncer) Stop() {
	d.stopOnce.Do(func() {
		d.cancel()
		close(d.stopChan)
	})
}

// loop is the periodic sync goroutine, mirroring SkillSyncer.loop().
func (d *DiscoverySyncer) loop() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[discovery-syncer] goroutine panic: %v\n%s", r, debug.Stack())
		}
	}()

	interval := time.Duration(d.registry.System.DiscoverySyncIntervalHours) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial sync
	d.SyncAll()

	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			d.SyncAll()
		}
	}
}

// SyncAll iterates over all configured discovery sources, fetches items,
// and ingests them into the registry. Errors per-source are logged but
// do not abort the overall sync.
func (d *DiscoverySyncer) SyncAll() {
	if len(d.sources) == 0 {
		return
	}

	if d.logger != nil {
		d.logger.Log("EventDiscoverySyncAllStarted", "", "", map[string]any{
			"source_count": len(d.sources),
		})
	}

	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	for _, source := range d.sources {
		if ctx.Err() != nil {
			break // shutdown in progress
		}
		items, err := source.Discover(ctx)
		if err != nil {
			if d.logger != nil {
				d.logger.Log("EventDiscoveryError", source.Name(), "", map[string]any{
					"error": err.Error(),
				})
			}
			continue
		}

		switch source.Kind() {
		case DiscoveryKindSkill:
			d.ingestSkills(items, source)
		case DiscoveryKindMCP:
			d.ingestMCPs(items, source)
		}
	}

	if d.logger != nil {
		d.logger.Log("EventDiscoverySyncAllCompleted", "", "", nil)
	}
}

// ingestSkills processes discovered skill items and writes them to the
// registry. Trusted items go directly to Registry.Skills; untrusted items
// go to PendingSkills staging (requiring promote_skill before use).
func (d *DiscoverySyncer) ingestSkills(items []DiscoveredItem, source DiscoverySource) {
	skillCount := 0
	pendingCount := 0

	for _, item := range items {
		if item.Kind != DiscoveryKindSkill {
			continue
		}

		skill := discoveredItemToSkill(item)

		// For trusted local sources with directory-based skills, use the
		// full conversion that includes SkillDir, ExecutionMode, etc.
		// This is detected by TrustLevel == trusted and the source being
		// a LocalDirSource.
		if item.TrustLevel == TrustLevelTrusted {
			if _, ok := source.(*LocalDirSource); ok {
				// LocalDirSource items need directory-based skill conversion.
				// The skill directory is reconstructed from the item's
				// Implementations (which carry the system prompt from the
				// SKILL.md file). For now, use the basic conversion —
				// the full directory-based conversion requires the original
				// file path which is not available through the DiscoveredItem
				// interface. This is a known limitation; local skills are
				// also imported via ImportStandardDiscoveryPaths which does
				// have the full path.
				skill.Source = config.SourceExternalDynamic
			}
			d.registry.Mu.Lock()
			d.registry.Skills[skill.ID] = skill
			d.registry.Mu.Unlock()
			skillCount++
		} else {
			d.registry.Mu.Lock()
			if d.registry.PendingSkills == nil {
				d.registry.PendingSkills = make(map[string]*config.Skill)
			}
			d.registry.PendingSkills[skill.ID] = skill
			d.registry.Mu.Unlock()
			pendingCount++
		}
	}

	if d.logger != nil && (skillCount > 0 || pendingCount > 0) {
		d.logger.Log("EventDiscoverySkillsIngested", source.Name(), "", map[string]any{
			"direct_count":  skillCount,
			"pending_count": pendingCount,
		})
	}
}

// ingestMCPs processes discovered MCP items and registers them in
// DynamicMCPs with admin_only mount policy. All discovered MCPs default
// to admin_only regardless of source trust level — this is a security
// constraint. The MCPs are also stamped with Source = external_dynamic
// and CredentialScope = operator (fail-closed default).
func (d *DiscoverySyncer) ingestMCPs(items []DiscoveredItem, source DiscoverySource) {
	mcpCount := 0

	for _, item := range items {
		if item.Kind != DiscoveryKindMCP {
			continue
		}

		mcp := discoveredItemToMCPDef(item)

		d.registry.Mu.Lock()
		if d.registry.DynamicMCPs == nil {
			d.registry.DynamicMCPs = make(map[string]*config.MCPDef)
		}
		d.registry.DynamicMCPs[mcp.ID] = mcp
		d.registry.Mu.Unlock()
		mcpCount++
	}

	if d.logger != nil && mcpCount > 0 {
		d.logger.Log("EventDiscoveryMCPsIngested", source.Name(), "", map[string]any{
			"count": mcpCount,
		})
	}
}

// discoveredItemToMCPDef converts a DiscoveredItem to a config.MCPDef.
// All discovered MCPs are stamped with:
//   - MountPolicy = admin_only (security default)
//   - Source = external_dynamic
//   - CredentialScope = operator (fail-closed default)
//
// See docs/completed/2026-11-13/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.4.2.
func discoveredItemToMCPDef(item DiscoveredItem) *config.MCPDef {
	return &config.MCPDef{
		ID:              item.ID,
		URL:             item.URL,
		Command:         item.Command,
		Args:            item.Args,
		Env:             item.Env,
		Provides:        item.Provides,
		Source:          config.SourceExternalDynamic,
		MountPolicy:     config.MountPolicyAdminOnly,
		CredentialScope: config.CredentialOperator,
	}
}

// SourceCount returns the number of configured discovery sources.
// Useful for diagnostics and tests.
func (d *DiscoverySyncer) SourceCount() int {
	return len(d.sources)
}
