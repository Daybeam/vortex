package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

func TestDirectedEngineCollectCurrentSchemaFingerprints(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"mcpA": {ID: "mcpA", SchemaFingerprint: "hashA"},
			"mcpB": {ID: "mcpB", SchemaFingerprint: "hashB"},
			"mcpC": {ID: "mcpC"},
		},
		DynamicMCPs: map[string]*config.MCPDef{
			"mcpD": {ID: "mcpD", SchemaFingerprint: "hashD"},
		},
	}

	engine := &DirectedEngine{registry: reg}
	fingerprints := engine.collectCurrentSchemaFingerprints()

	if len(fingerprints) != 3 {
		t.Fatalf("expected 3 fingerprints (mcpA, mcpB, mcpD), got %d: %v", len(fingerprints), fingerprints)
	}
	if fingerprints["mcpA"] != "hashA" {
		t.Errorf("mcpA = %s; want hashA", fingerprints["mcpA"])
	}
	if fingerprints["mcpB"] != "hashB" {
		t.Errorf("mcpB = %s; want hashB", fingerprints["mcpB"])
	}
	if fingerprints["mcpD"] != "hashD" {
		t.Errorf("mcpD = %s; want hashD", fingerprints["mcpD"])
	}
	if _, ok := fingerprints["mcpC"]; ok {
		t.Error("mcpC has empty fingerprint, should not be included")
	}
}

func TestDirectedEngineCollectCurrentSchemaFingerprints_NilRegistry(t *testing.T) {
	engine := &DirectedEngine{registry: nil}
	if fps := engine.collectCurrentSchemaFingerprints(); fps != nil {
		t.Errorf("expected nil for nil registry, got %v", fps)
	}
}

func makeSchemaGateEngine(t *testing.T, reg *config.Registry, precedents ...store.AntiPatternPrecedent) *DirectedEngine {
	t.Helper()
	tmpDir := t.TempDir()
	logger := mustNewLogger(t, tmpDir, nil)

	apStore := store.NewAntiPatternStore(nil, nil)
	for _, p := range precedents {
		apStore.Upsert(context.Background(), p)
	}
	expStore := &store.ExperienceStore{AntiPatternStore: apStore}

	return &DirectedEngine{
		expStore: expStore,
		registry: reg,
		logger:   logger,
	}
}

func makeSchemaGatePromptAssembler(t *testing.T, reg *config.Registry, precedents ...store.AntiPatternPrecedent) *PromptAssembler {
	t.Helper()
	tmpDir := t.TempDir()
	logger := mustNewLogger(t, tmpDir, nil)

	apStore := store.NewAntiPatternStore(nil, nil)
	for _, p := range precedents {
		apStore.Upsert(context.Background(), p)
	}
	expStore := &store.ExperienceStore{AntiPatternStore: apStore}

	return NewPromptAssembler(expStore, nil, reg, logger)
}

func makeSchemaGateRegistry() *config.Registry {
	return &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"mcpA": {ID: "mcpA", SchemaFingerprint: "hashA_v1"},
			"mcpB": {ID: "mcpB", SchemaFingerprint: "hashB_v1"},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}
}

func TestBuildAntiPatternGuidance_SchemaGateMatch(t *testing.T) {
	reg := makeSchemaGateRegistry()
	precedent := store.AntiPatternPrecedent{
		ID:           "prec_match",
		AntiPattern:  "testing schema gate match",
		Symptom:      "symptom",
		CorrectPattern: "correct pattern",
		SchemaHashes: map[string]string{"mcpA": "hashA_v1", "mcpB": "hashB_v1"},
		Confidence:   0.9,
	}
	engine := makeSchemaGateEngine(t, reg, precedent)

	output := engine.buildAntiPatternGuidance("testing", "failure")
	if output == "" {
		t.Fatal("expected non-empty output for matching schema hashes")
	}
	if !strings.Contains(output, "testing schema gate match") {
		t.Errorf("output must contain the matching precedent's AntiPattern text:\n%s", output)
	}
}

func TestBuildAntiPatternGuidance_SchemaGateMismatch(t *testing.T) {
	reg := makeSchemaGateRegistry()
	precedent := store.AntiPatternPrecedent{
		ID:           "prec_mismatch",
		AntiPattern:  "testing schema gate mismatch",
		Symptom:      "symptom",
		CorrectPattern: "correct pattern",
		SchemaHashes: map[string]string{"mcpA": "hashA_v2"},
		Confidence:   0.9,
	}
	engine := makeSchemaGateEngine(t, reg, precedent)

	output := engine.buildAntiPatternGuidance("testing", "failure")
	if output != "" {
		t.Errorf("expected empty output when all precedents are quarantined, got:\n%s", output)
	}
}

func TestBuildAntiPatternGuidance_LegacyPrecedentsPass(t *testing.T) {
	reg := makeSchemaGateRegistry()
	precedent := store.AntiPatternPrecedent{
		ID:           "prec_legacy",
		AntiPattern:  "testing legacy precedent",
		Symptom:      "symptom",
		CorrectPattern: "correct pattern",
		SchemaHashes: nil,
		Confidence:   0.8,
	}
	engine := makeSchemaGateEngine(t, reg, precedent)

	output := engine.buildAntiPatternGuidance("testing", "failure")
	if output == "" {
		t.Fatal("expected non-empty output for legacy precedent (nil SchemaHashes)")
	}
	if !strings.Contains(output, "testing legacy precedent") {
		t.Errorf("output must contain the legacy precedent's AntiPattern text:\n%s", output)
	}
}

func TestBuildAntiPatternGuidance_MixedPrecedents(t *testing.T) {
	reg := makeSchemaGateRegistry()
	matching := store.AntiPatternPrecedent{
		ID:           "prec_match",
		AntiPattern:  "testing matching precedent",
		Symptom:      "symptom",
		CorrectPattern: "correct",
		SchemaHashes: map[string]string{"mcpA": "hashA_v1"},
		Confidence:   0.9,
	}
	mismatched := store.AntiPatternPrecedent{
		ID:           "prec_mismatch",
		AntiPattern:  "testing mismatched precedent",
		Symptom:      "symptom",
		CorrectPattern: "correct",
		SchemaHashes: map[string]string{"mcpA": "hashA_v2"},
		Confidence:   0.85,
	}
	legacy := store.AntiPatternPrecedent{
		ID:           "prec_legacy",
		AntiPattern:  "testing legacy precedent",
		Symptom:      "symptom",
		CorrectPattern: "correct",
		SchemaHashes: nil,
		Confidence:   0.8,
	}
	engine := makeSchemaGateEngine(t, reg, matching, mismatched, legacy)

	output := engine.buildAntiPatternGuidance("testing", "failure")
	if output == "" {
		t.Fatal("expected non-empty output with matching + legacy precedents")
	}
	if !strings.Contains(output, "testing matching precedent") {
		t.Errorf("output must contain matching precedent:\n%s", output)
	}
	if !strings.Contains(output, "testing legacy precedent") {
		t.Errorf("output must contain legacy precedent:\n%s", output)
	}
	if strings.Contains(output, "testing mismatched precedent") {
		t.Errorf("output must NOT contain mismatched precedent:\n%s", output)
	}
}

func TestBuildAntiPatternGuidance_MCPRemovedDrops(t *testing.T) {
	reg := makeSchemaGateRegistry()
	precedent := store.AntiPatternPrecedent{
		ID:           "prec_removed_mcp",
		AntiPattern:  "testing removed mcp precedent",
		Symptom:      "symptom",
		CorrectPattern: "correct",
		SchemaHashes: map[string]string{"mcpA": "hashA_v1", "mcpZ": "hashZ_v1"},
		Confidence:   0.9,
	}
	engine := makeSchemaGateEngine(t, reg, precedent)

	output := engine.buildAntiPatternGuidance("testing", "failure")
	if output != "" {
		t.Errorf("expected empty output when MCP is removed, got:\n%s", output)
	}
}

func TestResolveAntiPatternWarnings_SchemaGateMatch(t *testing.T) {
	reg := makeSchemaGateRegistry()
	precedent := store.AntiPatternPrecedent{
		ID:           "prec_match",
		AntiPattern:  "testing schema gate match",
		Symptom:      "symptom",
		CorrectPattern: "correct pattern",
		SchemaHashes: map[string]string{"mcpA": "hashA_v1", "mcpB": "hashB_v1"},
		Confidence:   0.9,
	}
	pa := makeSchemaGatePromptAssembler(t, reg, precedent)

	output := pa.resolveAntiPatternWarnings("testing", []string{"failure"})
	if output == "" {
		t.Fatal("expected non-empty output for matching schema hashes")
	}
	if !strings.Contains(output, "testing schema gate match") {
		t.Errorf("output must contain the matching precedent:\n%s", output)
	}
}

func TestResolveAntiPatternWarnings_SchemaGateMismatch(t *testing.T) {
	reg := makeSchemaGateRegistry()
	precedent := store.AntiPatternPrecedent{
		ID:           "prec_mismatch",
		AntiPattern:  "testing schema gate mismatch",
		Symptom:      "symptom",
		CorrectPattern: "correct pattern",
		SchemaHashes: map[string]string{"mcpA": "hashA_v2"},
		Confidence:   0.9,
	}
	pa := makeSchemaGatePromptAssembler(t, reg, precedent)

	output := pa.resolveAntiPatternWarnings("testing", []string{"failure"})
	if output != "" {
		t.Errorf("expected empty output when all precedents are quarantined, got:\n%s", output)
	}
}

func TestResolveAntiPatternWarnings_LegacyPrecedentsPass(t *testing.T) {
	reg := makeSchemaGateRegistry()
	precedent := store.AntiPatternPrecedent{
		ID:           "prec_legacy",
		AntiPattern:  "testing legacy precedent",
		Symptom:      "symptom",
		CorrectPattern: "correct pattern",
		SchemaHashes: nil,
		Confidence:   0.8,
	}
	pa := makeSchemaGatePromptAssembler(t, reg, precedent)

	output := pa.resolveAntiPatternWarnings("testing", []string{"failure"})
	if output == "" {
		t.Fatal("expected non-empty output for legacy precedent")
	}
	if !strings.Contains(output, "testing legacy precedent") {
		t.Errorf("output must contain the legacy precedent:\n%s", output)
	}
}

func TestAutoDepositDecisionRecovery_RecordsSchemaHashes(t *testing.T) {
	backend := &mockAntiPatternBackend{}
	apStore := store.NewAntiPatternStore(nil, backend)

	schemaHashes := map[string]string{"mcpA": "hashA_v1", "mcpB": "hashB_v1"}
	apStore.AutoDepositDecisionRecovery(
		context.Background(),
		"intent",
		"failure_mode",
		"trigger",
		"symptom",
		"recovery",
		schemaHashes,
	)

	if backend.UpsertedCount() != 1 {
		t.Fatalf("expected 1 upserted precedent, got %d", backend.UpsertedCount())
	}
	deposited := backend.Upserted()[0]
	if len(deposited.SchemaHashes) != 2 {
		t.Fatalf("expected 2 schema hashes, got %d: %v", len(deposited.SchemaHashes), deposited.SchemaHashes)
	}
	if deposited.SchemaHashes["mcpA"] != "hashA_v1" {
		t.Errorf("SchemaHashes[mcpA] = %s; want hashA_v1", deposited.SchemaHashes["mcpA"])
	}
	if deposited.SchemaHashes["mcpB"] != "hashB_v1" {
		t.Errorf("SchemaHashes[mcpB] = %s; want hashB_v1", deposited.SchemaHashes["mcpB"])
	}
}

func TestAutoDepositDecisionRecovery_NilSchemaHashes(t *testing.T) {
	backend := &mockAntiPatternBackend{}
	apStore := store.NewAntiPatternStore(nil, backend)

	apStore.AutoDepositDecisionRecovery(
		context.Background(),
		"intent",
		"failure_mode",
		"trigger",
		"symptom",
		"recovery",
		nil,
	)

	if backend.UpsertedCount() != 1 {
		t.Fatalf("expected 1 upserted precedent, got %d", backend.UpsertedCount())
	}
	deposited := backend.Upserted()[0]
	if deposited.SchemaHashes != nil {
		t.Errorf("expected nil SchemaHashes for nil input, got %v", deposited.SchemaHashes)
	}
}
