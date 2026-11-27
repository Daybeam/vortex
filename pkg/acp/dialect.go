package acp

import (
	"encoding/json"
	"strings"
)

type dialectDecision struct {
	dialect string
	layer   string
	signal  string
}

func resolveDialectMode(env, configured string) (mode, source string) {
	mode = normalizeDialect(env)
	if strings.TrimSpace(env) != "" {
		if mode == "" {
			mode = "auto"
		}
		return mode, "env"
	}
	mode = normalizeDialect(configured)
	if mode == "" {
		mode = "auto"
	}
	return mode, "config"
}

func normalizeDialect(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "standard", "legacy", "auto":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func detectDialect(params json.RawMessage) (dialectDecision, bool) {
	var fields map[string]json.RawMessage
	if len(params) == 0 || json.Unmarshal(params, &fields) != nil || fields == nil {
		return dialectDecision{}, false
	}
	if _, ok := fields["protocolVersion"]; ok {
		return dialectDecision{"standard", "L1", "protocolVersion"}, true
	}
	for _, key := range []string{"clientCapabilities", "capabilities"} {
		if raw, ok := fields[key]; ok && isJSONObject(raw) {
			return dialectDecision{"standard", "L2", key}, true
		}
	}
	if raw, ok := fields["clientInfo"]; ok && strings.TrimSpace(string(raw)) != "null" && strings.TrimSpace(string(raw)) != "" {
		return dialectDecision{"standard", "L2", "clientInfo"}, true
	}
	return dialectDecision{"legacy", "L2", "no_standard_initialize_fields"}, true
}

func isJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func detectDialectMethod(method string) (dialectDecision, bool) {
	switch method {
	case "session/new", "session/prompt", "session/cancel", "session/update":
		return dialectDecision{"standard", "L3", method}, true
	case "ping":
		return dialectDecision{}, false
	default:
		return dialectDecision{"legacy", "L3", method}, true
	}
}
