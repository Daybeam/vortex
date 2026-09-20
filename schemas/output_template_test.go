package schemas

import "testing"

// TestBuildOutputConstraintWithTemplate_EmptyFallsBackToDefault confirms
// the zero-config path (no override configured) is byte-identical to
// calling BuildOutputConstraint directly -- this is what every existing
// deployment sees until someone opts in by creating
// workspace/prompts/output_contract.txt.
func TestBuildOutputConstraintWithTemplate_EmptyFallsBackToDefault(t *testing.T) {
	got := BuildOutputConstraintWithTemplate("analyze", "")
	want := BuildOutputConstraint("analyze")
	if got != want {
		t.Fatalf("expected empty template to fall back to BuildOutputConstraint verbatim")
	}
}

// TestBuildOutputConstraintWithTemplate_SubstitutesTokens confirms both
// placeholder tokens are replaced and that a literal '%' character in the
// template survives untouched (i.e. this uses strings.Replace, not
// fmt.Sprintf -- a template author's stray '%' must never be interpreted
// as a format verb, which would either corrupt the output or panic).
func TestBuildOutputConstraintWithTemplate_SubstitutesTokens(t *testing.T) {
	tmpl := "Capability: {{CAPABILITY}} | Hint: {{RESULT_HINT}} | Literal: 100% done"
	got := BuildOutputConstraintWithTemplate("analyze", tmpl)
	wantHint := resultSchemaHints["analyze"]
	want := "Capability: analyze | Hint: " + wantHint + " | Literal: 100% done"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestBuildOutputConstraintWithTemplate_UnknownCapabilityUsesGenericHint
// mirrors BuildOutputConstraint's own fallback for a capability with no
// entry in resultSchemaHints.
func TestBuildOutputConstraintWithTemplate_UnknownCapabilityUsesGenericHint(t *testing.T) {
	got := BuildOutputConstraintWithTemplate("nonexistent_capability", "{{RESULT_HINT}}")
	want := `"...capability specific fields..."`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
