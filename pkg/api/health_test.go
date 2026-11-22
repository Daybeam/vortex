package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/pkg/bootmetrics"
)

func TestHealthHandlerIncludesBootPhases(t *testing.T) {
	bootmetrics.Mark("test-phase")

	rec := httptest.NewRecorder()
	HealthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || len(body.Boot) == 0 {
		t.Fatalf("health = %+v, want ok plus boot phases", body)
	}
	found := false
	for _, phase := range body.Boot {
		if phase.Name == "test-phase" {
			found = true
		}
	}
	if !found {
		t.Errorf("boot phases = %+v, want test-phase", body.Boot)
	}
}
