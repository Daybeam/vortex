package core

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// GenerateStateSignature extracts a discrete environment state identifier
// from a tool name and its result. ADDED (2026-09-08) for PGPO.
func (s *Spawner) GenerateStateSignature(toolName string, result any) string {
	// 1. Extract observation signature (e.g. status code, key element presence)
	observation := "default"

	if resMap, ok := result.(map[string]any); ok {
		// Look for common status indicators
		if status, ok := resMap["status"].(string); ok {
			observation = status
		} else if exitCode, ok := resMap["exit_code"].(float64); ok {
			observation = fmt.Sprintf("exit_%d", int(exitCode))
		}

		// DOM state signature (simplified)
		if strings.Contains(toolName, "browser") {
			if _, exists := resMap["error"]; exists {
				observation = "browser_error"
			}
		}
	}

	// 2. Hash to form discrete state identifier s
	data := fmt.Sprintf("%s:%s", toolName, observation)
	hash := sha256.Sum256([]byte(data))

	return fmt.Sprintf("%s:%x", toolName, hash[:8])
}

// EvaluatePotentialDrop calculates the potential gradient between two states.
// Returns true if a sharp drop is detected (Potential Drop Alert).
func (s *Spawner) EvaluatePotentialDrop(previousState, currentState string) (bool, float64) {
	if s.expStore == nil || previousState == "" {
		return false, 0
	}

	v1 := s.expStore.GetStatePotential(previousState)
	v2 := s.expStore.GetStatePotential(currentState)

	delta := v2 - v1

	// Threshold from specification: -0.4
	if delta < -0.4 {
		return true, delta
	}

	return false, delta
}

// TriggerProactiveIntervention handles the Potential Drop Alert by injecting
// warnings or proven recovery strategies into the next turn's prompt.
func (s *Spawner) TriggerProactiveIntervention(currentState string, intent string) string {
	if s.expStore == nil {
		return ""
	}

	// Query AntiPatternStore using the current state and intent
	// The AntiPatternStore.QueryByKeyword uses simple string matching.
	aps := s.expStore.QueryRelevantAntiPatterns(currentState+" "+intent, 1)
	if len(aps) > 0 {
		ap := aps[0]
		return fmt.Sprintf("\n\n[PROACTIVE INTERVENTION]: Your current trajectory entering state %s has a high risk of failure based on historical data.\nWARNING: %s\nPROVEN RECOVERY: %s",
			currentState, ap.AntiPattern, ap.CorrectPattern)
	}

	return fmt.Sprintf("\n\n[PROACTIVE WARNING]: Your current trajectory entering state %s has shown a significant drop in success potential. Proceed with caution and verify all assumptions.", currentState)
}
