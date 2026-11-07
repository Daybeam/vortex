package store

import (
	"context"
	"testing"
	"time"
)

func TestRecordTaskCompletion_PopulatesAuthorityFields(t *testing.T) {
	es := newTestStore(t)

	decisions := []map[string]any{
		{
			"decision_type": "step_failed",
			"role_id":       "worker_1",
			"choice":        "skip",
			"proposer":      "agent",
			"decider":       "agent",
		},
		{
			"decision_type": "human_approval_required",
			"role_id":       "worker_2",
			"choice":        "approve",
			"proposer":      "agent",
			"decider":       "human",
			"intervention":  "informational",
		},
	}

	err := es.RecordTaskCompletion(
		context.Background(),
		"task_1",
		nil,
		0.8,
		decisions,
		true,
		false,
	)
	if err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}

	es.Mu.RLock()
	defer es.Mu.RUnlock()
	if len(es.DecisionOutcomes) != 2 {
		t.Fatalf("expected 2 DecisionOutcomes, got %d", len(es.DecisionOutcomes))
	}

	do0 := es.DecisionOutcomes[0]
	if do0.Proposer != "agent" {
		t.Errorf("do0.Proposer: expected agent, got %q", do0.Proposer)
	}
	if do0.Decider != "agent" {
		t.Errorf("do0.Decider: expected agent, got %q", do0.Decider)
	}
	if do0.Intervention != "" {
		t.Errorf("do0.Intervention: expected empty, got %q", do0.Intervention)
	}

	do1 := es.DecisionOutcomes[1]
	if do1.Proposer != "agent" {
		t.Errorf("do1.Proposer: expected agent, got %q", do1.Proposer)
	}
	if do1.Decider != "human" {
		t.Errorf("do1.Decider: expected human, got %q", do1.Decider)
	}
	if do1.Intervention != "informational" {
		t.Errorf("do1.Intervention: expected informational, got %q", do1.Intervention)
	}
}

func TestRecordTaskCompletion_InfersDeciderFromDecisionType(t *testing.T) {
	es := newTestStore(t)

	decisions := []map[string]any{
		{
			"decision_type": "step_failed",
			"role_id":       "worker_1",
			"choice":        "skip",
		},
		{
			"decision_type": "human_approval_required",
			"role_id":       "worker_2",
			"choice":        "approve",
		},
		{
			"decision_type": "autonomous_abort_requested",
			"role_id":       "worker_3",
			"choice":        "abort",
		},
		{
			"decision_type": "goal_change_requested",
			"role_id":       "worker_4",
			"choice":        "change",
		},
		{
			"decision_type": "low_confidence",
			"role_id":       "worker_5",
			"choice":        "retry",
		},
	}

	err := es.RecordTaskCompletion(
		context.Background(),
		"task_1",
		nil,
		0.8,
		decisions,
		true,
		false,
	)
	if err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}

	es.Mu.RLock()
	defer es.Mu.RUnlock()

	expectations := []struct {
		idx          int
		decider      string
		intervention string
	}{
		{0, "agent", ""},
		{1, "human", "informational"},
		{2, "human", "informational"},
		{3, "human", "informational"},
		{4, "agent", ""},
	}

	for _, exp := range expectations {
		do := es.DecisionOutcomes[exp.idx]
		if do.Decider != exp.decider {
			t.Errorf("outcome[%d].Decider: expected %q, got %q", exp.idx, exp.decider, do.Decider)
		}
		if do.Intervention != exp.intervention {
			t.Errorf("outcome[%d].Intervention: expected %q, got %q", exp.idx, exp.intervention, do.Intervention)
		}
		if do.Proposer != "agent" {
			t.Errorf("outcome[%d].Proposer: expected agent, got %q", exp.idx, do.Proposer)
		}
	}
}

func TestRecordTaskCompletion_ExplicitFieldsOverrideInference(t *testing.T) {
	es := newTestStore(t)

	decisions := []map[string]any{
		{
			"decision_type": "human_approval_required",
			"role_id":       "worker_1",
			"choice":        "approve",
			"proposer":      "verifier",
			"decider":       "agent",
			"intervention":  "edit",
		},
	}

	err := es.RecordTaskCompletion(
		context.Background(),
		"task_1",
		nil,
		0.8,
		decisions,
		true,
		false,
	)
	if err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}

	es.Mu.RLock()
	defer es.Mu.RUnlock()
	do := es.DecisionOutcomes[0]
	if do.Proposer != "verifier" {
		t.Errorf("Proposer: expected verifier (explicit), got %q", do.Proposer)
	}
	if do.Decider != "agent" {
		t.Errorf("Decider: expected agent (explicit override), got %q", do.Decider)
	}
	if do.Intervention != "edit" {
		t.Errorf("Intervention: expected edit (explicit), got %q", do.Intervention)
	}
}

func TestQueryDecisionAdvice_PropagatesAuthorityFields(t *testing.T) {
	es := newTestStore(t)

	now := time.Now()
	es.DecisionOutcomes = []*DecisionOutcome{
		{
			DecisionType: "step_failed",
			RoleID:       "worker_1",
			Choice:       "skip",
			Resolved:     true,
			Proposer:     "agent",
			Decider:      "agent",
			Timestamp:    now,
		},
		{
			DecisionType: "step_failed",
			RoleID:       "worker_1",
			Choice:       "skip",
			Resolved:     true,
			Proposer:     "agent",
			Decider:      "agent",
			Timestamp:    now.Add(time.Second),
		},
		{
			DecisionType: "step_failed",
			RoleID:       "worker_1",
			Choice:       "retry",
			Resolved:     true,
			Proposer:     "verifier",
			Decider:      "human",
			Intervention: "informational",
			Timestamp:    now.Add(2 * time.Second),
		},
	}

	result := es.QueryDecisionAdvice(context.Background(), "step_failed", "worker_1")
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	if result.Proposer != "agent" {
		t.Errorf("Proposer: expected agent (majority), got %q", result.Proposer)
	}
	if result.Decider != "agent" {
		t.Errorf("Decider: expected agent (majority), got %q", result.Decider)
	}
	if result.Choice != "skip" {
		t.Errorf("Choice: expected skip (majority), got %q", result.Choice)
	}
}

func TestJudgmentDensity(t *testing.T) {
	es := newTestStore(t)

	now := time.Now()
	es.DecisionOutcomes = []*DecisionOutcome{
		{Decider: "agent", Timestamp: now},
		{Decider: "agent", Timestamp: now},
		{Decider: "agent", Timestamp: now},
		{Decider: "human", Intervention: "informational", Timestamp: now},
	}

	density := es.JudgmentDensity(now.Add(-time.Hour))
	if density != 3.0 {
		t.Errorf("expected density 3.0 (3 agent / 1 human), got %f", density)
	}
}

func TestJudgmentDensity_NoInterventions(t *testing.T) {
	es := newTestStore(t)

	now := time.Now()
	es.DecisionOutcomes = []*DecisionOutcome{
		{Decider: "agent", Timestamp: now},
		{Decider: "agent", Timestamp: now},
	}

	density := es.JudgmentDensity(now.Add(-time.Hour))
	if density != 0 {
		t.Errorf("expected density 0 (no interventions), got %f", density)
	}
}

func TestJudgmentDensity_TimeWindow(t *testing.T) {
	es := newTestStore(t)

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now()

	es.DecisionOutcomes = []*DecisionOutcome{
		{Decider: "agent", Timestamp: old},
		{Decider: "human", Intervention: "informational", Timestamp: old},
		{Decider: "agent", Timestamp: recent},
		{Decider: "agent", Timestamp: recent},
		{Decider: "human", Intervention: "informational", Timestamp: recent},
	}

	densityAll := es.JudgmentDensity(old.Add(-time.Minute))
	if densityAll != 1.5 {
		t.Errorf("full window: expected density 1.5 (3 agent / 2 human), got %f", densityAll)
	}

	densityRecent := es.JudgmentDensity(recent.Add(-time.Minute))
	if densityRecent != 2.0 {
		t.Errorf("recent window: expected density 2.0 (2 agent / 1 human), got %f", densityRecent)
	}
}

func TestGetDecisionAuthorityStats(t *testing.T) {
	es := newTestStore(t)

	now := time.Now()
	es.DecisionOutcomes = []*DecisionOutcome{
		{Proposer: "agent", Decider: "agent", Timestamp: now},
		{Proposer: "agent", Decider: "agent", Timestamp: now},
		{Proposer: "verifier", Decider: "human", Intervention: "informational", Timestamp: now},
		{Proposer: "agent", Decider: "agent", Timestamp: now},
	}

	stats := es.GetDecisionAuthorityStats(now.Add(-time.Hour))
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}

	if stats.ProposerDistribution["agent"] != 3 {
		t.Errorf("ProposerDistribution[agent]: expected 3, got %d", stats.ProposerDistribution["agent"])
	}
	if stats.ProposerDistribution["verifier"] != 1 {
		t.Errorf("ProposerDistribution[verifier]: expected 1, got %d", stats.ProposerDistribution["verifier"])
	}
	if stats.DeciderDistribution["agent"] != 3 {
		t.Errorf("DeciderDistribution[agent]: expected 3, got %d", stats.DeciderDistribution["agent"])
	}
	if stats.DeciderDistribution["human"] != 1 {
		t.Errorf("DeciderDistribution[human]: expected 1, got %d", stats.DeciderDistribution["human"])
	}
	if stats.JudgmentDensity != 3.0 {
		t.Errorf("JudgmentDensity: expected 3.0, got %f", stats.JudgmentDensity)
	}
}

func TestMostCommon(t *testing.T) {
	tests := []struct {
		input    map[string]int
		expected string
	}{
		{map[string]int{"a": 3, "b": 1}, "a"},
		{map[string]int{"a": 1, "b": 3}, "b"},
		{map[string]int{"a": 2, "b": 2}, "a"},
		{map[string]int{}, ""},
	}
	for _, tc := range tests {
		got := mostCommon(tc.input)
		if got != tc.expected {
			t.Errorf("mostCommon(%v): expected %q, got %q", tc.input, tc.expected, got)
		}
	}
}

func TestDecisionOutcome_JSONSerialization(t *testing.T) {
	es := newTestStore(t)

	decisions := []map[string]any{
		{
			"decision_type": "human_approval_required",
			"role_id":       "worker_1",
			"choice":        "approve",
		},
	}

	err := es.RecordTaskCompletion(
		context.Background(),
		"task_1",
		nil,
		0.9,
		decisions,
		true,
		false,
	)
	if err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}

	es.Mu.RLock()
	do := es.DecisionOutcomes[0]
	es.Mu.RUnlock()

	if do.Proposer != "agent" {
		t.Errorf("Proposer: expected agent, got %q", do.Proposer)
	}
	if do.Decider != "human" {
		t.Errorf("Decider: expected human, got %q", do.Decider)
	}
	if do.Intervention != "informational" {
		t.Errorf("Intervention: expected informational, got %q", do.Intervention)
	}
}
