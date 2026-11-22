package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// Regression tests for the systemone decision-gate confidence instrumentation
// (2026-11-18). askSystemOneForDecision emits an EventDecisionAutoResolved
// trace event with the NUMERIC confidence and decision_id on every outcome —
// resolved, low_confidence, error, invalid_choice, no_answer — so
// global_trajectory.jsonl can build a reliability diagram (confidence vs
// downstream gold label) to audit MinConfidence against real traffic
// instead of trusting vendor-reported ECE.

// newSystemOneGateEngine builds an engine with the systemone decision-gate
// enabled against a mock /v1/systemone backend served by httptest.
func newSystemOneGateEngine(t *testing.T, backend http.HandlerFunc) (*DirectedEngine, *obs.Logger) {
	t.Helper()
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)

	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	logDir := t.TempDir()
	outDir := t.TempDir()
	logger, err := obs.NewLogger(logDir, &config.SystemSettings{})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)

	sc := config.SystemOneConfig{
		BaseURL:            srv.URL,
		Model:              "jev-test",
		EnableDecisionGate: true,
		MinConfidence:      0.5,
	}
	sysOneP := providers.NewSystemOneProvider(&config.ProviderConfig{
		Provider: "systemone",
		Model:    sc.Model,
		BaseURL:  sc.BaseURL,
	})
	s.SetSystemOneProvider(sysOneP, sc)
	return s, logger
}

// systemOneBackendChoice returns a handler that answers q1 with the given
// choice and confidence in /v1/systemone wire format.
func systemOneBackendChoice(choice string, confidence float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"answers": map[string]any{
				"q1": map[string]any{
					"type":          "choice",
					"choice":        choice,
					"confidence":    confidence,
					"probabilities": map[string]float64{choice: confidence, "other": 1 - confidence},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// readAllGateEvents reads today's decision_auto_resolved events from the
// logger's on-disk jsonl (the same sink global_trajectory.jsonl consumes).
// The logger writes asynchronously (channel + background goroutine); Close()
// closes the channel, drains it, and waits (wg.Wait) — i.e. Close is the
// flush barrier. It is idempotent (closed flag), so calling it here before
// the t.Cleanup Close is safe.
func readAllGateEvents(t *testing.T, logger *obs.Logger) []map[string]any {
	t.Helper()
	logger.Close()
	events, err := logger.ReadAllLogs(time.Now().UTC().Format("2006-01-02"), string(obs.EventDecisionAutoResolved), "", 200, 0)
	if err != nil {
		t.Fatalf("ReadAllLogs: %v", err)
	}
	// Flatten: merge nested detail fields into the top-level map so tests
	// can assert ev["outcome"], ev["decision_id"], ... directly. The wire
	// format keeps them under "detail" (logger.go LogEvent json tags).
	flat := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		if d, ok := ev["detail"].(map[string]any); ok {
			for k, v := range d {
				if _, exists := ev[k]; !exists {
					ev[k] = v
				}
			}
		}
		flat = append(flat, ev)
	}
	return flat
}

func TestSystemOneGateConfidence_LowConfidenceLogged(t *testing.T) {
	s, logger := newSystemOneGateEngine(t, systemOneBackendChoice("skip", 0.30))
	defer s.Stop()

	_, conf, ok := s.askSystemOneForDecision("task-1", "dec-1", "s1",
		schemas.DecisionStepFailed, map[string]any{"root_cause": "test"},
		[]string{"skip", "abort"})
	if ok {
		t.Fatal("low confidence (0.30 < 0.5) must not resolve")
	}
	if conf != 0.30 {
		t.Fatalf("returned confidence = %v, want 0.30", conf)
	}

	// The trace event must carry the numeric confidence + decision_id + outcome.
	found := false
	for _, ev := range readAllGateEvents(t, logger) {
		if ev["outcome"] == "systemone_low_confidence" {
			found = true
			if ev["decision_id"] != "dec-1" {
				t.Errorf("decision_id = %v, want dec-1", ev["decision_id"])
			}
			if c, _ := ev["confidence"].(float64); c != 0.30 {
				t.Errorf("confidence = %v, want 0.30", ev["confidence"])
			}
			if ev["picked"] != "skip" {
				t.Errorf("picked = %v, want skip", ev["picked"])
			}
			if mc, _ := ev["min_confidence"].(float64); mc != 0.5 {
				t.Errorf("min_confidence = %v, want 0.5", ev["min_confidence"])
			}
		}
	}
	if !found {
		t.Fatal("no systemone_low_confidence event found in trace logs")
	}
}

func TestSystemOneGateConfidence_ResolvedLogged(t *testing.T) {
	s, logger := newSystemOneGateEngine(t, systemOneBackendChoice("skip", 0.92))
	defer s.Stop()

	choice, conf, ok := s.askSystemOneForDecision("task-2", "dec-2", "s1",
		schemas.DecisionStepFailed, map[string]any{"root_cause": "test"},
		[]string{"skip", "abort"})
	if !ok || choice != "skip" {
		t.Fatalf("expected resolved skip, got (%q, %v)", choice, ok)
	}
	if conf != 0.92 {
		t.Fatalf("returned confidence = %v, want 0.92", conf)
	}

	found := false
	for _, ev := range readAllGateEvents(t, logger) {
		if ev["outcome"] == "systemone_resolved" {
			found = true
			if ev["choice"] != "skip" {
				t.Errorf("choice = %v, want skip", ev["choice"])
			}
			if c, _ := ev["confidence"].(float64); c != 0.92 {
				t.Errorf("confidence = %v, want 0.92", ev["confidence"])
			}
		}
	}
	if !found {
		t.Fatal("no systemone_resolved event found in trace logs")
	}
}

func TestSystemOneGateConfidence_InvalidChoiceLogged(t *testing.T) {
	// Backend answers with an option outside the offered set.
	s, logger := newSystemOneGateEngine(t, systemOneBackendChoice("nonsense_option", 0.99))
	defer s.Stop()

	_, _, ok := s.askSystemOneForDecision("task-3", "dec-3", "s1",
		schemas.DecisionStepFailed, map[string]any{"root_cause": "test"},
		[]string{"skip", "abort"})
	if ok {
		t.Fatal("invalid choice must not resolve")
	}

	found := false
	for _, ev := range readAllGateEvents(t, logger) {
		if ev["outcome"] == "systemone_invalid_choice" {
			found = true
			if ev["picked"] != "nonsense_option" {
				t.Errorf("picked = %v, want nonsense_option", ev["picked"])
			}
		}
	}
	if !found {
		t.Fatal("no systemone_invalid_choice event found in trace logs")
	}
}

func TestSystemOneGateConfidence_ErrorLogged(t *testing.T) {
	// Backend returns HTTP 500 → Complete errors → systemone_error event.
	s, logger := newSystemOneGateEngine(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer s.Stop()

	_, _, ok := s.askSystemOneForDecision("task-4", "dec-4", "s1",
		schemas.DecisionStepFailed, map[string]any{"root_cause": "test"},
		[]string{"skip", "abort"})
	if ok {
		t.Fatal("backend error must not resolve")
	}

	found := false
	for _, ev := range readAllGateEvents(t, logger) {
		if ev["outcome"] == "systemone_error" {
			found = true
			if ev["error"] == nil || ev["error"] == "" {
				t.Error("error event must carry the error string")
			}
		}
	}
	if !found {
		t.Fatal("no systemone_error event found in trace logs")
	}
}
