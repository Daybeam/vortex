package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// capability_driver_test.go — tests for the Unified Capability Driver Pattern.
// See docs/architecture/UNIFIED_CAPABILITY_DISCOVERY_DESIGN.md.

func TestNewCapabilityDriver_RemoteSSE(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	def := config.CapabilityDef{
		ID:        "test-sse",
		Transport: config.TransportRemoteSSE,
		Endpoint:  "https://example.com/sse",
	}
	driver, err := NewCapabilityDriver(def, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := driver.(*RemoteSSEDriver); !ok {
		t.Errorf("expected *RemoteSSEDriver, got %T", driver)
	}
}

func TestNewCapabilityDriver_UnsupportedTransport(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{}
	def := config.CapabilityDef{
		ID:        "test-local",
		Transport: config.TransportLocalProcess,
		Endpoint:  "/usr/local/bin/tool",
	}
	_, err := NewCapabilityDriver(def, reg)
	if err == nil {
		t.Fatal("expected error for unimplemented transport")
	}
}

func TestNewCapabilityDriver_UnknownTransport(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{}
	def := config.CapabilityDef{
		ID:        "test-unknown",
		Transport: "quantum_entanglement",
	}
	_, err := NewCapabilityDriver(def, reg)
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
}

func TestRemoteSSEDriver_MountUnmount(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	def := config.CapabilityDef{
		ID:        "cap-test",
		Transport: config.TransportRemoteSSE,
		Endpoint:  "https://example.com/mcp",
	}
	driver, err := NewCapabilityDriver(def, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx := context.Background()

	// Mount
	if err := driver.Mount(ctx); err != nil {
		t.Fatalf("Mount failed: %v", err)
	}
	mcp := reg.GetMCP("cap-test")
	if mcp == nil {
		t.Fatal("expected MCP to be registered after Mount")
	}
	if mcp.URL == "" {
		t.Error("expected non-empty URL on mounted MCP")
	}

	// Mount again (idempotent)
	if err := driver.Mount(ctx); err != nil {
		t.Fatalf("idempotent Mount failed: %v", err)
	}

	// Unmount
	if err := driver.Unmount(ctx); err != nil {
		t.Fatalf("Unmount failed: %v", err)
	}
	if reg.GetMCP("cap-test") != nil {
		t.Error("expected MCP to be gone after Unmount")
	}

	// Unmount again (idempotent)
	if err := driver.Unmount(ctx); err != nil {
		t.Fatalf("idempotent Unmount failed: %v", err)
	}
}

func TestRemoteSSEDriver_Mount_WithAuth(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	def := config.CapabilityDef{
		ID:        "cap-auth",
		Transport: config.TransportRemoteSSE,
		Endpoint:  "https://api.example.com/mcp",
		Manifest: map[string]any{
			"auth_header_name": "X-API-Key",
			"api_key_env":      "EXAMPLE_API_KEY",
		},
	}
	driver, err := NewCapabilityDriver(def, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := driver.Mount(context.Background()); err != nil {
		t.Fatalf("Mount failed: %v", err)
	}
	mcp := reg.GetMCP("cap-auth")
	if mcp == nil {
		t.Fatal("expected MCP after Mount")
	}
	if mcp.AuthHeaderName != "X-API-Key" {
		t.Errorf("expected AuthHeaderName 'X-API-Key', got %q", mcp.AuthHeaderName)
	}
	if mcp.APIKeyEnv != "EXAMPLE_API_KEY" {
		t.Errorf("expected APIKeyEnv 'EXAMPLE_API_KEY', got %q", mcp.APIKeyEnv)
	}
}

func TestRemoteSSEDriver_Mount_EmptyEndpoint(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	def := config.CapabilityDef{ID: "cap-empty", Transport: config.TransportRemoteSSE}
	driver, _ := NewCapabilityDriver(def, reg)
	if err := driver.Mount(context.Background()); err == nil {
		t.Error("expected error for empty endpoint")
	}
}

func TestRemoteSSEDriver_Inspect_EmptyEndpoint(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{}
	def := config.CapabilityDef{ID: "cap-empty", Transport: config.TransportRemoteSSE}
	driver, _ := NewCapabilityDriver(def, reg)
	if err := driver.Inspect(context.Background()); err == nil {
		t.Error("expected error for empty endpoint")
	}
}

func TestRemoteSSEDriver_Inspect_Unreachable(t *testing.T) {
	t.Parallel()
	reg := &config.Registry{}
	// Use a port that's almost certainly not listening.
	def := config.CapabilityDef{
		ID:        "cap-dead",
		Transport: config.TransportRemoteSSE,
		Endpoint:  "http://127.0.0.1:1",
	}
	driver, _ := NewCapabilityDriver(def, reg)
	// This should fail with a connection error.
	if err := driver.Inspect(context.Background()); err == nil {
		t.Error("expected error for unreachable endpoint")
	}
}
