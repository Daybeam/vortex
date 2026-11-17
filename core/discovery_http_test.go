package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPListSource_Discover_ArrayFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items := []DiscoveredItem{
			{ID: "web-scraper", Name: "Web Scraper", Kind: DiscoveryKindMCP, URL: "https://example.com/mcp"},
			{ID: "db-query", Name: "DB Query", Kind: DiscoveryKindMCP, URL: "https://example.com/db"},
		}
		json.NewEncoder(w).Encode(items)
	}))
	defer server.Close()

	src := NewHTTPListSource("test-mcp", DiscoveryKindMCP, server.URL, nil)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != "web-scraper" {
		t.Errorf("expected first item ID 'web-scraper', got %q", items[0].ID)
	}
	if items[0].SourceName != "test-mcp" {
		t.Errorf("expected SourceName 'test-mcp', got %q", items[0].SourceName)
	}
	if items[0].TrustLevel != TrustLevelUntrusted {
		t.Errorf("expected TrustLevel %q, got %q", TrustLevelUntrusted, items[0].TrustLevel)
	}
	if items[0].Kind != DiscoveryKindMCP {
		t.Errorf("expected Kind %q, got %q", DiscoveryKindMCP, items[0].Kind)
	}
}

func TestHTTPListSource_Discover_WrapperFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapper := map[string]any{
			"items": []DiscoveredItem{
				{ID: "code-review", Name: "Code Review", Kind: DiscoveryKindSkill, Capability: "code-review"},
			},
		}
		json.NewEncoder(w).Encode(wrapper)
	}))
	defer server.Close()

	src := NewHTTPListSource("test-skill", DiscoveryKindSkill, server.URL, nil)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "code-review" {
		t.Errorf("expected item ID 'code-review', got %q", items[0].ID)
	}
	if items[0].Capability != "code-review" {
		t.Errorf("expected Capability 'code-review', got %q", items[0].Capability)
	}
}

func TestHTTPListSource_Discover_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	src := NewHTTPListSource("test-err", DiscoveryKindMCP, server.URL, nil)
	_, err := src.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

func TestHTTPListSource_Discover_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not valid json`))
	}))
	defer server.Close()

	src := NewHTTPListSource("test-bad", DiscoveryKindMCP, server.URL, nil)
	_, err := src.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestHTTPListSource_Discover_EmptyArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	src := NewHTTPListSource("test-empty", DiscoveryKindMCP, server.URL, nil)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestHTTPListSource_Discover_CustomHeaders(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	headers := map[string]string{"Authorization": "Bearer test-token"}
	src := NewHTTPListSource("test-auth", DiscoveryKindMCP, server.URL, headers)
	_, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if receivedAuth != "Bearer test-token" {
		t.Errorf("expected Authorization header 'Bearer test-token', got %q", receivedAuth)
	}
}

func TestHTTPListSource_Discover_TrustLevelFromResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items := []DiscoveredItem{
			{ID: "trusted-skill", Name: "Trusted", TrustLevel: TrustLevelTrusted},
			{ID: "default-skill", Name: "Default"},
		}
		json.NewEncoder(w).Encode(items)
	}))
	defer server.Close()

	src := NewHTTPListSource("test-trust", DiscoveryKindSkill, server.URL, nil)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if items[0].TrustLevel != TrustLevelTrusted {
		t.Errorf("expected first item TrustLevel %q, got %q", TrustLevelTrusted, items[0].TrustLevel)
	}
	if items[1].TrustLevel != TrustLevelUntrusted {
		t.Errorf("expected second item TrustLevel %q, got %q", TrustLevelUntrusted, items[1].TrustLevel)
	}
}

func TestHTTPListSource_Name(t *testing.T) {
	src := NewHTTPListSource("my-source", DiscoveryKindSkill, "https://example.com", nil)
	if src.Name() != "my-source" {
		t.Errorf("expected Name() 'my-source', got %q", src.Name())
	}
}

func TestHTTPListSource_Kind(t *testing.T) {
	src := NewHTTPListSource("my-source", DiscoveryKindMCP, "https://example.com", nil)
	if src.Kind() != DiscoveryKindMCP {
		t.Errorf("expected Kind() %q, got %q", DiscoveryKindMCP, src.Kind())
	}
}

func TestParseDiscoveryResponse_Array(t *testing.T) {
	body := []byte(`[{"id":"a","name":"A"},{"id":"b","name":"B"}]`)
	items, err := parseDiscoveryResponse(body)
	if err != nil {
		t.Fatalf("parseDiscoveryResponse failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != "a" || items[1].ID != "b" {
		t.Errorf("unexpected item IDs: %q, %q", items[0].ID, items[1].ID)
	}
}

func TestParseDiscoveryResponse_Wrapper(t *testing.T) {
	body := []byte(`{"items":[{"id":"x","name":"X"}]}`)
	items, err := parseDiscoveryResponse(body)
	if err != nil {
		t.Fatalf("parseDiscoveryResponse failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "x" {
		t.Errorf("expected item ID 'x', got %q", items[0].ID)
	}
}

func TestParseDiscoveryResponse_Invalid(t *testing.T) {
	body := []byte(`{"not_items": [1,2,3]}`)
	_, err := parseDiscoveryResponse(body)
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
}
