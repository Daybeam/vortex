package core

import (
	"testing"
)

// TestParseMCPError_IsErrorTrue verifies that parseMCPError correctly detects
// the MCP isError flag and extracts the error text.
// Audit ID: SERF-001
func TestParseMCPError_IsErrorTrue(t *testing.T) {
	finalRes := map[string]any{
		"isError": true,
		"content": []any{
			map[string]any{"type": "text", "text": "HTTP 401: Unauthorized"},
		},
	}
	isError, serf, errMsg := parseMCPError(finalRes)
	if !isError {
		t.Fatal("expected isError=true")
	}
	if errMsg != "HTTP 401: Unauthorized" {
		t.Errorf("expected error message 'HTTP 401: Unauthorized', got %q", errMsg)
	}
	if serf == nil {
		t.Fatal("expected non-nil SERF from HTTP 401 text")
	}
	if serf.Category != "PERMISSION_DENIED" {
		t.Errorf("expected PERMISSION_DENIED, got %s", serf.Category)
	}
}

// TestParseMCPError_IsErrorFalse verifies that normal (non-error) MCP responses
// are not flagged as errors.
// Audit ID: SERF-002
func TestParseMCPError_IsErrorFalse(t *testing.T) {
	finalRes := map[string]any{
		"isError": false,
		"content": []any{
			map[string]any{"type": "text", "text": "success"},
		},
	}
	isError, _, _ := parseMCPError(finalRes)
	if isError {
		t.Fatal("expected isError=false for normal response")
	}
}

// TestParseMCPError_NoIsErrorField verifies that responses without isError field
// are treated as non-errors.
// Audit ID: SERF-003
func TestParseMCPError_NoIsErrorField(t *testing.T) {
	finalRes := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "result"},
		},
	}
	isError, _, _ := parseMCPError(finalRes)
	if isError {
		t.Fatal("expected isError=false when field is absent")
	}
}

// TestParseMCPError_NotMap verifies graceful handling of non-map results.
// Audit ID: SERF-004
func TestParseMCPError_NotMap(t *testing.T) {
	isError, _, _ := parseMCPError("some string")
	if isError {
		t.Fatal("expected isError=false for non-map input")
	}

	isError, _, _ = parseMCPError(nil)
	if isError {
		t.Fatal("expected isError=false for nil input")
	}
}

// TestTryParseSERF_JSONFormat1 verifies parsing of {"serf":{"category":"..."}}
// format.
// Audit ID: SERF-005
func TestTryParseSERF_JSONFormat1(t *testing.T) {
	input := `{"serf":{"category":"RESOURCE_NOT_FOUND","retryable":false,"suggested_actions":[{"type":"SWITCH_RESOURCE","params":{"field":"project_id"}}]}}`
	serf := tryParseSERF(input)
	if serf == nil {
		t.Fatal("expected non-nil SERF")
	}
	if serf.Category != "RESOURCE_NOT_FOUND" {
		t.Errorf("expected RESOURCE_NOT_FOUND, got %s", serf.Category)
	}
	if serf.Retryable != false {
		t.Error("expected retryable=false")
	}
	if len(serf.SuggestedActions) != 1 {
		t.Fatalf("expected 1 suggested action, got %d", len(serf.SuggestedActions))
	}
	if serf.SuggestedActions[0].Type != "SWITCH_RESOURCE" {
		t.Errorf("expected SWITCH_RESOURCE, got %s", serf.SuggestedActions[0].Type)
	}
}

// TestTryParseSERF_JSONFormat2 verifies parsing of flat JSON
// {"code":"...","retryable":...} format.
// Audit ID: SERF-006
func TestTryParseSERF_JSONFormat2(t *testing.T) {
	input := `{"code":"UPSTREAM_FAILURE","message":"timeout","retryable":true}`
	serf := tryParseSERF(input)
	if serf == nil {
		t.Fatal("expected non-nil SERF")
	}
	if serf.Category != "UPSTREAM_FAILURE" {
		t.Errorf("expected UPSTREAM_FAILURE, got %s", serf.Category)
	}
	if serf.Retryable != true {
		t.Error("expected retryable=true")
	}
}

// TestTryParseSERF_HTTP401 verifies HTTP 401 maps to PERMISSION_DENIED.
// Audit ID: SERF-007
func TestTryParseSERF_HTTP401(t *testing.T) {
	serf := tryParseSERF("HTTP 401: Unauthorized")
	if serf == nil {
		t.Fatal("expected non-nil SERF")
	}
	if serf.Category != "PERMISSION_DENIED" {
		t.Errorf("expected PERMISSION_DENIED, got %s", serf.Category)
	}
}

// TestTryParseSERF_HTTP404 verifies HTTP 404 maps to RESOURCE_NOT_FOUND.
// Audit ID: SERF-008
func TestTryParseSERF_HTTP404(t *testing.T) {
	serf := tryParseSERF("HTTP 404: Not Found")
	if serf == nil {
		t.Fatal("expected non-nil SERF")
	}
	if serf.Category != "RESOURCE_NOT_FOUND" {
		t.Errorf("expected RESOURCE_NOT_FOUND, got %s", serf.Category)
	}
}

// TestTryParseSERF_HTTP429 verifies HTTP 429 maps to RESOURCE_EXHAUSTED.
// Audit ID: SERF-009
func TestTryParseSERF_HTTP429(t *testing.T) {
	serf := tryParseSERF("HTTP 429: Too Many Requests")
	if serf == nil {
		t.Fatal("expected non-nil SERF")
	}
	if serf.Category != "RESOURCE_EXHAUSTED" {
		t.Errorf("expected RESOURCE_EXHAUSTED, got %s", serf.Category)
	}
}

// TestTryParseSERF_HTTP503 verifies HTTP 503 maps to UPSTREAM_FAILURE with
// retryable=true.
// Audit ID: SERF-010
func TestTryParseSERF_HTTP503(t *testing.T) {
	serf := tryParseSERF("HTTP 503: Service Unavailable")
	if serf == nil {
		t.Fatal("expected non-nil SERF")
	}
	if serf.Category != "UPSTREAM_FAILURE" {
		t.Errorf("expected UPSTREAM_FAILURE, got %s", serf.Category)
	}
	if !serf.Retryable {
		t.Error("expected retryable=true for 503")
	}
}

// TestTryParseSERF_Timeout verifies "timeout" maps to UPSTREAM_FAILURE.
// Audit ID: SERF-011
func TestTryParseSERF_Timeout(t *testing.T) {
	serf := tryParseSERF("context deadline exceeded")
	if serf == nil {
		t.Fatal("expected non-nil SERF")
	}
	if serf.Category != "UPSTREAM_FAILURE" {
		t.Errorf("expected UPSTREAM_FAILURE, got %s", serf.Category)
	}
}

// TestTryParseSERF_UnparseableText verifies that unrecognized free text returns
// nil (no SERF metadata).
// Audit ID: SERF-012
func TestTryParseSERF_UnparseableText(t *testing.T) {
	serf := tryParseSERF("something went wrong")
	if serf != nil {
		t.Fatal("expected nil for unparseable text")
	}
}

// TestTryParseSERF_EmptyString verifies empty input returns nil.
// Audit ID: SERF-013
func TestTryParseSERF_EmptyString(t *testing.T) {
	serf := tryParseSERF("")
	if serf != nil {
		t.Fatal("expected nil for empty string")
	}
}

// TestSERFCategoryToFailureClass verifies all 6 SERF category mappings.
// Audit ID: SERF-014
func TestSERFCategoryToFailureClass(t *testing.T) {
	tests := []struct {
		category string
		expected FailureClass
	}{
		{"INVALID_INPUT", FailureClassBadRequest},
		{"RESOURCE_NOT_FOUND", FailureClassResourceNotFound},
		{"RESOURCE_EXHAUSTED", FailureClassRateLimit},
		{"PERMISSION_DENIED", FailureClassAuthPermission},
		{"UPSTREAM_FAILURE", FailureClassTransient},
		{"INTERNAL_ERROR", FailureClassTransient},
		{"UNKNOWN", FailureClassTransient}, // default
	}
	for _, tc := range tests {
		got := SERFCategoryToFailureClass(tc.category)
		if got != tc.expected {
			t.Errorf("SERFCategoryToFailureClass(%q) = %q, want %q", tc.category, got, tc.expected)
		}
	}
}

// TestBuildSERFRecovery_NoSERF verifies fallback to toolFailFeedback behavior
// when SERF metadata is nil.
// Audit ID: SERF-015
func TestBuildSERFRecovery_NoSERF(t *testing.T) {
	msg := buildSERFRecovery("write_file", nil, "permission denied", 1)
	if msg == "" {
		t.Fatal("expected non-empty recovery message")
	}
	if !serfContains(msg, "[TOOL FAILED: write_file]") {
		t.Errorf("expected [TOOL FAILED: write_file] in message, got: %s", msg)
	}
}

// TestBuildSERFRecovery_WithSERF verifies structured recovery advice.
// Audit ID: SERF-016
func TestBuildSERFRecovery_WithSERF(t *testing.T) {
	serf := &SERFError{
		Category:  "PERMISSION_DENIED",
		Retryable: false,
	}
	msg := buildSERFRecovery("read_file", serf, "HTTP 401", 1)
	if !serfContains(msg, "Category: PERMISSION_DENIED") {
		t.Errorf("expected 'Category: PERMISSION_DENIED' in message, got: %s", msg)
	}
	if !serfContains(msg, "Do not retry") {
		t.Errorf("expected 'Do not retry' advice for PERMISSION_DENIED, got: %s", msg)
	}
}

// TestBuildSERFRecovery_CircuitBreaker verifies circuit breaker at threshold.
// Audit ID: SERF-017
func TestBuildSERFRecovery_CircuitBreaker(t *testing.T) {
	serf := &SERFError{Category: "UPSTREAM_FAILURE", Retryable: true}
	msg := buildSERFRecovery("search", serf, "timeout", toolFailCircuitBreakerThreshold)
	if !serfContains(msg, "HAS FAILED") {
		t.Errorf("expected circuit breaker message, got: %s", msg)
	}
	if !serfContains(msg, "Stop using this tool") {
		t.Errorf("expected 'Stop using this tool' in circuit breaker, got: %s", msg)
	}
}

// TestBuildSERFRecovery_SuggestedActions verifies suggested actions are appended.
// Audit ID: SERF-018
func TestBuildSERFRecovery_SuggestedActions(t *testing.T) {
	serf := &SERFError{
		Category: "RESOURCE_NOT_FOUND",
		SuggestedActions: []SERFAction{
			{Type: "SWITCH_RESOURCE", Params: map[string]any{"field": "project_id"}},
			{Type: "ESCALATE_TO_USER", Message: "Ask user for correct ID"},
		},
	}
	msg := buildSERFRecovery("get_resource", serf, "not found", 1)
	if !serfContains(msg, "Try switching project_id") {
		t.Errorf("expected SWITCH_RESOURCE advice, got: %s", msg)
	}
	if !serfContains(msg, "Ask user for correct ID") {
		t.Errorf("expected ESCALATE_TO_USER advice, got: %s", msg)
	}
}

// TestBuildSERFRecovery_RetryAfterMs verifies retry_after_ms is included.
// Audit ID: SERF-019
func TestBuildSERFRecovery_RetryAfterMs(t *testing.T) {
	retryAfter := 5000
	serf := &SERFError{
		Category:     "RESOURCE_EXHAUSTED",
		RetryAfterMs: &retryAfter,
	}
	msg := buildSERFRecovery("api_call", serf, "rate limited", 1)
	if !serfContains(msg, "5000ms") {
		t.Errorf("expected '5000ms' in message, got: %s", msg)
	}
}

// serfContains is a simple string contains helper to avoid import conflicts.
func serfContains(s, substr string) bool {
	return len(s) >= len(substr) && serfIndexOf(s, substr) >= 0
}

func serfIndexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
