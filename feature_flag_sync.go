package main

import (
	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/providers"
)

// syncFeatureFlags pushes Registry.System.FeatureFlags to the package-level
// atomic flags in core, providers, and client. Called at startup and on
// every config hot-reload via OnReload.
//
// This bridges the gap between the config layer (which has the Registry)
// and the core/providers/client packages (which use package-level atomic
// flags because they lack Registry access in their hot paths).
//
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md.
func syncFeatureFlags(reg *config.Registry) {
	reg.Mu.RLock()
	ff := reg.System.FeatureFlags
	reg.Mu.RUnlock()
	core.SetEgressRedactionDisabled(ff.DisableEgressRedaction)
	providers.SetDisableThinking(ff.DisableThinking)
	providers.SetDebugEnabled(ff.Debug)
	client.SetCgroupSandboxDisabled(ff.DisableCgroupSandbox)
}
