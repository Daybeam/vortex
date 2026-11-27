package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestPublicTaskDecisionHandler_InputValidation (Stage 4, 2026-10-24): the
// public owner-self-approve endpoint accepts ONLY POST + a /decision path +
// approve/reject choices. These guards run before the scheduler call, so no
// scheduler is needed. The actual approve authority is covered by
// TestCheckApprovalAuthority_* in core.
func TestPublicTaskDecisionHandler_InputValidation(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)
	h := &APIHandler{App: app}

	tests := []struct {
		method, path, body string
		wantStatus         int
	}{
		{http.MethodGet, "/api/tasks/t1/decision", `{}`, http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/tasks/t1/result", `{}`, http.StatusNotFound}, // not /decision
		{http.MethodPost, "/api/tasks/t1/decision", `{"decision_id":"d","choice":"skip"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/tasks/t1/decision", `{"decision_id":"d","choice":"retry"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
		rr := httptest.NewRecorder()
		h.PublicTaskDecisionHandler(rr, req)
		if rr.Code != tt.wantStatus {
			t.Fatalf("%s %s body=%q: got %d, want %d (%s)", tt.method, tt.path, tt.body, rr.Code, tt.wantStatus, rr.Body.String())
		}
	}
}
