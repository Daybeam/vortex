package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// mockDiscoverySource is a test-only DiscoverySource that returns
// predetermined items, allowing tests to exercise the syncer's
// ingestion logic without network calls.
type mockDiscoverySource struct {
	name  string
	kind  DiscoveryKind
	items []DiscoveredItem
	err   error
}

func (m *mockDiscoverySource) Name() string { return m.name }
func (m *mockDiscoverySource) Kind() DiscoveryKind { return m.kind }
func (m *mockDiscoverySource) Discover(ctx context.Context) ([]DiscoveredItem, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items, nil
}

func newDiscoveryTestRegistry() *config.Registry {
	return &config.Registry{
		Providers:     make(map[string]*config.ProviderConfig),
		MCPs:          make(map[string]*config.MCPDef),
		DynamicMCPs:   make(map[string]*config.MCPDef),
		Skills:        make(map[string]*config.Skill),
		PendingSkills: make(map[string]*config.Skill),
		System:        config.SystemSettings{},
	}
}

// --- NewDiscoverySyncer / buildSources ---

func TestDiscoverySyncer_NoSources(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := NewDiscoverySyncer(reg, nil, nil)
	if d.SourceCount() != 0 {
		t.Fatalf("expected 0 sources, got %d", d.SourceCount())
	}
}

func TestDiscoverySyncer_DiscoverySources(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	reg.System.DiscoverySources = []config.DiscoverySourceConfig{
		{Name: "test-http", Type: "http", Kind: "mcp", URL: "https://example.com/api"},
		{Name: "test-local", Type: "local", Kind: "skill", Path: "/tmp/skills"},
	}
	d := NewDiscoverySyncer(reg, nil, nil)
	if d.SourceCount() != 2 {
		t.Fatalf("expected 2 sources, got %d", d.SourceCount())
	}
}

func TestDiscoverySyncer_LegacySkillSyncSources(t *testing.T) {
	// Legacy SkillSyncSources are converted to DiscoverySources by the config
	// loader. The DiscoverySyncer only reads DiscoverySources.
	reg := newDiscoveryTestRegistry()
	reg.System.DiscoverySources = []config.DiscoverySourceConfig{
		{Name: "legacy-general", Type: "github", Kind: "skill", URL: "github:mattpocock/skills"},
	}
	loader := NewResourceLoader()
	d := NewDiscoverySyncer(reg, loader, nil)
	if d.SourceCount() != 1 {
		t.Fatalf("expected 1 legacy source, got %d", d.SourceCount())
	}
}

func TestDiscoverySyncer_MixedSources(t *testing.T) {
	// Mixed: one new-format DiscoverySource + one legacy-converted source.
	reg := newDiscoveryTestRegistry()
	reg.System.DiscoverySources = []config.DiscoverySourceConfig{
		{Name: "new-http", Type: "http", Kind: "mcp", URL: "https://example.com/api"},
		{Name: "legacy-general", Type: "github", Kind: "skill", URL: "github:mattpocock/skills"},
	}
	loader := NewResourceLoader()
	d := NewDiscoverySyncer(reg, loader, nil)
	if d.SourceCount() != 2 {
		t.Fatalf("expected 2 sources (1 new + 1 legacy), got %d", d.SourceCount())
	}
}

func TestDiscoverySyncer_UnknownSourceType(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	reg.System.DiscoverySources = []config.DiscoverySourceConfig{
		{Name: "bad", Type: "unknown", Kind: "skill", URL: "foo"},
	}
	d := NewDiscoverySyncer(reg, nil, nil)
	if d.SourceCount() != 0 {
		t.Fatalf("expected 0 sources (unknown type skipped), got %d", d.SourceCount())
	}
}

func TestDiscoverySyncer_GitHubWithoutLoader(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	reg.System.DiscoverySources = []config.DiscoverySourceConfig{
		{Name: "gh", Type: "github", Kind: "skill", URL: "github:owner/repo"},
	}
	d := NewDiscoverySyncer(reg, nil, nil)
	if d.SourceCount() != 0 {
		t.Fatalf("expected 0 sources (GitHub without loader skipped), got %d", d.SourceCount())
	}
}

// --- SyncAll: skill ingestion ---

func TestDiscoverySyncer_IngestSkills_Untrusted(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "remote-skills",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "skill-1", Name: "Skill 1", TrustLevel: TrustLevelUntrusted},
					{Kind: DiscoveryKindSkill, ID: "skill-2", Name: "Skill 2", TrustLevel: TrustLevelUntrusted},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.PendingSkills) != 2 {
		t.Fatalf("expected 2 pending skills, got %d", len(reg.PendingSkills))
	}
	if len(reg.Skills) != 0 {
		t.Fatalf("expected 0 registered skills, got %d", len(reg.Skills))
	}
	if reg.PendingSkills["skill-1"] == nil {
		t.Fatal("skill-1 not in PendingSkills")
	}
	if reg.PendingSkills["skill-2"] == nil {
		t.Fatal("skill-2 not in PendingSkills")
	}
}

func TestDiscoverySyncer_IngestSkills_Trusted(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "local-skills",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "local-1", Name: "Local 1", TrustLevel: TrustLevelTrusted},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.Skills) != 1 {
		t.Fatalf("expected 1 registered skill, got %d", len(reg.Skills))
	}
	if len(reg.PendingSkills) != 0 {
		t.Fatalf("expected 0 pending skills, got %d", len(reg.PendingSkills))
	}
	if reg.Skills["local-1"] == nil {
		t.Fatal("local-1 not in Skills")
	}
}

func TestDiscoverySyncer_IngestSkills_MixedTrust(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "mixed",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "trusted-1", Name: "Trusted", TrustLevel: TrustLevelTrusted},
					{Kind: DiscoveryKindSkill, ID: "untrusted-1", Name: "Untrusted", TrustLevel: TrustLevelUntrusted},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.Skills) != 1 {
		t.Fatalf("expected 1 registered skill, got %d", len(reg.Skills))
	}
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(reg.PendingSkills))
	}
}

// --- SyncAll: MCP ingestion ---

func TestDiscoverySyncer_IngestMCPs(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "glama",
				kind: DiscoveryKindMCP,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindMCP, ID: "mcp-1", Name: "MCP 1", URL: "https://example.com/mcp1"},
					{Kind: DiscoveryKindMCP, ID: "mcp-2", Name: "MCP 2", URL: "https://example.com/mcp2"},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.DynamicMCPs) != 2 {
		t.Fatalf("expected 2 DynamicMCPs, got %d", len(reg.DynamicMCPs))
	}

	mcp1 := reg.DynamicMCPs["mcp-1"]
	if mcp1 == nil {
		t.Fatal("mcp-1 not in DynamicMCPs")
	}
	if mcp1.MountPolicy != config.MountPolicyAdminOnly {
		t.Fatalf("expected admin_only mount policy, got %q", mcp1.MountPolicy)
	}
	if mcp1.Source != config.SourceExternalDynamic {
		t.Fatalf("expected external_dynamic source, got %q", mcp1.Source)
	}
	if mcp1.CredentialScope != config.CredentialOperator {
		t.Fatalf("expected operator credential scope, got %q", mcp1.CredentialScope)
	}
	if mcp1.URL != "https://example.com/mcp1" {
		t.Fatalf("expected URL https://example.com/mcp1, got %q", mcp1.URL)
	}
}

func TestDiscoverySyncer_IngestMCPs_AlwaysAdminOnly(t *testing.T) {
	// Even if a source is trusted, discovered MCPs must be admin_only.
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "trusted-mcp",
				kind: DiscoveryKindMCP,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindMCP, ID: "mcp-trusted", Name: "Trusted MCP", TrustLevel: TrustLevelTrusted, URL: "https://example.com"},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	mcp := reg.DynamicMCPs["mcp-trusted"]
	if mcp == nil {
		t.Fatal("trusted MCP not in DynamicMCPs")
	}
	if mcp.MountPolicy != config.MountPolicyAdminOnly {
		t.Fatalf("discovered MCP must always be admin_only, got %q", mcp.MountPolicy)
	}
}

// --- SyncAll: error handling ---

func TestDiscoverySyncer_SourceError(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "failing",
				kind: DiscoveryKindSkill,
				err:  context.DeadlineExceeded,
			},
			&mockDiscoverySource{
				name: "ok",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "skill-ok", Name: "OK", TrustLevel: TrustLevelUntrusted},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill from the working source, got %d", len(reg.PendingSkills))
	}
}

func TestDiscoverySyncer_EmptySources(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources:  []DiscoverySource{},
	}
	d.SyncAll() // should not panic
}

func TestDiscoverySyncer_NilItems(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name:  "empty",
				kind:  DiscoveryKindSkill,
				items: []DiscoveredItem{},
			},
		},
	}
	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.Skills) != 0 || len(reg.PendingSkills) != 0 {
		t.Fatal("expected no skills from empty source")
	}
}

// --- discoveredItemToMCPDef ---

func TestDiscoveredItemToMCPDef(t *testing.T) {
	item := DiscoveredItem{
		Kind:     DiscoveryKindMCP,
		ID:       "test-mcp",
		Name:     "Test MCP",
		URL:      "https://example.com/mcp",
		Command:  "node",
		Args:     []string{"server.js"},
		Env:      map[string]string{"API_KEY": "secret"},
		Provides: []string{"search", "fetch"},
	}

	mcp := discoveredItemToMCPDef(item)

	if mcp.ID != "test-mcp" {
		t.Fatalf("expected ID test-mcp, got %q", mcp.ID)
	}
	if mcp.URL != "https://example.com/mcp" {
		t.Fatalf("expected URL, got %q", mcp.URL)
	}
	if mcp.Command != "node" {
		t.Fatalf("expected Command node, got %q", mcp.Command)
	}
	if len(mcp.Args) != 1 || mcp.Args[0] != "server.js" {
		t.Fatalf("expected Args [server.js], got %v", mcp.Args)
	}
	if mcp.Env["API_KEY"] != "secret" {
		t.Fatalf("expected Env API_KEY=secret, got %v", mcp.Env)
	}
	if len(mcp.Provides) != 2 {
		t.Fatalf("expected 2 provides, got %d", len(mcp.Provides))
	}
	if mcp.MountPolicy != config.MountPolicyAdminOnly {
		t.Fatalf("expected admin_only, got %q", mcp.MountPolicy)
	}
	if mcp.Source != config.SourceExternalDynamic {
		t.Fatalf("expected external_dynamic, got %q", mcp.Source)
	}
	if mcp.CredentialScope != config.CredentialOperator {
		t.Fatalf("expected operator, got %q", mcp.CredentialScope)
	}
}

// --- trustedSourceWrapper ---

func TestTrustedSourceWrapper(t *testing.T) {
	inner := &mockDiscoverySource{
		name: "remote",
		kind: DiscoveryKindSkill,
		items: []DiscoveredItem{
			{Kind: DiscoveryKindSkill, ID: "s1", Name: "S1", TrustLevel: TrustLevelUntrusted},
		},
	}
	wrapped := &trustedSourceWrapper{inner: inner}

	if wrapped.Name() != "remote" {
		t.Fatalf("expected name remote, got %q", wrapped.Name())
	}
	if wrapped.Kind() != DiscoveryKindSkill {
		t.Fatalf("expected kind skill, got %q", wrapped.Kind())
	}

	items, err := wrapped.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].TrustLevel != TrustLevelTrusted {
		t.Fatalf("expected trusted, got %q", items[0].TrustLevel)
	}
}

// --- buildSource ---

func TestBuildSource_HTTP(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{registry: reg}
	cfg := config.DiscoverySourceConfig{
		Name: "test-http",
		Type: "http",
		Kind: "mcp",
		URL:  "https://example.com/api",
	}
	src, err := d.buildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.Name() != "test-http" {
		t.Fatalf("expected name test-http, got %q", src.Name())
	}
	if src.Kind() != DiscoveryKindMCP {
		t.Fatalf("expected kind mcp, got %q", src.Kind())
	}
}

func TestBuildSource_Local(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{registry: reg}
	cfg := config.DiscoverySourceConfig{
		Name: "test-local",
		Type: "local",
		Kind: "skill",
		Path: "/tmp/skills",
	}
	src, err := d.buildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.Name() != "test-local" {
		t.Fatalf("expected name test-local, got %q", src.Name())
	}
	if src.Kind() != DiscoveryKindSkill {
		t.Fatalf("expected kind skill, got %q", src.Kind())
	}
}

func TestBuildSource_HTTP_Trusted(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{registry: reg}
	cfg := config.DiscoverySourceConfig{
		Name:    "trusted-http",
		Type:    "http",
		Kind:    "skill",
		URL:     "https://example.com/api",
		Trusted: true,
	}
	src, err := d.buildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be wrapped in trustedSourceWrapper
	if _, ok := src.(*trustedSourceWrapper); !ok {
		t.Fatal("expected trustedSourceWrapper for trusted HTTP source")
	}
}

func TestBuildSource_UnknownType(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{registry: reg}
	cfg := config.DiscoverySourceConfig{
		Name: "bad",
		Type: "unknown",
		Kind: "skill",
	}
	_, err := d.buildSource(cfg, nil)
	if err == nil {
		t.Fatal("expected error for unknown source type")
	}
}

// --- Lifecycle ---

func TestDiscoverySyncer_StartStop(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := NewDiscoverySyncer(reg, nil, nil)
	d.Start()
	d.Stop() // should not panic or hang
}
