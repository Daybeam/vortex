package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/store"
)

// ContextTier represents the priority level of context injection.
type ContextTier int

const (
	ContextHot  ContextTier = iota // Required: current task spec, step input, latest error
	ContextWarm                    // Relevant: matched experience nodes (successes + failures)
	ContextCold                    // Background: SOPs, role cookbooks, skill index
)

// ActiveContextAssembler rebuilds the prompt before each step execution.
type ActiveContextAssembler struct {
	Graph       store.IExperienceStore
	EmbedClient store.IEmbeddingClient
	TokenBudget int // total token budget for context injection
}

func NewActiveContextAssembler(gs store.IExperienceStore, ec store.IEmbeddingClient, budget int) *ActiveContextAssembler {
	return &ActiveContextAssembler{
		Graph:       gs,
		EmbedClient: ec,
		TokenBudget: budget,
	}
}

// Assemble builds the context string for a step execution.
// Called by executeStep() before spawning the sub-agent.
func (a *ActiveContextAssembler) Assemble(
	taskSpec string, // Hot: current task description
	stepInput string, // Hot: current step input
	latestError string, // Hot: error from previous attempt (if retrying)
	capability string, // Warm: for graph retrieval
	roleID string, // Cold: for role cookbook lookup
) string {
	var sb strings.Builder

	// === HOT CONTEXT (always injected) ===
	sb.WriteString("## Current Task\n")
	sb.WriteString(taskSpec)
	sb.WriteString("\n\n## Current Step\n")
	sb.WriteString(stepInput)
	if latestError != "" {
		sb.WriteString("\n\n## Previous Error\n")
		sb.WriteString(latestError)
	}

	// === WARM CONTEXT (graph-aware retrieval, token-budgeted) ===
	if a.Graph != nil {
		warmBudget := a.TokenBudget / 2 // 50% for warm context
		// audit L5: bounded context prevents embedding/retrieval from hanging
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var query []float32
		if a.EmbedClient != nil {
			// Compute embedding for the current state (input + error)
			emb, _, err := a.EmbedClient.EmbedWithModel(ctx, stepInput+" "+latestError)
			if err == nil {
				query = emb
			}
		}

		nodes := a.Graph.RetrieveRelevantExperience(ctx, query, capability, latestError, warmBudget)
		if len(nodes) > 0 {
			sb.WriteString("\n\n[LEARNED EXPERIENCE PRECEDENT]\n")
			for _, n := range nodes {
				if n.Outcome == "failure" {
					if n.FailureMode != "" {
						sb.WriteString(fmt.Sprintf("## Known Pitfall (Failure Mode: %s)\n", n.FailureMode))
						sb.WriteString(fmt.Sprintf("- Task Pattern: %s\n", n.SourceText))
						sb.WriteString(fmt.Sprintf("- Wrong Approach: %s\n", n.Strategy))
						if n.Critique != "" {
							sb.WriteString(fmt.Sprintf("- Critique: %s\n\n", n.Critique))
						}
					} else {
						sb.WriteString(fmt.Sprintf("- ⚠️ Failure: %s → error: %s (avoid this path)\n",
							n.Strategy, n.ErrorSignal))
					}
				} else {
					sb.WriteString(fmt.Sprintf("- ✅ Success: %s (confidence: %.2f)\n",
						n.Strategy, n.Confidence))
				}
			}
		}

		// === TIER 3: AntiPatterns (Protected Zone) ===
		aps := a.Graph.QueryRelevantAntiPatterns(stepInput+" "+latestError, 3)
		if len(aps) > 0 {
			sb.WriteString("\n\n[HISTORICAL PITFALL WARNING]\n")
			for _, p := range aps {
				sb.WriteString(fmt.Sprintf("- ⚠️ Anti-Pattern: %s\n", p.AntiPattern))
				sb.WriteString(fmt.Sprintf("  Symptom: %s\n", p.Symptom))
				sb.WriteString(fmt.Sprintf("  Correct Action: %s\n", p.CorrectPattern))
			}
		}
	}

	// === COLD CONTEXT (fills remaining budget) ===
	// SOPs, role cookbook, and skill index lookup are intentionally not
	// included here yet — they require ContextArchive or specialized lookup
	// infrastructure planned for a future phase.

	return sb.String()
}
