package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestValidateToolArgs_ValidArgs(t *testing.T) {
	mcp := &config.MCPDef{
		ID: "test-mcp",
		FullToolDefinitions: []schemas.ToolDefinition{
			{
				Name: "search",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []any{"query"},
				},
			},
		},
	}

	err := validateToolArgs(mcp, "search", map[string]any{"query": "hello"})
	if err != nil {
		t.Fatalf("expected nil error for valid args, got: %v", err)
	}
}

func TestValidateToolArgs_InvalidArgs_MissingRequired(t *testing.T) {
	mcp := &config.MCPDef{
		ID: "test-mcp",
		FullToolDefinitions: []schemas.ToolDefinition{
			{
				Name: "search",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []any{"query"},
				},
			},
		},
	}

	err := validateToolArgs(mcp, "search", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing required field, got nil")
	}
	// Error should mention the MCP ID and tool name for debuggability.
	if !strings.Contains(err.Error(), "test-mcp") || !strings.Contains(err.Error(), "search") {
		t.Fatalf("error should mention mcp ID and tool name, got: %v", err)
	}
}

func TestValidateToolArgs_InvalidArgs_WrongType(t *testing.T) {
	mcp := &config.MCPDef{
		ID: "test-mcp",
		FullToolDefinitions: []schemas.ToolDefinition{
			{
				Name: "search",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []any{"query"},
				},
			},
		},
	}

	// "query" should be string, but we pass a number.
	err := validateToolArgs(mcp, "search", map[string]any{"query": 42})
	if err == nil {
		t.Fatal("expected error for wrong type, got nil")
	}
}

func TestValidateToolArgs_NoSchema_SkipsValidation(t *testing.T) {
	// Tool has no InputSchema — validation should be skipped (backward compatible).
	mcp := &config.MCPDef{
		ID: "test-mcp",
		FullToolDefinitions: []schemas.ToolDefinition{
			{Name: "search"},
		},
	}

	err := validateToolArgs(mcp, "search", map[string]any{"anything": "goes"})
	if err != nil {
		t.Fatalf("expected nil error when no schema, got: %v", err)
	}
}

func TestValidateToolArgs_ToolNotFound_SkipsValidation(t *testing.T) {
	// Tool name not in FullToolDefinitions — validation should be skipped.
	mcp := &config.MCPDef{
		ID: "test-mcp",
		FullToolDefinitions: []schemas.ToolDefinition{
			{Name: "other_tool"},
		},
	}

	err := validateToolArgs(mcp, "search", map[string]any{})
	if err != nil {
		t.Fatalf("expected nil error when tool not found, got: %v", err)
	}
}

func TestValidateToolArgs_NilMCP_SkipsValidation(t *testing.T) {
	err := validateToolArgs(nil, "search", map[string]any{})
	if err != nil {
		t.Fatalf("expected nil error for nil mcp, got: %v", err)
	}
}

func TestValidateToolArgs_EmptySchema_SkipsValidation(t *testing.T) {
	mcp := &config.MCPDef{
		ID: "test-mcp",
		FullToolDefinitions: []schemas.ToolDefinition{
			{Name: "search", InputSchema: map[string]any{}},
		},
	}

	err := validateToolArgs(mcp, "search", map[string]any{})
	if err != nil {
		t.Fatalf("expected nil error for empty schema, got: %v", err)
	}
}

func TestValidateToolArgs_AdditionalProperties(t *testing.T) {
	// Schema with additionalProperties: false — extra fields should be rejected.
	mcp := &config.MCPDef{
		ID: "test-mcp",
		FullToolDefinitions: []schemas.ToolDefinition{
			{
				Name: "create",
				InputSchema: map[string]any{
					"type":                 "object",
					"properties":           map[string]any{"name": map[string]any{"type": "string"}},
					"required":             []any{"name"},
					"additionalProperties": false,
				},
			},
		},
	}

	// Valid: only "name" field.
	err := validateToolArgs(mcp, "create", map[string]any{"name": "foo"})
	if err != nil {
		t.Fatalf("expected nil for valid args, got: %v", err)
	}

	// Invalid: extra field "x" when additionalProperties is false.
	err = validateToolArgs(mcp, "create", map[string]any{"name": "foo", "x": 1})
	if err == nil {
		t.Fatal("expected error for additional property, got nil")
	}
}

func TestCallRemoteMCPTool_ValidationRejectsBeforeHTTP(t *testing.T) {
	// If validation fails, the HTTP call should never be made.
	httpCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{
		ID:  "test-mcp",
		URL: srv.URL,
		FullToolDefinitions: []schemas.ToolDefinition{
			{
				Name: "search",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []any{"query"},
				},
			},
		},
	}

	m := NewMCPConnectionManager(nil, nil)
	// Missing required "query" field — should fail before HTTP.
	_, err := m.callRemoteMCPTool(context.Background(), mcp, "search", map[string]any{})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if httpCalled {
		t.Fatal("HTTP call should not have been made when validation fails")
	}
}

func TestCallRemoteMCPTool_ValidationPassesThenHTTP(t *testing.T) {
	// If validation passes, the HTTP call should proceed normally.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{
		ID:  "test-mcp",
		URL: srv.URL,
		FullToolDefinitions: []schemas.ToolDefinition{
			{
				Name: "search",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []any{"query"},
				},
			},
		},
	}

	m := NewMCPConnectionManager(nil, nil)
	result, err := m.callRemoteMCPTool(context.Background(), mcp, "search", map[string]any{"query": "hello"})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}
