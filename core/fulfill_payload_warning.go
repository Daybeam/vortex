package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/pkg/jsonrepair"
	"github.com/daybeam/vortex/schemas"
)

// fulfillPayloadMismatch only inspects the returned payload. O53 deliberately
// excludes file:, deterministic checks, artifact contracts and model audits:
// those run against the engine's environment, not the delegate's.
func fulfillPayloadMismatch(output schemas.SubagentOutput, criterion string) string {
	if criterion == "" || strings.HasPrefix(criterion, "file:") {
		return ""
	}
	content := fmt.Sprintf("%v", output.Result)
	var reason string
	switch {
	case strings.HasPrefix(criterion, "regex:"):
		pattern := strings.TrimPrefix(criterion, "regex:")
		re, err := compileRegexCache(pattern)
		if err != nil {
			reason = "invalid regex criterion"
		} else if !re.MatchString(content) {
			reason = "output did not match regex pattern: " + pattern
		}
	case criterion == "json":
		raw, isModelText := output.Result["raw"].(string)
		if !isModelText && len(output.Result) == 0 {
			reason = "output is not valid JSON"
		} else if isModelText && !json.Valid([]byte(raw)) {
			if repaired, ok := jsonrepair.Repair(raw); !ok || !json.Valid([]byte(repaired)) {
				reason = "output is not valid JSON"
			}
		}
	default:
		if !strings.Contains(strings.ToLower(content), strings.ToLower(criterion)) {
			reason = "output missing required keyword: " + criterion
		}
	}
	if reason == "" {
		return ""
	}
	return fmt.Sprintf("fulfill_payload_mismatch: %s; the fulfillment remains accepted, but this payload-local criterion was not met", reason)
}
