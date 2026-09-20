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
		advice = "The UI element you tried to interact with is hidden or not ready. Try calling 'wait_for_selector' or taking a 'screenshot' to re-verify the page state before retrying."
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		advice = "The operation timed out. This could be due to slow network or heavy page load. Consider increasing the timeout parameter or breaking the task into smaller sub-steps."
	case strings.Contains(lower, "selector not found") || strings.Contains(lower, "no such element"):
		advice = "The CSS selector you provided was not found on the page. Use 'get_page_source' or 'screenshot' to check if the page structure has changed or if you are on the wrong URL."
	case strings.Contains(lower, "permission denied") || strings.Contains(lower, "unauthorized"):
		advice = "You hit a permission barrier. Check if you are correctly logged in or if you need to request elevated access for this specific tool."
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "429"):
		advice = "Rate limit reached. Please pause for a few seconds before retrying, or reduce the frequency of your requests."
	case strings.Contains(lower, "context window") || strings.Contains(lower, "too many tokens"):
		advice = "Context overflow risk. Try summarizing the previous steps or removing redundant logs from your next action."
	}

	if advice != "" {
		return fmt.Sprintf("\n\n[SYSTEM SELF-HEALING ADVICE]\n%s", advice)
	}

	return ""
}
