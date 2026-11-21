package schemas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ComputeSchemaFingerprint produces the hard-signal hash: changes when tool
// names, parameter names, required fields, or additionalProperties change.
// Tools are sorted by name for determinism. Each tool contributes
// name + "\x00" + canonicalJSON(inputSchema).
func ComputeSchemaFingerprint(tools []ToolDefinition) string {
	sorted := make([]ToolDefinition, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	for _, t := range sorted {
		h.Write([]byte(t.Name))
		h.Write([]byte{0x00})
		h.Write([]byte(canonicalJSON(t.InputSchema)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeSurfaceFingerprint produces the soft-signal hash: changes when
// descriptions are rewritten or annotations flip (e.g. readOnlyHint true→false).
// Covers the full ToolDefinition surface that influences tool selection and
// confirmation gating, but not the call contract.
func ComputeSurfaceFingerprint(tools []ToolDefinition) string {
	sorted := make([]ToolDefinition, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	for _, t := range sorted {
		h.Write([]byte(t.Name))
		h.Write([]byte{0x00})
		h.Write([]byte(t.Description))
		h.Write([]byte{0x00})
		h.Write([]byte(canonicalJSON(t.InputSchema)))
		h.Write([]byte{0x00})
		if t.Annotations != nil {
			h.Write([]byte(canonicalJSON(annotationsToMap(t.Annotations))))
		}
		h.Write([]byte{0x00})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON serializes a map[string]any with recursively sorted keys and
// no extraneous whitespace, so semantically identical schemas produce identical
// bytes regardless of key order or formatting differences.
func canonicalJSON(v any) string {
	b, err := json.Marshal(canonicalize(v))
	if err != nil {
		return ""
	}
	return string(b)
}

func canonicalize(v any) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(val))
		for _, k := range keys {
			out[k] = canonicalize(val[k])
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = canonicalize(item)
		}
		return out
	default:
		return v
	}
}

func annotationsToMap(a *ToolAnnotations) map[string]any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"title":           a.Title,
		"readOnlyHint":    a.ReadOnlyHint,
		"destructiveHint": a.DestructiveHint,
		"idempotentHint":  a.IdempotentHint,
		"openWorldHint":   a.OpenWorldHint,
	}
}

// FingerprintDiff returns the list of tool names that were added, removed, or
// whose SchemaHash-relevant fields changed between two tool sets. Used for
// logging EventMCPToolsChanged.
func FingerprintDiff(old, new []ToolDefinition) []string {
	oldMap := make(map[string]ToolDefinition, len(old))
	for _, t := range old {
		oldMap[t.Name] = t
	}
	newMap := make(map[string]ToolDefinition, len(new))
	for _, t := range new {
		newMap[t.Name] = t
	}

	var diff []string
	for name, nt := range newMap {
		ot, exists := oldMap[name]
		if !exists {
			diff = append(diff, "+"+name)
			continue
		}
		if canonicalJSON(ot.InputSchema) != canonicalJSON(nt.InputSchema) {
			diff = append(diff, "~"+name)
		}
	}
	for name := range oldMap {
		if _, exists := newMap[name]; !exists {
			diff = append(diff, "-"+name)
		}
	}
	sort.Strings(diff)
	return diff
}

// SchemaHashesFromTools builds a map of mcpID → SchemaFingerprint for a set
// of MCP definitions. Used at DecisionPrecedent write time to snapshot which
// tool versions the precedent was recorded under.
func SchemaHashesFromTools(mcpID string, tools []ToolDefinition) map[string]string {
	return map[string]string{mcpID: ComputeSchemaFingerprint(tools)}
}

// PrecedentPassesSchemaGate checks whether a precedent's recorded schema
// hashes still match the current registry. Returns true if all recorded MCP
// hashes match, or if the precedent has no hash bindings (legacy data).
// Returns false if any MCP hash differs or a referenced MCP no longer exists.
func PrecedentPassesSchemaGate(
	recorded map[string]string,
	currentFingerprints map[string]string,
) bool {
	if len(recorded) == 0 {
		return true // unbound legacy data — pass (counted separately)
	}
	for mcpID, hash := range recorded {
		cur, exists := currentFingerprints[mcpID]
		if !exists {
			return false // MCP removed — drop
		}
		if cur != hash {
			return false // schema changed — drop
		}
	}
	return true
}

