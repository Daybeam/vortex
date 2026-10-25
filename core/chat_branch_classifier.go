package core

import (
	"fmt"
	"strings"
)

// Chat branching auto-detection (Mode 3, 2026-10-24).
// See docs/plan/CHAT_BRANCHING_AND_QUOTE_DESIGN.md §5-6.
//
// Lightweight: keyword-overlap similarity (local, free) + rough token-cost
// heuristic. Prefers quote (KV-cache-friendly) over fork (cache-breaking).
// Returns a transparent decision with human-readable reasoning.

// BranchDecision is the output of the auto-detection.
type BranchDecision struct {
	Mode          string // "continue" | "quote" | "fork"
	QuoteTargetID string // the message ID to quote/fork from
	Reasoning     string // human-readable explanation (transparency)
}

// ClassifyBranchIntent decides whether a new message should continue the
// current branch, quote an earlier message, or fork from a branch point.
// Call this ONLY when the user hasn't explicitly chosen a mode (parentID=""
// AND replyTo=""). Lightweight: keyword overlap + rough token-cost.
// @decision:DECISION-CHAT-BRANCH-AUTO
func ClassifyBranchIntent(newMessage string, session *ChatSession) BranchDecision {
	if session == nil {
		return BranchDecision{Mode: "continue", Reasoning: "no session"}
	}
	session.mu.Lock()
	defer session.mu.Unlock()

	path := session.getPathLocked(session.ActiveLeafID)
	if len(path) == 0 {
		return BranchDecision{Mode: "continue", Reasoning: "empty path"}
	}

	leaf := path[len(path)-1]
	leafSim := keywordSimilarity(newMessage, leaf.Content)

	var bestTarget *ChatMessage
	var bestSim float64
	for i := len(path) - 2; i >= 0; i-- {
		sim := keywordSimilarity(newMessage, path[i].Content)
		if sim > bestSim {
			bestSim = sim
			bestTarget = &path[i]
		}
	}

	// Conservative thresholds (prefer continue/quote over fork).
	const (
		continueThreshold = 0.30
		quoteThreshold    = 0.40
		forkThreshold     = 0.15
	)

	switch {
	case leafSim >= continueThreshold:
		return BranchDecision{Mode: "continue",
			Reasoning: fmt.Sprintf("similarity to last msg=%.2f (≥%.2f) → continue", leafSim, continueThreshold)}

	case bestTarget != nil && bestSim >= quoteThreshold:
		// Token-cost check: is quoting cheaper than forking?
		quoteCost := len(bestTarget.Content) / 4 // rough: 1 token ≈ 4 chars
		forkCost := 0
		for _, m := range path {
			forkCost += len(m.Content) / 4
		}
		if quoteCost < forkCost {
			return BranchDecision{Mode: "quote", QuoteTargetID: bestTarget.ID,
				Reasoning: fmt.Sprintf("similarity to msg %s=%.2f (≥%.2f), quote_cost=%d < fork_cost=%d → quote (KV-cache-friendly)",
					bestTarget.ID, bestSim, quoteThreshold, quoteCost, forkCost)}
		}
		return BranchDecision{Mode: "fork", QuoteTargetID: bestTarget.ID,
			Reasoning: fmt.Sprintf("similarity to msg %s=%.2f but quote_cost=%d ≥ fork_cost=%d → fork",
				bestTarget.ID, bestSim, quoteCost, forkCost)}

	case leafSim < forkThreshold && bestSim < forkThreshold:
		return BranchDecision{Mode: "fork", QuoteTargetID: leaf.ID,
			Reasoning: fmt.Sprintf("low similarity (leaf=%.2f, best=%.2f, both <%.2f) → fork (topic shift)",
				leafSim, bestSim, forkThreshold)}

	default:
		return BranchDecision{Mode: "continue",
			Reasoning: fmt.Sprintf("no strong signal (leaf=%.2f, best=%.2f) → continue (default)", leafSim, bestSim)}
	}
}

// keywordSimilarity computes a simple keyword-overlap ratio between two
// strings. Local, free, no API call. Returns a float in [0, 1].
func keywordSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	wordsA := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(a)) {
		wordsA[w] = true
	}
	if len(wordsA) == 0 {
		return 0
	}
	overlap := 0
	for _, w := range strings.Fields(strings.ToLower(b)) {
		if wordsA[w] {
			overlap++
		}
	}
	return float64(overlap) / float64(len(wordsA))
}
