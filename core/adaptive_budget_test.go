package core

import "testing"

func TestIRTBudgetEstimator_EstimateDifficulty(t *testing.T) {
	e := NewIRTBudgetEstimator(50)

	tests := []struct {
		name  string
		task  string
		files int
		minB  float64
		maxB  float64
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
