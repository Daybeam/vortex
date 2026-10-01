package gateway

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// secretRegexes are the patterns reused from core/scrubber.go.
// Matched values are replaced with deterministic session-scoped tokens.
var secretRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),      // OpenAI/Anthropic style
	regexp.MustCompile(`(?i)(AIza[a-zA-Z0-9_\-]{35})`), // Google style
	regexp.MustCompile(`(?i)(api_key|apikey|password|secret|token|credential)["']?\s*[:=]\s*["']?([a-zA-Z0-9_\-\.]{8,})["']?`),
}

// sessionVault holds the bidirectional token mapping for one task session.
type sessionVault struct {
	tokenToValue map[string]string // "[REDACTED_1]" → "sk-abc..."
	valueToToken map[string]string // "sk-abc..."    → "[REDACTED_1]"
	counter      int
}

// SanitizerProxy implements bidirectional PII tokenization (design §3.1).
// Outbound prompts get secrets replaced with deterministic tokens; inbound
// tool calls get tokens restored to real values before local execution.
// Each TaskID gets an isolated, ephemeral vault that auto-expires on Clear.
type SanitizerProxy struct {
	mu       sync.RWMutex
	vaults   map[string]*sessionVault // taskID → vault
}

// NewSanitizerProxy creates a ready-to-use SanitizerProxy.
func NewSanitizerProxy() *SanitizerProxy {
	return &SanitizerProxy{
		vaults: make(map[string]*sessionVault),
	}
}

// Mask replaces sensitive patterns in input with deterministic session tokens
// (e.g. "sk-abc123..." → "[REDACTED_1]"). The same secret within the same
// session always maps to the same token, enabling the external LLM to reason
// about entities consistently. The mapping is stored in the session vault.
func (s *SanitizerProxy) Mask(input string, taskID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	vault, ok := s.vaults[taskID]
	if !ok {
		vault = &sessionVault{
			tokenToValue: make(map[string]string),
			valueToToken: make(map[string]string),
		}
		s.vaults[taskID] = vault
	}

	for _, re := range secretRegexes {
		input = re.ReplaceAllStringFunc(input, func(match string) string {
			// If this exact value was already seen, reuse its token.
			if token, exists := vault.valueToToken[match]; exists {
				return token
			}
			vault.counter++
			token := fmt.Sprintf("[REDACTED_%d]", vault.counter)
			vault.tokenToValue[token] = match
			vault.valueToToken[match] = token
			return token
		})
	}
	return input
}

// Unmask restores real values for tokens in the given input, using the
// session vault. If the taskID has no vault or a token is not found,
// the token is left unchanged (fail-safe: no data fabrication).
func (s *SanitizerProxy) Unmask(maskedInput string, taskID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	vault, ok := s.vaults[taskID]
	if !ok {
		return maskedInput // no vault → nothing to restore
	}
	for token, value := range vault.tokenToValue {
		maskedInput = strings.ReplaceAll(maskedInput, token, value)
	}
	return maskedInput
}

// Clear removes the session vault for the given taskID. Call this on task
// completion to prevent unbounded memory growth and enforce ephemeral scope.
func (s *SanitizerProxy) Clear(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vaults, taskID)
}
