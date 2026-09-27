package core

import (
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/daybeam/vortex/config"
)

// validateToolArgs validates tool call arguments against the tool's JSON Schema
// before dispatching the call to the MCP server. This prevents wasted round-trips
// and gives clear 400-style errors instead of opaque server-side failures.
//
// Design decisions:
//   - Zero-trust default: if no schema is available for the tool, validation is
//     skipped (backward compatible — we don't break tools that don't expose schemas).
//   - The schema is looked up from mcp.FullToolDefinitions, which is populated
//     during tool discovery (discoverRemoteMCPTools for URL-based MCPs, or
//     ListTools for stdio-based MCPs).
//   - Uses santhosh-tekuri/jsonschema/v6 (already an indirect dependency via
//     mark3labs/mcp-go/server) — no new dependency added.
func validateToolArgs(mcp *config.MCPDef, toolName string, args map[string]any) error {
	if mcp == nil {
		return nil
	}

	// Find the tool's input schema from discovered tool definitions.
	var schema map[string]any
	for _, td := range mcp.FullToolDefinitions {
		if td.Name == toolName {
			schema = td.InputSchema
			break
		}
	}
	if schema == nil || len(schema) == 0 {
		return nil // no schema available — skip validation
	}

	// Compile the schema and validate the arguments.
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", schema); err != nil {
		// If we can't compile the schema, don't block the call — just skip.
		// A malformed schema is a server-side issue, not a caller error.
		return nil
	}
	sch, err := compiler.Compile("schema.json")
	if err != nil {
		return nil // same rationale: skip on malformed schema
	}

	if err := sch.Validate(args); err != nil {
		return fmt.Errorf("validateToolArgs: mcp %q tool %q: invalid arguments: %w", mcp.ID, toolName, err)
	}
	return nil
}
