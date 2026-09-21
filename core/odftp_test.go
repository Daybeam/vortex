package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestSieve_VerifyFidelity(t *testing.T) {
	s := NewSieve(10)

	original := "Goal: Implement ODFTP\nDecisionID: 12345\n$artifact: data.json"

	// Case 1: Faithful compression
	compressedOK := "Goal: Implement ODFTP\nDecisionID: 12345\n$artifact: data.json\n... pruned content ..."
	faithful, lost := s.VerifyFidelity(original, compressedOK)
	if !faithful {
		t.Errorf("Expected faithful, but lost: %s", lost)
	}

	// Case 2: Lost signal
	compressedBad := "Goal: Implement ODFTP\n... pruned content ..."
	faithful, lost = s.VerifyFidelity(original, compressedBad)
	if faithful {
		t.Errorf("Expected unfaithful due to lost signal, but got faithful")
	}
	if lost != "DecisionID" {
		t.Errorf("Expected lost signal 'DecisionID', got %q", lost)
	}
}

func TestDirectedEngine_DiagnoseFault(t *testing.T) {
	s := &DirectedEngine{}

	// Case 1: Context Deficit
	output := &schemas.SubagentOutput{
		MissingContext: []string{"user_id"},
	}
	cause, _, details := s.diagnoseFault(output, &schemas.Step{})
	if cause != FailureClassContextDeficit {
		t.Errorf("Expected context_deficit, got %s", cause)
	}
	if len(details) != 1 || details[0] != "user_id" {
		t.Errorf("Expected detail 'user_id', got %v", details)
	}

	// Case 2: Capability Required
	output = &schemas.SubagentOutput{
		Status:               schemas.StatusCapabilityRequired,
		RequiredCapabilities: []string{"shell.run"},
	}
	cause, _, details = s.diagnoseFault(output, &schemas.Step{})
	if cause != FailureClassCapabilityRequired {
		t.Errorf("Expected capability_required, got %s", cause)
	}
	if len(details) != 1 || details[0] != "shell.run" {
		t.Errorf("Expected detail 'shell.run', got %v", details)
	}

	// Case 3: Generative Uncertainty (Low Confidence)
	conf := 0.4
	output = &schemas.SubagentOutput{
		Confidence: 0.4,
	}
	step := &schemas.Step{
		Confidence: &conf,
	}
	cause, _, _ = s.diagnoseFault(output, step)
	if cause != FailureClassGenerativeUncertainty {
		t.Errorf("Expected generative_uncertainty, got %s", cause)
	}
}
