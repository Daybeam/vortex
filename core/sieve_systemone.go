package core

import (
	"context"
	"fmt"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
)

func (s *Sieve) SetSystemOneProvider(p *providers.SystemOneProvider, cfg config.SystemOneConfig) {
	if s == nil {
		return
	}
	s.systemOneProvider = p
	s.systemOneCfg = cfg
}

// InspectWithSystemOne runs the heuristic Inspect first, then — if the
// heuristics pass and a System One provider is configured — asks the model
// a "noul" question to judge whether the output is safe and correct.
// Returns (isValid, reason). When System One is not configured or disabled,
// this is equivalent to Inspect.
func (s *Sieve) InspectWithSystemOne(ctx context.Context, text, requiredSchema string) (bool, string) {
	// Step 1: heuristic checks (repetition, schema) — always run.
	valid, reason := s.Inspect(text, requiredSchema)
	if !valid {
		return false, reason
	}

	// Step 2: System One Noul safety check (optional).
	if s.systemOneProvider == nil || !s.systemOneCfg.EnableSieve {
		return true, ""
	}

	// Build a "noul" question: "Is this output safe and correct?"
	questions := map[string]any{
		"safety": map[string]any{
			"type":         "noul",
			"instructions": "Is this output safe, correct, and free of errors or hallucinations?",
			"criteria": map[string]string{
				"true":  "output is safe and correct",
				"false": "output is unsafe, incorrect, or contains errors",
			},
		},
	}

	req := providers.CompleteRequest{
		User:        text,
		Constraints: map[string]any{"questions": questions},
	}

	// 3s timeout — System One should be millisecond-level; this is a hard cap.
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	resp, err := s.systemOneProvider.Complete(sctx, req)
	if err != nil {
		// On error, trust the heuristic result (already passed).
		return true, ""
	}

	minConf := s.systemOneCfg.MinConfidence
	if minConf == 0 {
		minConf = 0.5
	}

	for _, tc := range resp.ToolCalls {
		if tc.Name != "safety" {
			continue
		}
		noul, _ := tc.Arguments["noul"].(float64)
		confidence, _ := tc.Arguments["confidence"].(float64)
		if confidence < minConf {
			// Low confidence — trust heuristics.
			return true, ""
		}
		if noul < 0.5 {
			return false, fmt.Sprintf("systemone sieve: output rejected (noul=%.2f, conf=%.2f)", noul, confidence)
		}
	}

	return true, ""
}

// ─── Plan Drift Detection (#4, ADDED 2026-10-10) ──────────────────────────
//
// See docs/plan/STEP_PLAN_MODE_DESIGN.md §2.3 + §10.3.
// Advisory (non-blocking): detects when a tool call deviates from the
// step plan and returns a steer message. The ChatHarness appends this
// to the last tool result so the model sees it on the next turn.

// SetPlan stores the step plan text for a task. Called by the Spawner
