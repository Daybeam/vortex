package schemas

import (
	"testing"
)

func TestComputeSchemaFingerprint_Stability(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "alpha", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
		{Name: "beta", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"y": map[string]any{"type": "integer"}}}},
	}
	h1 := ComputeSchemaFingerprint(tools)

	shuffled := []ToolDefinition{tools[1], tools[0]}
	h2 := ComputeSchemaFingerprint(shuffled)

	if h1 != h2 {
		t.Errorf("fingerprint must be order-independent: got %s vs %s", h1, h2)
	}
}

func TestComputeSchemaFingerprint_SchemaChangeDetected(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "alpha", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
	}
	h1 := ComputeSchemaFingerprint(tools)

	tools[0].InputSchema["properties"].(map[string]any)["x"].(map[string]any)["type"] = "integer"
	h2 := ComputeSchemaFingerprint(tools)

	if h1 == h2 {
		t.Error("fingerprint must change when inputSchema changes")
	}
}

func TestComputeSchemaFingerprint_ToolAddedRemoved(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "alpha", InputSchema: map[string]any{"type": "object"}},
	}
	h1 := ComputeSchemaFingerprint(tools)

	tools2 := append(tools, ToolDefinition{Name: "beta", InputSchema: map[string]any{"type": "object"}})
	h2 := ComputeSchemaFingerprint(tools2)

	if h1 == h2 {
		t.Error("fingerprint must change when a tool is added")
	}
}

func TestComputeSurfaceFingerprint_DescriptionChange(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "alpha", Description: "old desc", InputSchema: map[string]any{"type": "object"}},
	}
	schemaHash1 := ComputeSchemaFingerprint(tools)
	surfaceHash1 := ComputeSurfaceFingerprint(tools)

	tools[0].Description = "new desc"
	schemaHash2 := ComputeSchemaFingerprint(tools)
	surfaceHash2 := ComputeSurfaceFingerprint(tools)

	if schemaHash1 != schemaHash2 {
		t.Error("SchemaHash must NOT change when only description changes")
	}
	if surfaceHash1 == surfaceHash2 {
		t.Error("SurfaceHash must change when description changes")
	}
}

func TestComputeSurfaceFingerprint_AnnotationFlip(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "alpha", Description: "desc", InputSchema: map[string]any{"type": "object"},
			Annotations: &ToolAnnotations{ReadOnlyHint: true}},
	}
	schemaHash1 := ComputeSchemaFingerprint(tools)
	surfaceHash1 := ComputeSurfaceFingerprint(tools)

	tools[0].Annotations.ReadOnlyHint = false
	schemaHash2 := ComputeSchemaFingerprint(tools)
	surfaceHash2 := ComputeSurfaceFingerprint(tools)

	if schemaHash1 != schemaHash2 {
		t.Error("SchemaHash must NOT change when only annotation flips")
	}
	if surfaceHash1 == surfaceHash2 {
		t.Error("SurfaceHash must change when annotation flips")
	}
}

func TestFingerprintDiff(t *testing.T) {
	old := []ToolDefinition{
		{Name: "alpha", InputSchema: map[string]any{"type": "object"}},
		{Name: "beta", InputSchema: map[string]any{"type": "object"}},
		{Name: "gamma", InputSchema: map[string]any{"type": "object"}},
	}
	new := []ToolDefinition{
		{Name: "alpha", InputSchema: map[string]any{"type": "object"}},
		{Name: "beta", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
		{Name: "delta", InputSchema: map[string]any{"type": "object"}},
	}

	diff := FingerprintDiff(old, new)

	want := []string{"+delta", "-gamma", "~beta"}
	if len(diff) != len(want) {
		t.Fatalf("expected %d diffs, got %d: %v", len(want), len(diff), diff)
	}
	for i, w := range want {
		if diff[i] != w {
			t.Errorf("diff[%d] = %s, want %s", i, diff[i], w)
		}
	}
}

func TestPrecedentPassesSchemaGate(t *testing.T) {
	current := map[string]string{"mcpA": "hashA1", "mcpB": "hashB1"}

	tests := []struct {
		name    string
		recorded map[string]string
		current  map[string]string
		want     bool
	}{
		{"unbound legacy passes", nil, current, true},
		{"empty recorded passes", map[string]string{}, current, true},
		{"all match passes", map[string]string{"mcpA": "hashA1", "mcpB": "hashB1"}, current, true},
		{"hash mismatch drops", map[string]string{"mcpA": "hashA1", "mcpB": "stale"}, current, false},
		{"MCP removed drops", map[string]string{"mcpA": "hashA1", "mcpC": "hashC1"}, current, false},
		{"partial match still drops", map[string]string{"mcpA": "hashA1", "mcpB": "stale"}, current, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PrecedentPassesSchemaGate(tc.recorded, tc.current)
			if got != tc.want {
				t.Errorf("PrecedentPassesSchemaGate(%v, %v) = %v, want %v",
					tc.recorded, tc.current, got, tc.want)
			}
		})
	}
}

func TestSchemaHashesFromTools(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "alpha", InputSchema: map[string]any{"type": "object"}},
	}
	hashes := SchemaHashesFromTools("mcpA", tools)
	if len(hashes) != 1 {
		t.Fatalf("expected 1 hash, got %d", len(hashes))
	}
	if hashes["mcpA"] != ComputeSchemaFingerprint(tools) {
		t.Error("SchemaHashesFromTools must return the SchemaFingerprint")
	}
}

func TestCanonicalJSON_Deterministic(t *testing.T) {
	m1 := map[string]any{"b": 1, "a": 2, "c": map[string]any{"z": 1, "y": 2}}
	m2 := map[string]any{"a": 2, "c": map[string]any{"y": 2, "z": 1}, "b": 1}
	if canonicalJSON(m1) != canonicalJSON(m2) {
		t.Error("canonicalJSON must be key-order independent")
	}
}
