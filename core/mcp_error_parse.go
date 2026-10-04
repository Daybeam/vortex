package core

import (
	"encoding/json"
	"strings"
)

// SERFError represents a structured error parsed from an MCP tool response,
// following the SERF (Structured Error Recovery Framework) schema from
// arXiv:2603.13417 §12.3.
type SERFError struct {
	Category         string         `json:"category"`          // INVALID_INPUT / RESOURCE_NOT_FOUND / ...
	Retryable        bool           `json:"retryable"`
	RetryAfterMs     *int           `json:"retry_after_ms"`    // nil = no suggested value
	SuggestedActions []SERFAction   `json:"suggested_actions"`
	Context          map[string]any `json:"context"`
	RawMessage       string         `json:"message"`           // original error text
}

// SERFAction represents a recovery action suggested by the tool.
type SERFAction struct {
	Type    string         `json:"type"`             // SWITCH_RESOURCE / ESCALATE_TO_USER / RETRY
	Params  map[string]any `json:"params"`
	Message string         `json:"message,omitempty"`
}

// parseMCPError inspects an MCP tool call result for the isError flag and
// extracts SERF metadata if present. finalRes is the value returned by
// cli.SendRequest("tools/call", ...).
//
// Returns:
//   - isError: true if the MCP response has isError:true
//   - serf:    parsed SERF metadata, or nil if none found
//   - errMsg:  the human-readable error text from the response content
func parseMCPError(finalRes any) (isError bool, serf *SERFError, errMsg string) {
	m, ok := finalRes.(map[string]any)
	if !ok {
		return false, nil, ""
	}

	isErrorFlag, _ := m["isError"].(bool)
	if !isErrorFlag {
		return false, nil, ""
	}

	errMsg = extractTextFromContent(m["content"])
	serf = tryParseSERF(errMsg)
	return true, serf, errMsg
}

// extractTextFromContent extracts text from an MCP content array.
// Content format: [{"type":"text","text":"..."}]
func extractTextFromContent(content any) string {
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				texts = append(texts, t)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// tryParseSERF attempts to parse SERF structured metadata from an error text.
// Supports three formats:
//  1. Pure SERF JSON: {"serf":{"category":"...","retryable":...}}
//  2. Flat JSON with code/message: {"code":"...","message":"...","retryable":...}
//  3. Free text with HTTP status codes (e.g. "HTTP 401: Unauthorized")
func tryParseSERF(errMsg string) *SERFError {
	if errMsg == "" {
		return nil
	}

	trimmed := strings.TrimSpace(errMsg)
	if strings.HasPrefix(trimmed, "{") {
		var wrapper struct {
			SERF      *SERFError      `json:"serf"`
			Code      string          `json:"code"`
			Message   string          `json:"message"`
			Retryable bool            `json:"retryable"`
			Context   map[string]any  `json:"context"`
		}
		if json.Unmarshal([]byte(trimmed), &wrapper) == nil {
			if wrapper.SERF != nil {
				wrapper.SERF.RawMessage = errMsg
				return wrapper.SERF
			}
			if wrapper.Code != "" {
				return &SERFError{
					Category:   wrapper.Code,
					Retryable:  wrapper.Retryable,
					Context:    wrapper.Context,
					RawMessage: errMsg,
				}
			}
		}
	}

	return parseHTTPStatusSERF(errMsg)
}

// parseHTTPStatusSERF maps free-text HTTP status codes to SERF categories.
func parseHTTPStatusSERF(msg string) *SERFError {
	upper := strings.ToUpper(msg)
	switch {
	case strings.Contains(upper, "HTTP 400") || strings.Contains(upper, "BAD REQUEST"):
		return &SERFError{Category: "INVALID_INPUT", Retryable: false, RawMessage: msg}
	case strings.Contains(upper, "HTTP 401") || strings.Contains(upper, "UNAUTHORIZED") || strings.Contains(upper, "PERMISSION_DENIED"):
		return &SERFError{Category: "PERMISSION_DENIED", Retryable: false, RawMessage: msg}
	case strings.Contains(upper, "HTTP 403") || strings.Contains(upper, "FORBIDDEN"):
		return &SERFError{Category: "PERMISSION_DENIED", Retryable: false, RawMessage: msg}
	case strings.Contains(upper, "HTTP 404") || strings.Contains(upper, "NOT FOUND") || strings.Contains(upper, "RESOURCE_NOT_FOUND"):
		return &SERFError{Category: "RESOURCE_NOT_FOUND", Retryable: false, RawMessage: msg}
	case strings.Contains(upper, "HTTP 429") || strings.Contains(upper, "TOO MANY REQUESTS") || strings.Contains(upper, "RESOURCE_EXHAUSTED") || strings.Contains(upper, "RATE_LIMITED"):
		return &SERFError{Category: "RESOURCE_EXHAUSTED", Retryable: false, RawMessage: msg}
	case strings.Contains(upper, "HTTP 500") || strings.Contains(upper, "INTERNAL SERVER ERROR") || strings.Contains(upper, "INTERNAL_ERROR"):
		return &SERFError{Category: "INTERNAL_ERROR", Retryable: false, RawMessage: msg}
	case strings.Contains(upper, "HTTP 502") || strings.Contains(upper, "HTTP 503") || strings.Contains(upper, "HTTP 504") || strings.Contains(upper, "UPSTREAM_FAILURE") || strings.Contains(upper, "TIMEOUT") || strings.Contains(upper, "DEADLINE EXCEEDED"):
		return &SERFError{Category: "UPSTREAM_FAILURE", Retryable: true, RawMessage: msg}
	default:
		return nil
	}
}

// SERFCategoryToFailureClass maps a SERF category string to the existing
// FailureClass enum. Used to bridge SERF metadata into the scheduler's
// retry/decision logic.
func SERFCategoryToFailureClass(cat string) FailureClass {
	switch cat {
	case "INVALID_INPUT":
		return FailureClassBadRequest
	case "RESOURCE_NOT_FOUND":
		return FailureClassResourceNotFound
	case "RESOURCE_EXHAUSTED":
		return FailureClassRateLimit
	case "PERMISSION_DENIED":
		return FailureClassAuthPermission
	case "UPSTREAM_FAILURE":
		return FailureClassTransient
	case "INTERNAL_ERROR":
		return FailureClassTransient
	default:
		return FailureClassTransient
	}
}
