package core

import (
	"sync"
)

// CacheSentinel detects sustained prompt-cache inefficiency.
// Fires when CacheReadTokens == 0 for >= threshold consecutive rounds
// where PromptTokens > minPromptTokens (small prompts are cold by design).
//
// Design ref: docs/architecture/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §2.2 告警器1.
// This is a pure observer: it never mutates execution flow. Alerting ≠ blocking.
type CacheSentinel struct {
	mu              sync.Mutex
	consecutiveZero map[string]int
	threshold       int
	minPromptTokens int
}

func NewCacheSentinel() *CacheSentinel {
	return &CacheSentinel{
		consecutiveZero: make(map[string]int),
		threshold:       3,
		minPromptTokens: 5000,
	}
}

// Observe records one round and returns (shouldAlert, consecutiveCount).
// Keyed by taskID+stepID so concurrent steps don't cross-contaminate.
func (c *CacheSentinel) Observe(taskID, stepID string, promptTokens, cacheRead int) (bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := taskID + ":" + stepID
	if promptTokens > c.minPromptTokens && cacheRead == 0 {
		c.consecutiveZero[key]++
	} else {
		c.consecutiveZero[key] = 0
	}
	n := c.consecutiveZero[key]
	return n >= c.threshold, n
}

// Reset clears the counter for a key (call when a step completes or task resets).
func (c *CacheSentinel) Reset(taskID, stepID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.consecutiveZero, taskID+":"+stepID)
}

// BudgetSentinel emits soft alerts at 60/80/90% budget consumption with a
// linear extrapolation of the predicted total. It never auto-extends the budget.
//
// Design ref: §2.2 告警器2. Prediction ≠ decision.
type BudgetSentinel struct {
	mu    sync.Mutex
	fired map[string]map[int]bool // taskID -> tier(60/80/90) -> fired
	tiers []int
}

func NewBudgetSentinel() *BudgetSentinel {
	return &BudgetSentinel{
		fired: make(map[string]map[int]bool),
		tiers: []int{60, 80, 90},
	}
}

// Observe returns the tier that should be alerted this round (0 if none).
// pct = int(100 * tokensUsed / tokenBudget). predictedPct is the extrapolated
// final percentage given current progress (0 if not computable).
func (b *BudgetSentinel) Observe(taskID string, pct, predictedPct int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fired[taskID] == nil {
		b.fired[taskID] = make(map[int]bool)
	}
	highest := 0
	for _, tier := range b.tiers {
		if pct >= tier && !b.fired[taskID][tier] {
			b.fired[taskID][tier] = true
			if tier > highest {
				highest = tier
			}
		}
	}
	if highest == 0 && predictedPct >= 80 {
		for _, tier := range b.tiers {
			if predictedPct >= tier && !b.fired[taskID][tier] {
				b.fired[taskID][tier] = true
				if tier > highest {
					highest = tier
				}
			}
		}
	}
	return highest
}

// Reset clears all fired tiers for a task (call on task completion).
func (b *BudgetSentinel) Reset(taskID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.fired, taskID)
}
