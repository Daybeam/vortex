package core

import (
	"testing"
)

func TestSanitizeMCPToolText_StripsTagBlock(t *testing.T) {
	in := "a\U000E0061b" // U+E0061 = TAG character for 'a'
	want := "ab"
	if got := SanitizeMCPToolText(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeMCPToolText_StripsZeroWidth(t *testing.T) {
	in := "a\u200Bb\u200Cc\u200Dd\uFEFFe\u2060f\u00ADg"
	want := "abcdefg"
	if got := SanitizeMCPToolText(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeMCPToolText_StripsControlChars(t *testing.T) {
	in := "a\x00b\x01c\td\n"
	want := "abc\td\n"
	if got := SanitizeMCPToolText(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeMCPToolText_PreservesVisibleUnicode(t *testing.T) {
	in := "你好世界 🌍 café"
	want := in
	if got := SanitizeMCPToolText(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeMCPToolSchema_StripsTagInNestedDefault(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"trigger": map[string]any{
				"default": "a\U000E0000b",
			},
		},
	}
	out := SanitizeMCPToolSchema(schema)
	props := out["properties"].(map[string]any)
	trigger := props["trigger"].(map[string]any)
	if got := trigger["default"].(string); got != "ab" {
		t.Errorf("nested default: got %q, want %q", got, "ab")
	}
}

func TestSanitizeMCPToolSchema_StripsTagInEnum(t *testing.T) {
	schema := map[string]any{
		"enum": []any{"a\U000E0000", "b"},
	}
	out := SanitizeMCPToolSchema(schema)
	enum := out["enum"].([]any)
	if enum[0] != "a" || enum[1] != "b" {
		t.Errorf("enum: got %v, want [a b]", enum)
	}
}

func TestSanitizeMCPToolSchema_StripsTagInPropertyKey(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"invisible\U000E0061key": map[string]any{"type": "string"},
		},
	}
	out := SanitizeMCPToolSchema(schema)
	props := out["properties"].(map[string]any)
	if _, ok := props["invisiblekey"]; !ok {
		t.Errorf("sanitized key not found: got keys %v", keysOf(props))
	}
	if _, ok := props["invisible\U000E0061key"]; ok {
		t.Error("original TAG-laden key should not exist in output")
	}
}

func TestSanitizeMCPToolSchema_DeepNesting(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"x": map[string]any{
				"items": map[string]any{
					"properties": map[string]any{
						"y": map[string]any{
							"default": "hidden\u200B",
						},
					},
				},
			},
		},
	}
	out := SanitizeMCPToolSchema(schema)
	x := out["properties"].(map[string]any)["x"].(map[string]any)
	items := x["items"].(map[string]any)
	yProps := items["properties"].(map[string]any)["y"].(map[string]any)
	if got := yProps["default"].(string); got != "hidden" {
		t.Errorf("deep nested default: got %q, want %q", got, "hidden")
	}
}

func TestSanitizeMCPToolSchema_NonStringLeavesUnchanged(t *testing.T) {
	schema := map[string]any{
		"type":     "string",
		"required": true,
		"min":      float64(0),
		"nullable": nil,
	}
	out := SanitizeMCPToolSchema(schema)
	if out["type"] != "string" {
		t.Errorf("type: got %v", out["type"])
	}
	if out["required"] != true {
		t.Errorf("required: got %v", out["required"])
	}
	if out["min"] != float64(0) {
		t.Errorf("min: got %v", out["min"])
	}
	if out["nullable"] != nil {
		t.Errorf("nullable: got %v", out["nullable"])
	}
}

func TestSanitizeMCPToolSchema_DoesNotMutateInput(t *testing.T) {
	original := map[string]any{
		"properties": map[string]any{
			"k\U000E0061": "v\u200B",
		},
	}
	_ = SanitizeMCPToolSchema(original)
	props := original["properties"].(map[string]any)
	for k := range props {
		if k != "k\U000E0061" {
			t.Errorf("input mutated: key changed to %q", k)
		}
	}
	if props["k\U000E0061"] != "v\u200B" {
		t.Errorf("input mutated: value changed to %v", props["k\U000E0061"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestVerifyApprovalFidelity_CleanStrings(t *testing.T) {
	if !VerifyApprovalFidelity("clean_name", "clean description") {
		t.Error("clean strings should pass fidelity check")
	}
}

func TestVerifyApprovalFidelity_TagInName(t *testing.T) {
	if VerifyApprovalFidelity("a\U000E0061b", "clean") {
		t.Error("TAG block in name should fail fidelity check")
	}
}

func TestVerifyApprovalFidelity_TagInDesc(t *testing.T) {
	if VerifyApprovalFidelity("clean", "a\U000E0000b") {
		t.Error("TAG block in description should fail fidelity check")
	}
}

func TestVerifyApprovalFidelity_ZeroWidthInName(t *testing.T) {
	if VerifyApprovalFidelity("a\u200Bb", "clean") {
		t.Error("zero-width space in name should fail fidelity check")
	}
}

func TestVerifySchemaFidelity_CleanSchema(t *testing.T) {
	schema := map[string]any{
		"type": "string",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "user name"},
		},
	}
	if !VerifySchemaFidelity(schema) {
		t.Error("clean schema should pass fidelity check")
	}
}

func TestVerifySchemaFidelity_TagInSchemaValue(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"trigger": map[string]any{"default": "a\U000E0000b"},
		},
	}
	if VerifySchemaFidelity(schema) {
		t.Error("TAG block in schema value should fail fidelity check")
	}
}

func TestVerifySchemaFidelity_TagInSchemaKey(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"hidden\U000E0061key": map[string]any{"type": "string"},
		},
	}
	if VerifySchemaFidelity(schema) {
		t.Error("TAG block in schema key should fail fidelity check")
	}
}
