/*
 * ToolRouter SearchTools — keyword-based tool search with pagination.
 */

package core

import (
	"context"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"sync"
	"testing"
)

func TestSearchTools(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "get_earnings", Description: "Get earnings data"},
					{Name: "earnings_report", Description: "Get earnings report"},
				},
			},
		},
	}

	router := NewToolRouter(registry, nil)

	// Test 1: Successful search
	res, err := router.SearchTools(context.Background(), "earnings", "", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(res.Tools))
	}

	// Test 2: Pagination
	res, err = router.SearchTools(context.Background(), "earnings", "", 1, 1)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(res.Tools))
	}

	// Test 3: Zero results
	res, err = router.SearchTools(context.Background(), "unknown", "", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(res.Tools))
	}
}
