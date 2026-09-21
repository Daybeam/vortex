package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
)

// RunFMCBatch scans for failure nodes with empty FailureMode and backfills them
// using a weak LLM (e.g. GPT-4o-mini or Gemini Flash).
func (es *ExperienceStore) RunFMCBatch(ctx context.Context, provider interfaces.Provider, modelID string) (int, error) {
	es.Mu.Lock()
	var pendingNodes []*ExperienceNode
	for _, node := range es.Nodes {
		if node.Outcome == "failure" && node.FailureMode == "" {
			pendingNodes = append(pendingNodes, node)
		}
	}
	es.Mu.Unlock()

	if len(pendingNodes) == 0 {
		return 0, nil
	}

	log.Printf("[FMC] Starting batch classification for %d nodes using %s", len(pendingNodes), modelID)

	count := 0
	for _, node := range pendingNodes {
		// 1. Construct critique prompt
		prompt := fmt.Sprintf(`Analyze this agent failure:
Task: %s
Capability: %s
Action attempted: %s
Error signal: %s

Assign a FailureMode (one of: tool_selection_error, auth_permission_denied, rate_limit_hit, timeout_exceeded, context_overflow, logic_error)
And provide a 1-sentence Critique of how to avoid this.
Output MUST be valid JSON: {"failure_mode": "...", "critique": "..."}`, node.SourceText, node.Capability, node.Action, node.ErrorSignal)

		// 2. Call weak LLM
		resp, err := provider.Complete(ctx, schemas.CompleteRequest{
			Model:  modelID,
			System: "",
			User:   prompt,
		})
		if err != nil {
			log.Printf("[FMC] Failed to classify node %s: %v", node.NodeID, err)
			continue
		}

		// 3. Parse and Update
		var result struct {
			FailureMode string `json:"failure_mode"`
			Critique    string `json:"critique"`
		}

		cleanJSON := extractJSON(resp.Text)
		if err := json.Unmarshal([]byte(cleanJSON), &result); err != nil {
			log.Printf("[FMC] Failed to parse JSON for node %s: %v", node.NodeID, err)
			continue
		}

		es.Mu.Lock()
		node.FailureMode = result.FailureMode
		node.Critique = result.Critique
		es.Nodes[node.NodeID] = node
		es.Mu.Unlock()
		count++
	}

	log.Printf("[FMC] Batch classification complete. Updated %d nodes.", count)
	return count, es.PersistAll(ctx)
}

func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return s
	}
	return s[start : end+1]
}
