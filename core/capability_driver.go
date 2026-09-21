package core

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
)

// capability_driver.go — Unified Capability Driver Pattern.
// Implements docs/architecture/UNIFIED_CAPABILITY_DISCOVERY_DESIGN.md §二.
//
// Decouples capability discovery from execution. Whether a capability is a
// remote SSE endpoint (Phase 1) or a future local binary (Phase 2), the
// orchestrator interacts with it through the same CapabilityDriver lifecycle:
// Inspect → Mount → (use via existing MCP machinery) → Unmount.
//
// The driver does NOT replace the existing MCP execution path — Mount simply
// registers a config.MCPDef into the registry's DynamicMCPs, so the spawner,
// hub, and tool router see it as a normal remote MCP. This is the minimal
// seam: zero changes to the execution layer, all the value is in the
// discovery + lifecycle layer.

// CapabilityDriver defines the unified lifecycle for any external capability.
type CapabilityDriver interface {
	// Inspect pre-checks whether the capability is reachable: for remote
	// SSE, an HTTP HEAD/GET against the endpoint; for future local
	// drivers, a binary existence + version check. Must be fast (seconds)
	// and side-effect-free.
	Inspect(ctx context.Context) error

	// Mount registers the capability into the registry so the existing
	// tool router and spawner can dispatch to it. Idempotent: mounting
	// an already-mounted capability is a no-op.
	Mount(ctx context.Context) error

	// Unmount safely removes the capability from the registry. Idempotent.
	Unmount(ctx context.Context) error
}

// NewCapabilityDriver is the factory: given a CapabilityDef, returns the
// concrete driver for its transport. Returns an error for unrecognized
// transports (including Phase 2 transports not yet implemented).
func NewCapabilityDriver(def config.CapabilityDef, reg *config.Registry) (CapabilityDriver, error) {
	switch def.Transport {
	case config.TransportRemoteSSE:
		return &RemoteSSEDriver{def: def, reg: reg}, nil
	case config.TransportLocalProcess, config.TransportSandboxJIT:
		return nil, fmt.Errorf("capability transport %q not yet implemented (Phase 2)", def.Transport)
	default:
		return nil, fmt.Errorf("unknown capability transport %q", def.Transport)
	}
}

// ---- RemoteSSEDriver (Phase 1) ---------------------------------------------

// RemoteSSEDriver manages a remote SSE/HTTP MCP endpoint through the
// CapabilityDriver lifecycle. Mount registers an MCPDef with URL set into
// the registry's DynamicMCPs; the existing spawner_mcp.go callRemoteMCPTool
// path then handles execution with no further driver involvement.
type RemoteSSEDriver struct {
	def config.CapabilityDef
	reg *config.Registry
}

// Inspect does a lightweight HTTP HEAD against the endpoint. A 2xx or 4xx
// response means the endpoint is alive (404/405 are fine — the endpoint
// exists, just doesn't support HEAD). Only network-level failures (DNS,
// connection refused, timeout) return an error.
func (d *RemoteSSEDriver) Inspect(ctx context.Context) error {
	if d.def.Endpoint == "" {
		return fmt.Errorf("inspect: empty endpoint for capability %q", d.def.ID)
	}
	endpoint := d.def.Endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return fmt.Errorf("inspect %q: build request: %w", d.def.ID, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("inspect %q: %w", d.def.ID, err)
	}
	resp.Body.Close()
	return nil
}

// Mount registers the SSE endpoint as a dynamic MCP in the registry. The
// MCPDef.URL field is what spawner_mcp.go's callRemoteMCPTool uses to
// dispatch HTTP calls — no execution-layer changes needed.
func (d *RemoteSSEDriver) Mount(ctx context.Context) error {
	if d.reg == nil {
		return fmt.Errorf("mount %q: nil registry", d.def.ID)
	}
	if d.def.Endpoint == "" {
		return fmt.Errorf("mount %q: empty endpoint", d.def.ID)
	}

	// Idempotent: if already registered, no-op.
	if existing := d.reg.GetMCP(d.def.ID); existing != nil && existing.URL != "" {
		return nil
	}

	endpoint := d.def.Endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	mcp := &config.MCPDef{
		ID:      d.def.ID,
		URL:     endpoint,
		Trusted: false,
	}
	// Propagate auth config from manifest if present.
	if v, ok := d.def.Manifest["auth_header_name"].(string); ok {
		mcp.AuthHeaderName = v
	}
	if v, ok := d.def.Manifest["api_key_env"].(string); ok {
		mcp.APIKeyEnv = v
	}

	d.reg.RegisterDynamicMCP(mcp)
	return nil
}

// Unmount removes the capability from the registry. Idempotent.
func (d *RemoteSSEDriver) Unmount(ctx context.Context) error {
	if d.reg == nil {
		return nil
	}
	d.reg.UnregisterDynamicMCP(d.def.ID)
	return nil
}
