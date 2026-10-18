package gateway

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
)

// tokenPattern matches complete [REDACTED_N] tokens (P3: unmatched detection).
var tokenPattern = regexp.MustCompile(`\[REDACTED_\d+\]`)

// secretRegexes are the patterns reused from core/scrubber.go.
// Matched values are replaced with deterministic session-scoped tokens.
var secretRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),      // OpenAI/Anthropic style
	regexp.MustCompile(`(?i)(AIza[a-zA-Z0-9_\-]{35})`), // Google style
	// Unified with egress_redaction.go defaultEgressPatterns (gap-1 fix, 2026-10-18).
	// These 6 patterns were previously only applied to tool results (RedactEgress)
	// but NOT to prompt egress (SanitizingProvider). A third-party relay model sees
	// unmasked prompts — AWS keys, JWTs, emails, etc. in system prompts / memory
	// bank leaked. Now masked bidirectionally like the other secrets.
	regexp.MustCompile(`(AKIA[0-9A-Z]{16})`),                                              // AWS access key
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), // PEM private key
	regexp.MustCompile(`(Bearer\s+[A-Za-z0-9._\-]{20,})`),                                 // Bearer token
	regexp.MustCompile(`(eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,})`), // JWT
	regexp.MustCompile(`([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`),               // email
	regexp.MustCompile(`(1[3-9]\d{9})`),                                                   // CN phone
	// key=value generic catch-all stays last (after specific patterns).
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
		if tokenPattern.MatchString(maskedInput) {
			log.Printf("WARN: SanitizerProxy.Unmask: no vault for taskID %q but input contains %d token(s); vault may have been cleared prematurely", taskID, len(tokenPattern.FindAllString(maskedInput, -1)))
		}
		return maskedInput
	}

	result := maskedInput
	for token, value := range vault.tokenToValue {
		result = strings.ReplaceAll(result, token, value)
	}

	if remaining := tokenPattern.FindAllString(result, -1); len(remaining) > 0 {
		log.Printf("WARN: SanitizerProxy.Unmask: %d unmatched token(s) remain after unmask for taskID %q: %v", len(remaining), taskID, remaining)
	}
	return result
}

// Clear removes the session vault for the given taskID. Call this on task
// completion to prevent unbounded memory growth and enforce ephemeral scope.
func (s *SanitizerProxy) Clear(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vaults, taskID)
}
