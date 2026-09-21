// Package core contains the deterministic artifact-constraint validator.
//
// Artifact Constraint Shield (arXiv:2608.24569 — When "Must" Becomes "Maybe")
//
// The problem: in multi-step LLM workflows, hard constraints (required fields,
// type contracts) attached to upstream step artifacts get weakened into
// soft suggestions as they pass through successive LLM handoffs. By the time
// a downstream step reads the artifact, the contract has degraded from
// "must contain risk_level" to "probably has risk_level somewhere".
//
// This file is the **deterministic** countermeasure: a pure-Go, zero-LLM,
// zero-external-dependency validator that checks on-disk artifacts against
// the declared ArtifactContract. It runs at the handoff boundary (between
// upstream and downstream steps) and returns FailureSchemaViolation on
// contract breach. The check is O(fields) — microsecond class.
//
// Design constraints (align with orchestrator-mcp-go architectural principles):
//
//   - Zero CGO, zero heavy dependencies — pure encoding/json + reflect.
//   - Deterministic — same input always yields same result, no LLM call.
//   - Read-only — validates, never mutates the artifact on disk.
//   - Idempotent — safe to call repeatedly on the same artifact.
//   - Graceful — if contract has no constraints, validation passes (no-op).
package core

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/daybeam/vortex/schemas"
)

// ValidateArtifact checks the on-disk file at ac.Path against the constraints
// declared in ac.RequiredFields and ac.TypeSchema.
//
// Returns nil when:
//   - the artifact file is missing or unreadable (defers to caller's file-exists check)
//   - the contract declares no constraints (empty RequiredFields AND empty TypeSchema)
//   - all required fields are present with matching types
//
// Returns an error containing FailureSchemaViolation when:
//   - a required field is absent
//   - a field's JSON type doesn't match its declared type
//   - the file content is not valid JSON (when constraints require JSON-level checks)
func ValidateArtifact(ac schemas.ArtifactContract) error {
	// Fast path: no constraints declared — nothing to validate
	if len(ac.RequiredFields) == 0 && len(ac.TypeSchema) == 0 {
		return nil
	}

	// Read the artifact content
	raw, err := os.ReadFile(ac.Path)
	if err != nil {
		return fmt.Errorf("artifact %s: cannot read file for validation: %w", ac.Path, err)
	}

	// Parse JSON
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("artifact %s: %s: content is not valid JSON: %v",
			ac.Path, schemas.FailureSchemaViolation, err)
	}

	// Check required fields presence
	for _, field := range ac.RequiredFields {
		if _, ok := doc[field]; !ok {
			return fmt.Errorf("artifact %s: %s: missing required field %q",
				ac.Path, schemas.FailureSchemaViolation, field)
		}
	}

	// Check type schema
	for field, expectedType := range ac.TypeSchema {
		val, ok := doc[field]
		if !ok {
			// Field missing — already caught by RequiredFields, but be explicit
			// for cases where TypeSchema declares it but RequiredFields doesn't
			return fmt.Errorf("artifact %s: %s: field %q declared in type_schema but absent",
				ac.Path, schemas.FailureSchemaViolation, field)
		}
		actualType := jsonGoType(val)
		if actualType != expectedType {
			return fmt.Errorf("artifact %s: %s: field %q expected type %q, got %q",
				ac.Path, schemas.FailureSchemaViolation, field, expectedType, actualType)
		}
	}

	return nil
}

// jsonGoType returns a JSON-type string for the given value:
// "string", "number", "boolean", "array", "object", "null".
func jsonGoType(v interface{}) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case string:
		return "string"
	case float64, float32, int, int64, int32:
		return "number"
	case bool:
		return "boolean"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}
