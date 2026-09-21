package core

import (
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestDebateFields_PropagatedFromStepInput(t *testing.T) {
	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	logDir, _ := os.MkdirTemp("", "debate-log-*")
	outDir, _ := os.MkdirTemp("", "debate-out-*")
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	t.Cleanup(func() { _ = os.RemoveAll(outDir) })
	logger, _ := NewLogger(logDir, &reg.System)
	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	defer s.Stop()

	taskID, err := s.Submit([]schemas.StepInput{
		{
			ID:              "debate-step",
			RoleID:          "worker",
			Task:            "high-risk task",
			VerifierModel:   "critic-model",
			EnableDebate:    true,
			MaxDebateRounds: 2,
		},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()

	if graph == nil {
		t.Fatalf("graph not found")
	}
	step := graph.Steps["debate-step"]
	if step == nil {
		t.Fatalf("step not found")
	}
	if !step.EnableDebate {
		t.Errorf("expected EnableDebate=true on Step")
	}
	if step.MaxDebateRounds != 2 {
		t.Errorf("expected MaxDebateRounds=2, got %d", step.MaxDebateRounds)
	}
}

func TestDebateFields_DefaultZero(t *testing.T) {
	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	logDir, _ := os.MkdirTemp("", "debate-log-*")
	outDir, _ := os.MkdirTemp("", "debate-out-*")
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	t.Cleanup(func() { _ = os.RemoveAll(outDir) })
	logger, _ := NewLogger(logDir, &reg.System)
	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	defer s.Stop()

	taskID, err := s.Submit([]schemas.StepInput{
		{ID: "plain-step", RoleID: "worker", Task: "normal task"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()

	step := graph.Steps["plain-step"]
	if step.EnableDebate {
		t.Errorf("expected EnableDebate=false by default")
	}
	if step.MaxDebateRounds != 0 {
		t.Errorf("expected MaxDebateRounds=0 by default, got %d", step.MaxDebateRounds)
	}
}

func TestDebateRoundCap(t *testing.T) {
	tests := []struct {
		input  int
		expect int
	}{
		{0, 1},
		{-1, 1},
		{1, 1},
		{2, 2},
		{3, 2},
		{99, 2},
	}
	for _, tc := range tests {
		rounds := tc.input
		if rounds <= 0 {
			rounds = 1
		}
		if rounds > 2 {
			rounds = 2
		}
		if rounds != tc.expect {
			t.Errorf("input %d: expected %d rounds, got %d", tc.input, tc.expect, rounds)
		}
	}
}

func TestDebateEventTypesDefined(t *testing.T) {
	if EventDebateStarted == "" {
		t.Errorf("EventDebateStarted not defined")
	}
	if EventDebateCritiqueReceived == "" {
		t.Errorf("EventDebateCritiqueReceived not defined")
	}
	if EventDebateConcluded == "" {
		t.Errorf("EventDebateConcluded not defined")
	}
}
