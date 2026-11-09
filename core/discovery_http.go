package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/daybeam/vortex/pkg/safelimits"
)

// HTTPListSource discovers skills or MCPs from an HTTP endpoint that returns
// a JSON list. The response format is either:
//   - A JSON array of DiscoveredItem-compatible objects: [...]
//   - A JSON object with an "items" field: {"items": [...]}
//
// Each item in the list should have at least "id" and "name" fields. Other
// fields are optional and depend on the Kind (skill vs mcp).
//
// See docs/plan/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.2.2.
type HTTPListSource struct {
	name    string
	kind    DiscoveryKind
	url     string
	headers map[string]string
	timeout time.Duration
}

// NewHTTPListSource creates an HTTPListSource. The kind should be
// DiscoveryKindSkill or DiscoveryKindMCP. Headers are optional auth headers
// sent with the request.
func NewHTTPListSource(name string, kind DiscoveryKind, url string, headers map[string]string) *HTTPListSource {
	return &HTTPListSource{
		name:    name,
		kind:    kind,
		url:     url,
		headers: headers,
		timeout: 30 * time.Second,
	}
}

func (s *HTTPListSource) Name() string         { return s.name }
func (s *HTTPListSource) Kind() DiscoveryKind  { return s.kind }

// Discover fetches the HTTP endpoint and parses the JSON response into
// DiscoveredItem entries. All items inherit the source's Name and Kind,
// and default to TrustLevelUntrusted unless the response specifies otherwise.
func (s *HTTPListSource) Discover(ctx context.Context) ([]DiscoveredItem, error) {
	if err := validateFetchURL(s.url); err != nil {
		return nil, fmt.Errorf("discovery URL rejected (SSRF protection): %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request for %s: %w", s.name, err)
	}

	for k, v := range s.headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: s.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching discovery source %s: %w", s.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery source %s returned HTTP %d", s.name, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", s.name, err)
	}

	items, err := parseDiscoveryResponse(body)
	if err != nil {
		return nil, fmt.Errorf("parsing response from %s: %w", s.name, err)
	}

	// Stamp source metadata onto every item.
	for i := range items {
		items[i].SourceName = s.name
		items[i].Kind = s.kind // override to ensure consistency with configured Kind
		if items[i].TrustLevel == "" {
			items[i].TrustLevel = TrustLevelUntrusted
		}
	}

	return items, nil
}

// parseDiscoveryResponse parses a JSON response body that can be either:
//   - A bare JSON array of DiscoveredItem objects: [...]
//   - A wrapper object with an "items" field: {"items": [...]}
func parseDiscoveryResponse(body []byte) ([]DiscoveredItem, error) {
	// Try bare array format first.
	var items []DiscoveredItem
	if err := json.Unmarshal(body, &items); err == nil {
		return items, nil
	}

	// Try {"items": [...]} wrapper format. First verify the "items" key
	// exists in the raw JSON, otherwise any valid JSON object would match
	// the wrapper struct with an empty Items slice.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("response is neither a JSON array nor an {\"items\": [...]} object")
	}
	itemsRaw, ok := raw["items"]
	if !ok {
		return nil, fmt.Errorf("response is neither a JSON array nor an {\"items\": [...]} object")
	}

	var wrapper struct {
		Items []DiscoveredItem `json:"items"`
	}
	if err := json.Unmarshal(itemsRaw, &wrapper.Items); err != nil {
		return nil, fmt.Errorf("the \"items\" field is not a JSON array: %w", err)
	}

	return wrapper.Items, nil
}
