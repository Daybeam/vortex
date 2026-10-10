package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/providers"
)

// ProbeTaskComplexity uses a System One provider to classify whether a task
// needs multi-step planning. The probe is a single non-autoregressive choice
// question ("yes"/"no") with abstention via score_threshold.
//
// Returns (true, nil) if the task is complex, (false, nil) if simple,
// (_, error) if the probe fails (caller should fall back to keyword).
//
// See docs/STEP_PLAN_MODE_DESIGN.md §9.3 (方案 B).
func ProbeTaskComplexity(probe providers.Provider, task string) (bool, error) {
	if probe == nil {
		return false, fmt.Errorf("probe: provider is nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := probe.Complete(ctx, providers.CompleteRequest{
		User: task,
		Constraints: map[string]any{
			"questions": map[string]any{
				"complexity": map[string]any{
					"type":    "choice",
					"options": []string{"yes", "no"},
					"question": "This task requires multi-step planning?",
				},
			},
		},
	})
	if err != nil {
		return false, fmt.Errorf("probe: provider call: %w", err)
	}

	// System One returns choice answers as ToolCalls with typed Arguments.
	for _, tc := range resp.ToolCalls {
		if tc.Name == "complexity" {
			if choice, ok := tc.Arguments["choice"].(string); ok {
				return strings.EqualFold(choice, "yes"), nil
			}
		}
	}

	// Empty ToolCalls → abstention (score_threshold filtered the answer).
	return false, fmt.Errorf("probe: no complexity answer in response (abstained)")
}
