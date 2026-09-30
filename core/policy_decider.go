package core

import (
	"math"

	"github.com/daybeam/vortex/config"
)

// ── Policy Decision ─────────────────────────────────────────────────────────
//
// Separates "what to do with signals" (PolicyDecider) from "where data comes
// from" (SignalCollector). The default implementation is the existing IRT +
// strategy bias logic, moved here verbatim from spawner_policy.go.

// PolicyDecision is the output of a policy decision: how many turns to allow
// and which skills to inject.
type PolicyDecision struct {
	MaxTurns       int
	InjectedSkills []string
}

// PolicyDecider computes a policy decision from collected signals.
// Default impl: IRT-based turn budgeting + theta-based skill injection.
// Future impls: bandit-based, rule-based, A/B comparison.
type PolicyDecider interface {
	Decide(signals PolicySignals, req *SpawnRequest, role *config.Role) PolicyDecision
}

// irtPolicyDecider is the default implementation using IRT difficulty estimation
// and theta-based strategy bias.
type irtPolicyDecider struct {
	irtEstimator *IRTBudgetEstimator
	registry     *config.Registry
}

// NewIRTPolicyDecider creates the default IRT-based PolicyDecider.
func NewIRTPolicyDecider(irtEstimator *IRTBudgetEstimator, registry *config.Registry) PolicyDecider {
	return &irtPolicyDecider{
		irtEstimator: irtEstimator,
		registry:     registry,
	}
}

func (d *irtPolicyDecider) Decide(signals PolicySignals, req *SpawnRequest, role *config.Role) PolicyDecision {
	decision := PolicyDecision{}

	// ── MaxTurns: IRT adaptive budget ────────────────────────────────────
	maxTurns := 50
	if d.registry != nil && d.registry.System.MaxToolTurns > 0 {
		maxTurns = d.registry.System.MaxToolTurns
	}

	if d.irtEstimator != nil {
		historicalAvg := signals.HistoricalAvgTurns
		difficulty := d.irtEstimator.EstimateDifficultyWithHistory(
			req.Task, len(req.ContextRefs), signals.HistoricalSuccessRate)

		if historicalAvg <= 0 {
			historicalAvg = float64(d.irtEstimator.BaseTurns)
		}

		theta := signals.Theta
		exponent := 0.7 - theta*0.15
		adjustedTurns := historicalAvg * math.Pow(difficulty, exponent)

		irtTurns := int(adjustedTurns)
		if irtTurns > maxTurns {
			maxTurns = irtTurns
		}
	}

	decision.MaxTurns = maxTurns

	// ── InjectedSkills: strategy bias for weak models ───────────────────
	// When theta < -0.5 (success rate < ~38%), inject proven skill
	// recommendations to guide the model toward known-good approaches.
	if signals.Theta < -0.5 && len(signals.RecommendedSkills) > 0 {
		maxSkills := role.MaxAdditionalSkills
		if maxSkills <= 0 {
			maxSkills = 3
		}
		existing := len(req.AdditionalSkills)
		injectCount := maxSkills - existing
		if injectCount > 0 {
			for _, skillID := range signals.RecommendedSkills {
				if injectCount <= 0 {
					break
				}
				// Don't duplicate existing skills
				dup := false
				for _, ex := range req.AdditionalSkills {
					if ex == skillID {
						dup = true
						break
					}
				}
				if !dup {
					decision.InjectedSkills = append(decision.InjectedSkills, skillID)
					injectCount--
				}
			}
		}
	}

	return decision
}
