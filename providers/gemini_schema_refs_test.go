package providers

import (
	"encoding/json"
	"testing"
	"time"
)

// TestSanitizeGeminiSchema_ResolvesRefsAgainstDefs is a direct regression
// test for the bug found 2026-08-15: Gemini's generateContent API rejects
// the ENTIRE request (a 400 covering all function_declarations, not just
// the offending one) if any tool's parameter schema anywhere in the tree
// contains $ref/$defs. Pydantic (and other JSON Schema generators) commonly
// emit these for Dict[str, SomeModel]-shaped fields, e.g.:
//
//	{"type": "object", "additionalProperties": {"$ref": "#/$defs/Foo"}, "$defs": {"Foo": {...}}}
//
// The fix must inline the referenced type's actual content (not just delete
// the $ref key, which would silently discard real type information), while
// still stripping the $defs/$ref/definitions machinery itself from the
// final output (since that machinery is what Gemini rejects, regardless of
// whether it's actually being pointed at).
func TestSanitizeGeminiSchema_ResolvesRefsAgainstDefs(t *testing.T) {
	inputJSON := `
	{
		"$defs": {
			"Foo": {
				"type": "object",
				"properties": {
					"name": {"type": "string"},
					"count": {"type": "integer"}
				}
			}
		},
		"type": "object",
		"properties": {
			"items": {
				"type": "object",
				"additionalProperties": {"$ref": "#/$defs/Foo"}
			},
			"single": {"$ref": "#/$defs/Foo"}
		}
	}`

	var input map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		t.Fatalf("failed to unmarshal input: %v", err)
	}

	sanitized := sanitizeGeminiSchema(input)

	// $defs must be gone from the output entirely -- Gemini rejects it even
	// when nothing points at it anymore.
	if _, ok := sanitized["$defs"]; ok {
		t.Error("expected $defs to be stripped from the sanitized schema")
	}

	props, ok := sanitized["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected sanitized[\"properties\"] to be a map, got %T", sanitized["properties"])
	}

	// "single": {"$ref": "#/$defs/Foo"} must be replaced with Foo's actual
	// resolved content, not just have the $ref key deleted (which would
	// leave an empty {} and silently discard the real type).
	single, ok := props["single"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties.single to be a map, got %T", props["single"])
	}
	if _, hasRef := single["$ref"]; hasRef {
		t.Error("expected $ref to be gone from properties.single after resolution")
	}
	if single["type"] != "object" {
		t.Errorf("expected properties.single to inline Foo's type=object, got %v", single["type"])
	}
	singleProps, ok := single["properties"].(map[string]any)
	if !ok || singleProps["name"] == nil {
		t.Errorf("expected properties.single to inline Foo's own properties (name/count), got %v", single["properties"])
	}

	// "items": {"additionalProperties": {"$ref": "#/$defs/Foo"}} is the
	// exact Dict[str, Foo] shape that triggered the real bug -- but
	// additionalProperties itself is a forbidden/stripped key regardless of
	// whether it holds a $ref, so the correct outcome here is that
	// "items.additionalProperties" is gone entirely (stripped as a
	// forbidden key, same as any other additionalProperties), not that it
	// contains a dangling or resolved $ref.
	items, ok := props["items"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties.items to be a map, got %T", props["items"])
	}
	if _, hasAP := items["additionalProperties"]; hasAP {
		t.Error("expected additionalProperties to be stripped from properties.items regardless of its $ref content")
	}
}

// TestSanitizeGeminiSchema_UnresolvableRefDegradesToObject covers a $ref
// that points at a name not present in $defs (malformed/unusual schema) --
// must degrade to a generic object schema rather than leaving the $ref key
// in place (which Gemini would reject) or panicking.
func TestSanitizeGeminiSchema_UnresolvableRefDegradesToObject(t *testing.T) {
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"broken": map[string]any{"$ref": "#/$defs/DoesNotExist"},
		},
	}
	sanitized := sanitizeGeminiSchema(input)
	props := sanitized["properties"].(map[string]any)
	broken, ok := props["broken"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties.broken to be a map, got %T", props["broken"])
	}
	if _, hasRef := broken["$ref"]; hasRef {
		t.Error("expected an unresolvable $ref to be replaced, not left in place")
	}
	if broken["type"] != "object" {
		t.Errorf("expected an unresolvable $ref to degrade to type=object, got %v", broken["type"])
	}
}

// TestSanitizeGeminiSchema_NoRefsUnaffected confirms schemas with no
// $ref/$defs at all (the common case, and what the original
// TestSanitizeGeminiSchema already covers for the other forbidden keys)
// pass through resolveGeminiRefs with identical behavior to before this
// fix -- this rewrite must not change output for schemas that never had
// $ref/$defs in the first place.
func TestSanitizeGeminiSchema_NoRefsUnaffected(t *testing.T) {
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	sanitized := sanitizeGeminiSchema(input)
	if sanitized["type"] != "object" {
		t.Errorf("expected type=object to pass through unchanged, got %v", sanitized["type"])
	}
	props := sanitized["properties"].(map[string]any)
	name := props["name"].(map[string]any)
	if name["type"] != "string" {
		t.Errorf("expected properties.name.type=string to pass through unchanged, got %v", name["type"])
	}
}

// TestSanitizeGeminiSchema_CircularRefDoesNotHang guards against a
// pathological/malicious circular $ref (valid in general JSON Schema, used
// to express recursive types, but something Gemini's schema format can't
// represent either way) turning resolution into an infinite loop or stack
// overflow. Must terminate via maxGeminiRefDepth and degrade to a generic
// object rather than hanging or crashing.
func TestSanitizeGeminiSchema_CircularRefDoesNotHang(t *testing.T) {
	input := map[string]any{
		"$defs": map[string]any{
			"Node": map[string]any{
				"type": "object",
				"properties": map[string]any{
					// Node.properties.child refers back to Node itself.
					"child": map[string]any{"$ref": "#/$defs/Node"},
				},
			},
		},
		"$ref": "#/$defs/Node",
	}

	done := make(chan map[string]any, 1)
	go func() { done <- sanitizeGeminiSchema(input) }()

	select {
	case sanitized := <-done:
		if _, hasDefs := sanitized["$defs"]; hasDefs {
			t.Error("expected $defs to be stripped even from a circular-ref schema")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sanitizeGeminiSchema did not terminate on a circular $ref within the test timeout")
	}
}
