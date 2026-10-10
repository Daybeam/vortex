package core

import (
	"fmt"
	"strings"
)

// Healer provides universal, domain-agnostic self-healing advice for common tool failures.
// It implements Mechanism 2 of the Technical Specification for Control Flow.
type Healer struct{}

func NewHealer() *Healer {
	return &Healer{}
}

// Diagnose captures common error patterns from tool outputs and generates
// actionable [SYSTEM SELF-HEALING ADVICE].
func (h *Healer) Diagnose(err string) string {
	if err == "" {
		return ""
	}

	lower := strings.ToLower(err)
	advice := ""

	switch {
	case strings.Contains(lower, "element not interactable") || strings.Contains(lower, "not visible"):
		advice = HealerAdviceElementNotInteractable
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		advice = HealerAdviceTimeout
	case strings.Contains(lower, "selector not found") || strings.Contains(lower, "no such element"):
		advice = HealerAdviceSelectorNotFound
	case strings.Contains(lower, "permission denied") || strings.Contains(lower, "unauthorized"):
		advice = HealerAdvicePermissionDenied
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "429"):
		advice = HealerAdviceRateLimit
	case strings.Contains(lower, "context window") || strings.Contains(lower, "too many tokens"):
		advice = HealerAdviceContextWindow
	}

	if advice != "" {
		return fmt.Sprintf("\n\n%s\n%s", MarkerSystemSelfHealing, advice)
	}

	return ""
}
