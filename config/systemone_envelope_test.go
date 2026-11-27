package config

import (
	"encoding/json"
	"testing"
)

func TestSystemOneConfig_EnvelopeParsing(t *testing.T) {
	var c SystemOneConfig
	if err := json.Unmarshal([]byte(`{"base_url":"https://generativelanguage.googleapis.com","model":"gemini-decision-1","envelope":"gemini","enable_decision_gate":true}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Envelope != "gemini" {
		t.Errorf("Envelope = %q, want %q", c.Envelope, "gemini")
	}
	if c.BaseURL != "https://generativelanguage.googleapis.com" || c.Model != "gemini-decision-1" {
		t.Errorf("BaseURL/Model = %q/%q", c.BaseURL, c.Model)
	}
	if !c.EnableDecisionGate {
		t.Error("EnableDecisionGate should be true")
	}
	// Round-trip: empty envelope must not leak into JSON.
	b, _ := json.Marshal(c)
	var round map[string]any
	json.Unmarshal(b, &round)
	if got, has := round["envelope"]; !has || got != "gemini" {
		t.Errorf("envelope round-trip = %v (present=%v), want gemini", got, has)
	}
}
