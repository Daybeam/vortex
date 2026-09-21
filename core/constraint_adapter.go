package core

import (
	"strings"
)

// ConstraintAdapter translates a high-level JSON Schema into provider-specific constraints.
type ConstraintAdapter struct{}

func NewConstraintAdapter() *ConstraintAdapter {
	return &ConstraintAdapter{}
}

// Resolve converts a JSON Schema into the correct field name and value for the given provider.
func (a *ConstraintAdapter) Resolve(provider string, schema map[string]any) (string, any) {
	if schema == nil {
		return "", nil
	}

	switch provider {
	case "ollama", "llama.cpp":
		// Translate JSON Schema -> GBNF
		gbnf := a.schemaToGBNF(schema)
		return "grammar", gbnf
	case "vllm":
		// vLLM uses guided_json for JSON Schema
		return "guided_json", schema
	case "openai", "deepseek":
		// OpenAI uses response_format: { type: "json_schema", json_schema: { ... } }
		return "response_format", map[string]any{
			"type": "json_object", // Simplification, should ideally use json_schema for newer models
		}
	case "gemini":
		// Gemini uses response_mime_type: "application/json" and response_schema
		return "response_schema", schema
	default:
		return "", nil
	}
}

// schemaToGBNF is a simplified JSON Schema to GBNF converter.
// It handles basic objects, arrays, strings, and numbers.
func (a *ConstraintAdapter) schemaToGBNF(schema map[string]any) string {
	var rules []string
	rules = append(rules, "root ::= object")

	// This is a very simplified recursive converter.
	// In a full implementation, we would track types to avoid duplicate rule definitions.

	// For the purpose of the orchestrator, we'll generate a simplified "Valid JSON" GBNF
	// unless the schema is highly specific.

	// a simple, robust GBNF for generic JSON objects:
	genericJSON := `
object ::= "{" space (pair ( "," space pair)*)? "}"
pair ::= string ":" space value
value ::= string | number | object | array
string ::= "\"" [^"]* "\""
number ::= "-"? [0-9]+ ("." [0-9]+)?
array ::= "[" space (value ( "," space value)*)? "]"
space ::= [ \t\n\r]*
`
	// If we wanted to be specific, we would iterate through schema["properties"]
	// and build rules like: pair_user ::= "\"user\"" ":" space string

	return "root ::= object\n" + strings.TrimSpace(genericJSON)
}
