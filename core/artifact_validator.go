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
	"sort"

	"github.com/daybeam/vortex/schemas"
)

// ArtifactProblemReason classifies one way an artifact can fail its contract.
type ArtifactProblemReason string

const (
	// ProblemUnreadable means the file could not be read at all.
	ProblemUnreadable ArtifactProblemReason = "unreadable"
	// ProblemNotJSON means the bytes are not a JSON object.
	ProblemNotJSON ArtifactProblemReason = "not-json"
	// ProblemMissing means a field listed in RequiredFields is absent.
	ProblemMissing ArtifactProblemReason = "missing-field"
	// ProblemTypeSchemaAbsent means a field listed only in TypeSchema is absent.
	ProblemTypeSchemaAbsent ArtifactProblemReason = "type-schema-absent"
	// ProblemWrongType means the field is present with another JSON type.
	ProblemWrongType ArtifactProblemReason = "wrong-type"
)

// ArtifactProblem is a single, addressable breach of an artifact contract.
//
// ValidateArtifact needs only "is this artifact acceptable"; the completion
// contract needs "what exactly is wrong, so the model can fix it". Returning a
// struct instead of a string keeps the second caller from parsing the first
// one's prose (→147: a message is not a data structure).
type ArtifactProblem struct {
	Field  string                // empty when the problem is the file itself
	Reason ArtifactProblemReason // one of the constants above
	Detail string                // underlying error, or the actual JSON type
}

// InspectArtifact returns every problem with the artifact described by ac, in a
// deterministic order: file-level first, then RequiredFields in declaration
// order, then TypeSchema by sorted field name.
//
// ValidateArtifact is implemented on top of this rather than beside it. Two
// implementations of "does this artifact satisfy its fields" is exactly how one
// of them silently stops being applied after the next edit (→131).
func InspectArtifact(ac schemas.ArtifactContract) []ArtifactProblem {
	// Fast path: no constraints declared — nothing to validate
	if len(ac.RequiredFields) == 0 && len(ac.TypeSchema) == 0 {
		return nil
	}

	raw, err := os.ReadFile(ac.Path)
	if err != nil {
		return []ArtifactProblem{{Reason: ProblemUnreadable, Detail: err.Error()}}
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []ArtifactProblem{{Reason: ProblemNotJSON, Detail: err.Error()}}
	}

	var problems []ArtifactProblem
	seen := make(map[string]struct{}, len(ac.RequiredFields))
	for _, field := range ac.RequiredFields {
		seen[field] = struct{}{}
		if _, ok := doc[field]; !ok {
			problems = append(problems, ArtifactProblem{Field: field, Reason: ProblemMissing})
		}
	}

	fields := make([]string, 0, len(ac.TypeSchema))
	for field := range ac.TypeSchema {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		expectedType := ac.TypeSchema[field]
		val, ok := doc[field]
		if !ok {
			if _, declared := seen[field]; declared {
				continue // already reported as a missing required field
			}
			problems = append(problems, ArtifactProblem{Field: field, Reason: ProblemTypeSchemaAbsent})
			continue
		}
		if actualType := jsonGoType(val); actualType != expectedType {
			problems = append(problems, ArtifactProblem{
				Field:  field,
				Reason: ProblemWrongType,
				Detail: actualType,
			})
		}
	}

	return problems
}

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
	problems := InspectArtifact(ac)
	if len(problems) == 0 {
		return nil
	}

	first := problems[0]
	switch first.Reason {
	case ProblemUnreadable:
		return fmt.Errorf("artifact %s: cannot read file for validation: %s", ac.Path, first.Detail)
	case ProblemNotJSON:
		return fmt.Errorf("artifact %s: %s: content is not valid JSON: %s",
			ac.Path, schemas.FailureSchemaViolation, first.Detail)
	case ProblemWrongType:
		return fmt.Errorf("artifact %s: %s: field %q expected type %q, got %q",
			ac.Path, schemas.FailureSchemaViolation, first.Field, ac.TypeSchema[first.Field], first.Detail)
	case ProblemTypeSchemaAbsent:
		return fmt.Errorf("artifact %s: %s: field %q declared in type_schema but absent",
			ac.Path, schemas.FailureSchemaViolation, first.Field)
	default:
		return fmt.Errorf("artifact %s: %s: missing required field %q",
			ac.Path, schemas.FailureSchemaViolation, first.Field)
	}
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
