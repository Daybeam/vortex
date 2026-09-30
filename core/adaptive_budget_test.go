package core

import "testing"

func TestIRTBudgetEstimator_EstimateDifficulty(t *testing.T) {
	e := NewIRTBudgetEstimator(50)

	tests := []struct {
		name     string
		task     string
		files    int
		minB     float64
		maxB     float64
	}{
		{"simple short task", "check if file exists", 1, 0.5, 1.5},
		{"refactor keyword", "refactor the auth module", 3, 1.5, 2.5},
		{"debug keyword", "debug race condition in scheduler", 5, 1.5, 2.5},
		{"fix keyword", "fix typo in config", 1, 0.5, 2.0},
		{"long complex task", string(make([]byte, 500)), 10, 1.5, 2.5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := e.EstimateDifficulty(tc.task, tc.files)
			if b < tc.minB || b > tc.maxB {
				t.Errorf("difficulty %v not in [%v, %v]", b, tc.minB, tc.maxB)
			}
		})
	}
}

func TestIRTBudgetEstimator_CalculateAdaptiveTurns(t *testing.T) {
	e := NewIRTBudgetEstimator(50)

	turns := e.CalculateAdaptiveTurns("simple check", 0, 0)
	if turns < 10 {
		t.Errorf("cold start should give >= 10 turns, got %d", turns)
	}

	turns = e.CalculateAdaptiveTurns("simple check", 0, 15)
	if turns < 10 || turns > 100 {
		t.Errorf("low-difficulty task with low history should give modest budget, got %d", turns)
	}

	turns = e.CalculateAdaptiveTurns("refactor cross-module architecture", 8, 80)
	if turns <= 50 {
		t.Errorf("high-difficulty task with high history should give large budget, got %d", turns)
	}

	turns = e.CalculateAdaptiveTurns("simple", 0, 200)
	if turns > 100 {
		t.Errorf("should be capped at DefaultMax*2=100, got %d", turns)
	}
}

func TestCalculateAdaptiveTurnsWithTheta_BackwardCompatible(t *testing.T) {
	e := NewIRTBudgetEstimator(50)
	task := "refactor cross-module architecture"
	files := 8
	hist := 80.0

	legacy := e.CalculateAdaptiveTurns(task, files, hist)
	withTheta0 := e.CalculateAdaptiveTurnsWithTheta(task, files, hist, 0)
	if legacy != withTheta0 {
		t.Errorf("theta=0 should match legacy: %d vs %d", legacy, withTheta0)
	}
}

func TestCalculateAdaptiveTurnsWithTheta_CapableModelTighterBudget(t *testing.T) {
	e := NewIRTBudgetEstimator(50)
	task := "refactor cross-module architecture"
	files := 8
	hist := 20.0

	neutral := e.CalculateAdaptiveTurnsWithTheta(task, files, hist, 0)
	capable := e.CalculateAdaptiveTurnsWithTheta(task, files, hist, 2.0)
	if capable >= neutral {
		t.Errorf("capable model (theta=2.0) should get fewer turns: capable=%d neutral=%d", capable, neutral)
	}
}

func TestCalculateAdaptiveTurnsWithTheta_WeakModelExpandedBudget(t *testing.T) {
	e := NewIRTBudgetEstimator(50)
	task := "refactor cross-module architecture"
	files := 8
	hist := 20.0

	neutral := e.CalculateAdaptiveTurnsWithTheta(task, files, hist, 0)
	weak := e.CalculateAdaptiveTurnsWithTheta(task, files, hist, -1.0)
	if weak <= neutral {
		t.Errorf("weak model (theta=-1.0) should get more turns: weak=%d neutral=%d", weak, neutral)
	}
}

func TestEstimateTheta_Neutral(t *testing.T) {
	theta := EstimateTheta(0.5)
	if theta < -0.01 || theta > 0.01 {
		t.Errorf("theta for 50%% success should be ~0, got %f", theta)
	}
}

func TestEstimateTheta_HighSuccessPositive(t *testing.T) {
	theta := EstimateTheta(0.9)
	if theta <= 0 {
		t.Errorf("theta for 90%% success should be positive, got %f", theta)
	}
}

func TestEstimateTheta_LowSuccessNegative(t *testing.T) {
	theta := EstimateTheta(0.1)
	if theta >= 0 {
		t.Errorf("theta for 10%% success should be negative, got %f", theta)
	}
}

func TestEstimateTheta_Clamped(t *testing.T) {
	if theta := EstimateTheta(0.0); theta != -3.0 {
		t.Errorf("theta for 0%% success should be clamped to -3.0, got %f", theta)
	}
	if theta := EstimateTheta(1.0); theta != 3.0 {
		t.Errorf("theta for 100%% success should be clamped to 3.0, got %f", theta)
	}
}

// TestEstimateDifficultyWithHistory_UnfamiliarDomain verifies that a low
// historical success rate increases difficulty (×1.3 multiplier).
// Bug: L4 gap — difficulty was purely lexical, couldn't distinguish
// "known" from "new" territory. Fixed by EstimateDifficultyWithHistory.
func TestEstimateDifficultyWithHistory_UnfamiliarDomain(t *testing.T) {
	e := NewIRTBudgetEstimator(50)
	task := "refactor the auth module"
	files := 3

	// Baseline lexical difficulty
	lexical := e.EstimateDifficulty(task, files)

	// Unfamiliar domain (30% success) should increase difficulty
	withHistory := e.EstimateDifficultyWithHistory(task, files, 0.3)
	if withHistory <= lexical {
		t.Errorf("unfamiliar domain (30%% success) should increase difficulty: withHistory=%v lexical=%v", withHistory, lexical)
	}
	// Verify the multiplier is approximately 1.3x
	ratio := withHistory / lexical
	if ratio < 1.25 || ratio > 1.35 {
		t.Errorf("expected ~1.3x multiplier, got %.2fx", ratio)
	}
}

// TestEstimateDifficultyWithHistory_FamiliarDomain verifies that a high
// historical success rate decreases difficulty (×0.8 multiplier).
func TestEstimateDifficultyWithHistory_FamiliarDomain(t *testing.T) {
	e := NewIRTBudgetEstimator(50)
	task := "refactor the auth module"
	files := 3

	lexical := e.EstimateDifficulty(task, files)
	withHistory := e.EstimateDifficultyWithHistory(task, files, 0.9)
	if withHistory >= lexical {
		t.Errorf("familiar domain (90%% success) should decrease difficulty: withHistory=%v lexical=%v", withHistory, lexical)
	}
	ratio := withHistory / lexical
	if ratio < 0.75 || ratio > 0.85 {
		t.Errorf("expected ~0.8x multiplier, got %.2fx", ratio)
	}
}

// TestEstimateDifficultyWithHistory_NoHistory verifies that zero historical
// data returns the baseline lexical difficulty (no adjustment).
func TestEstimateDifficultyWithHistory_NoHistory(t *testing.T) {
	e := NewIRTBudgetEstimator(50)
	task := "refactor the auth module"
	files := 3

	lexical := e.EstimateDifficulty(task, files)
	withHistory := e.EstimateDifficultyWithHistory(task, files, 0)
	if withHistory != lexical {
		t.Errorf("no history (0%% success) should match lexical: withHistory=%v lexical=%v", withHistory, lexical)
	}
}

// TestEstimateDifficultyWithHistory_ClampBoundaries verifies the [0.5, 2.5]
// clamp still applies after history adjustment.
func TestEstimateDifficultyWithHistory_ClampBoundaries(t *testing.T) {
	e := NewIRTBudgetEstimator(50)
	// Simple task with low lexical difficulty
	simple := e.EstimateDifficulty("check", 0)
	// Even with unfamiliar domain, should not go below 0.5
	unfamiliar := e.EstimateDifficultyWithHistory("check", 0, 0.2)
	if unfamiliar < 0.5 {
		t.Errorf("should clamp to >= 0.5, got %v (baseline was %v)", unfamiliar, simple)
	}

	// Complex task with high lexical difficulty
	complex_ := e.EstimateDifficulty("refactor cross-module architecture", 10)
	// Even with familiar domain, should not exceed 2.5
	familiar := e.EstimateDifficultyWithHistory("refactor cross-module architecture", 10, 0.95)
	if familiar > 2.5 {
		t.Errorf("should clamp to <= 2.5, got %v (baseline was %v)", familiar, complex_)
	}
}
