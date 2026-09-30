package core

import (
	"math"
	"strings"
)

// IRTBudgetEstimator implements Item Response Theory (IRT) based adaptive
// turn budget allocation. Instead of a one-size-fits-all static MaxToolTurns,
// it estimates task difficulty and maps it to a nonlinear turn budget,
// smoothed by historical data from the Experience Store.
//
// Design: docs/completed/2026-09-13/IRT_ADAPTIVE_TURNS_BUDGET_DESIGN.md
type IRTBudgetEstimator struct {
	BaseTurns  int
	DefaultMax int
}

func NewIRTBudgetEstimator(baseTurns int) *IRTBudgetEstimator {
	return &IRTBudgetEstimator{
		BaseTurns:  baseTurns,
		DefaultMax: 50,
	}
}

// EstimateDifficulty evaluates task difficulty coefficient b ∈ [0.5, 2.5]
// from lexical features of the task description and the file count.
func (e *IRTBudgetEstimator) EstimateDifficulty(taskDesc string, fileCount int) float64 {
	lengthFactor := math.Min(float64(len(taskDesc))/500.0, 1.5)
	fileFactor := math.Min(float64(fileCount)*0.2, 1.0)

	keywordBonus := 0.0
	lower := strings.ToLower(taskDesc)
	if strings.Contains(lower, "refactor") || strings.Contains(lower, "debug") {
		keywordBonus += 0.5
	}
	if strings.Contains(lower, "fix") {
		keywordBonus += 0.3
	}

	rawDifficulty := 1.0 + lengthFactor*0.2 + fileFactor*0.3 + keywordBonus
	if rawDifficulty < 0.5 {
		return 0.5
	}
	if rawDifficulty > 2.5 {
		return 2.5
	}
	return rawDifficulty
}

// CalculateAdaptiveTurns combines the IRT difficulty estimate with the
// historical average turns (from Experience Store) to produce a dynamic
// maxTurns. When historicalAvg ≤ 0 (cold start), BaseTurns is used as
// the Bayesian prior.
//
// This is the backward-compatible variant with theta = 0 (neutral model ability).
// See CalculateAdaptiveTurnsWithTheta for the true IRT variant.
func (e *IRTBudgetEstimator) CalculateAdaptiveTurns(taskDesc string, fileCount int, historicalAvg float64) int {
	return e.CalculateAdaptiveTurnsWithTheta(taskDesc, fileCount, historicalAvg, 0)
}

// CalculateAdaptiveTurnsWithTheta extends the IRT turn budget with a model
// ability parameter theta. The exponent is adjusted as:
//
//	adjustedTurns = historicalAvg × difficulty^(0.7 − theta × 0.15)
//
// - theta = 0 (neutral/cold start): identical to the legacy formula.
// - theta > 0 (capable model): exponent shrinks → tighter budget for hard tasks.
// - theta < 0 (weak model): exponent grows → more turns to compensate.
//
// See docs/architecture/MODEL_CAPABILITY_AND_INTELLIGENT_ROUTING_ROADMAP.md §2 Module C.
func (e *IRTBudgetEstimator) CalculateAdaptiveTurnsWithTheta(taskDesc string, fileCount int, historicalAvg float64, theta float64) int {
	difficulty := e.EstimateDifficulty(taskDesc, fileCount)

	if historicalAvg <= 0 {
		historicalAvg = float64(e.BaseTurns)
	}

	exponent := 0.7 - theta*0.15
	adjustedTurns := historicalAvg * math.Pow(difficulty, exponent)

	if adjustedTurns < 10 {
		return 10
	}
	if int(adjustedTurns) > e.DefaultMax*2 {
		return e.DefaultMax * 2
	}
	return int(adjustedTurns)
}

// EstimateTheta computes the IRT model ability parameter from observed success
// rate using the Rasch model MLE: theta = log(P / (1 - P)).
//
// - P = 0.5 → theta = 0 (neutral)
// - P → 1.0 → theta → +inf (very capable)
// - P → 0.0 → theta → -inf (incapable)
//
// The result is clamped to [-3, 3] to avoid extreme values on small samples.
func EstimateTheta(successRate float64) float64 {
	if successRate <= 0.01 {
		return -3.0
	}
	if successRate >= 0.99 {
		return 3.0
	}
	theta := math.Log(successRate / (1 - successRate))
	if theta < -3.0 {
		return -3.0
	}
	if theta > 3.0 {
		return 3.0
	}
	return theta
}

// EstimateDifficultyWithHistory augments the lexical difficulty estimate
// with a domain-familiarity signal derived from historical success rates.
//
// The insight: a "refactor" task in a familiar codebase is easier than in
// an unfamiliar one — but both have identical lexical features (same text
// length, same keywords, same file count). Historical success rate on the
// same capability is the cheapest available proxy for familiarity:
//
//	historicalSuccessRate < 0.5  → domain unfamiliar → difficulty × 1.3
//	historicalSuccessRate > 0.8  → domain familiar   → difficulty × 0.8
//	historicalSuccessRate ≤ 0    → no history, skip  → lexical only
//
// This closes the L4 gap where difficulty was purely lexical and could not
// distinguish "known territory" from "new territory" — the core of the RSI
// paper's environment-adaptation concept.
func (e *IRTBudgetEstimator) EstimateDifficultyWithHistory(taskDesc string, fileCount int, historicalSuccessRate float64) float64 {
	base := e.EstimateDifficulty(taskDesc, fileCount)

	if historicalSuccessRate > 0 {
		if historicalSuccessRate < 0.5 {
			base *= 1.3 // unfamiliar domain: harder
		} else if historicalSuccessRate > 0.8 {
			base *= 0.8 // familiar domain: easier
		}
	}

	if base < 0.5 {
		return 0.5
	}
	if base > 2.5 {
		return 2.5
	}
	return base
}
