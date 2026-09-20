package providers

import (
	"fmt"

	"github.com/daybeam/vortex/config"
)

// RegisterConfiguredInstances eagerly constructs and registers a Provider
// (and a GlobalRouter slot) for every entry in a named ProviderConfig's
// opt-in Instances list.
//
// Why this needs to run eagerly at startup, not lazily on first use:
// GlobalRouter's normal registration path is via Get(), which only runs when
// some role actually resolves to that specific named provider config and
// spawns a step. That means a dedicated "backup" provider config would only
// ever get registered into the router once some role happens to reference it
// directly -- which never happens for a pure failover backup (nothing should
// normally route to it by name; it should only be discovered as a fallback
// slot for the *primary* provider's type when the primary is unhealthy). Left
// purely reactive, the first time a primary fails there would be no backup
// slot registered yet to fail over to. Calling this once after the Registry
// is loaded (before serving any requests) closes that gap.
//
// Fully opt-in and additive: named provider configs that don't set Instances
// are completely unaffected. Errors registering an individual instance (e.g.
// cfg.Provider naming an unsupported provider type) are logged via logFn and
// that instance is skipped, rather than aborting the whole pass -- a bad
// instance definition shouldn't take down the primary provider or any other,
// valid instances.
func RegisterConfiguredInstances(reg *config.Registry, runtimes config.ExternalRuntimes, logFn func(msg string)) {
	if reg == nil {
		return
	}

	type instanceJob struct {
		parentID string
		inst     config.ProviderInstance
		base     config.ProviderConfig
	}

	reg.Mu.RLock()
	var jobs []instanceJob
	for id, pc := range reg.Providers {
		if pc == nil {
			continue
		}

		// Case A: Explicit Instances list (takes precedence)
		if len(pc.Instances) > 0 {
			for _, inst := range pc.Instances {
				jobs = append(jobs, instanceJob{parentID: id, inst: inst, base: *pc})
			}
			continue
		}

		// Case B: Simple multi-key list (ADDED 2026-08-30)
		// Automatically expands into failover slots if no explicit instances defined.
		if len(pc.APIKeys) > 0 {
			for i, key := range pc.APIKeys {
				inst := config.ProviderInstance{
					ID:     fmt.Sprintf("%s-key-%d", id, i),
					APIKey: key,
				}
				jobs = append(jobs, instanceJob{parentID: id, inst: inst, base: *pc})
			}
		}
	}
	reg.Mu.RUnlock()

	for _, job := range jobs {
		derived := job.base
		// Instances is intentionally not carried over onto the derived copy:
		// each instance is a leaf slot, not itself a fan-out point. Carrying
		// it forward would be harmless today (nothing recurses into a
		// registered instance's own Instances field) but leaving it set would
		// be a footgun if this function is ever called more than once on the
		// same *Registry (e.g. after a config reload) or reused elsewhere.
		derived.Instances = nil
		// FIX (2026-07-19): explicitly set PoolID to the parent's registry key
		// so every instance under this parent shares one failover pool. This is
		// the one sanctioned way for multiple ProviderConfigs to share a pool --
		// see PoolID's doc comment in config.go. Registry.Load() would otherwise
		// default each of these derived instances (if it ever went through Load)
		// to its own unique PoolID, but these are constructed in-memory here,
		// never through Load(), so this must be set explicitly.
		derived.PoolID = job.parentID
		if job.inst.APIKey != "" {
			derived.APIKey = job.inst.APIKey
		}
		if job.inst.APIKeyEnv != "" {
			derived.APIKeyEnv = job.inst.APIKeyEnv
		}
		if job.inst.BaseURL != "" {
			derived.BaseURL = job.inst.BaseURL
		}

		instID := job.inst.ID
		if instID == "" {
			instID = fmt.Sprintf("%s-instance-%d", job.parentID, len(jobs))
		}

		if _, err := Get(&derived, runtimes); err != nil {
			if logFn != nil {
				logFn(fmt.Sprintf("provider instance %q (parent %q, type %q) failed to register: %v", instID, job.parentID, derived.Provider, err))
			}
			continue
		}
		if logFn != nil {
			logFn(fmt.Sprintf("registered provider instance %q (parent %q, type %q) for failover", instID, job.parentID, derived.Provider))
		}
	}
}
