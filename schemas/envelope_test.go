package schemas

import (
	"strings"
	"testing"
)

func TestSemanticEnvelope_ToMarkdown(t *testing.T) {
	e := &SemanticEnvelope{
		Status:    "ok",
		Source:    "mcp:test",
		Reason:    "Test purpose",
		Content:   "Hello world",
		Citations: []string{"test.go:10"},
	}

	md := e.ToMarkdown("test_tool")

	expected := []string{
		"### [TOOL RESULT] test_tool",
		"Test purpose",
		"✅ OK",
		"来源：mcp:test",
		"test.go:10",
		"Hello world",
	}

	for _, exp := range expected {
		if !strings.Contains(md, exp) {
			t.Errorf("expected markdown to contain %q, but it didn't", exp)
		}
	}
}

func TestSemanticEnvelope_ErrorStatus(t *testing.T) {
	e := &SemanticEnvelope{
		Status:  "error",
		Reason:  "Failing",
		Content: "Fatal Error",
	}

	md := e.ToMarkdown("fail_tool")
	if !strings.Contains(md, "❌ ERROR") {
		t.Errorf("expected error icon and text")
	}
}
