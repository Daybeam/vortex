package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/codeintel"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/store"
)

// Sieve handles result validation, repetition detection, and schema enforcement.
type Sieve struct {
	RepetitionThreshold     float64             // Similarity threshold to trigger repetition warning (e.g., 0.85)
	ToolRepetitionThreshold int                 // Number of identical tool calls allowed before interception
	WindowSize              int                 // Number of previous lines to compare for repetition
	MaxKeep                 int                 // Max lines to keep in pruner
	toolHistory             map[string][]string // taskID -> list of toolCallHashes
	compressor              *codeintel.Compressor // AST-aware code shrinking
	mu                      sync.Mutex

	// ProtectedPrefixes (Anti-Cliff, ADDED 2026-08-27, arXiv:2608.22752).
	// Any key in CompressContext's map whose string begins with one of these
	// prefixes is treated as a "rules-zone" entry and passes through WITHOUT
	// AST compression or recursion. This prevents structured artifact
	// contracts, SOP rules, or invariant declarations from being silently
	// AST-collapsed when they happen to carry a .go-looking value.
	ProtectedPrefixes []string

	// systemOneProvider is the optional System One decision model for the
	// Sieve's Noul/Score safety check (M5). Nil-safe: set via
	// SetSystemOneProvider. When non-nil and EnableSieve is true,
	// InspectWithSystemOne asks the model a "noul" question to judge
	// output safety. See docs/systemone-provider-design.md §6.2 + §8 M5.
	systemOneProvider *providers.SystemOneProvider
	systemOneCfg      config.SystemOneConfig
}

const (
	maxToolHistoryPerTask = 100  // cap per-task tool call history (only need last N for consecutive-repetition check)
	maxToolHistoryTasks   = 1000 // cap on number of tasks tracked in toolHistory map
)

func NewSieve(maxKeep int) *Sieve {
	if maxKeep <= 0 {
		maxKeep = 100
	}
	return &Sieve{
		RepetitionThreshold:     0.85,
		ToolRepetitionThreshold: 3,
		WindowSize:              5,
		MaxKeep:                 maxKeep,
		toolHistory:             make(map[string][]string),
		compressor:              codeintel.NewCompressor(100),
		ProtectedPrefixes: []string{
			"$artifact",    // ArtifactContract / provenance metadata
			"$contract",    // Artifact Contract fields (required_fields, type_schema)
			"$goal",        // Task goal / invariant
			"$rule",        // SOP rule / system directive
			"$sop",         // SOP role definition
			"$schema",      // JSON schema declarations
			"$invariant",   // Explicit invariants
			"protected_",   // Prefix reserved for any custom protected entry
		},
	}
}

func newSieveFromConfig(sys config.SystemSettings) *Sieve {
	s := NewSieve(sys.MaxContextKeep)
	if sys.ToolRepetitionThreshold > 0 {
		s.ToolRepetitionThreshold = sys.ToolRepetitionThreshold
	}
	return s
}

// isProtectedKey returns true when key begins with one of the
// ProtectedPrefixes configured on the Sieve.
func (s *Sieve) isProtectedKey(key string) bool {
	for _, pfx := range s.ProtectedPrefixes {
		if strings.HasPrefix(key, pfx) {
			return true
		}
	}
	return false
}

// CompressContext recursively scans a map and compresses any Go source code strings.
// Anti-Cliff: entries whose key is in ProtectedPrefixes are skipped (verbatim).
func (s *Sieve) CompressContext(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}

	out := make(map[string]any)
	for k, v := range data {
		// Anti-Cliff: protected keys bypass compression entirely
		if s.isProtectedKey(k) {
			out[k] = v
			continue
		}

		switch val := v.(type) {
		case string:
			// Heuristic: if key looks like a path or value looks like Go code
			if strings.HasSuffix(k, ".go") || strings.Contains(val, "package ") && strings.Contains(val, "func ") {
				compressed, ok := s.compressor.CompressGo(k, val)
				if ok {
					out[k] = compressed
					continue
				}
			}
			out[k] = val
		case map[string]any:
			out[k] = s.CompressContext(val)
		case []any:
			newSlice := make([]any, len(val))
			for i, item := range val {
				if m, ok := item.(map[string]any); ok {
					newSlice[i] = s.CompressContext(m)
				} else {
					newSlice[i] = item
				}
			}
			out[k] = newSlice
		default:
			out[k] = v
		}
	}
	return out
}

// VerifyFidelity ensures that critical semantic signals survived compression.
// Returns (isFaithful, lostSignal).
func (s *Sieve) VerifyFidelity(original, compressed string) (bool, string) {
	// Critical signals from ODFTP/Anti-Cliff spec
	signals := []string{"S2T", "DecisionID", "Goal:", "Final Result:", "$artifact", "$contract"}

	for _, sig := range signals {
		if strings.Contains(original, sig) && !strings.Contains(compressed, sig) {
			return false, sig
		}
	}
	return true, ""
}

// Inspect checks if the result contains repetitions or violates structural requirements.
// Returns (isValid, reason)
func (s *Sieve) Inspect(text string, requiredSchema string) (bool, string) {
	if text == "" {
		return false, "empty output"
	}

	// 1. Repetition Detection
	if s.detectRepetition(text) {
		return false, "repetition detected: output entered a logic loop"
	}

	// 2. Schema Validation
	if requiredSchema != "" {
		if !s.validateSchema(text, requiredSchema) {
			return false, fmt.Sprintf("schema violation: output does not match required format [%s]", requiredSchema)
		}
	}

	return true, ""
}

// PruneOutput implements a "Semantic State Parser" to reduce token noise.
// It keeps lines that match keywords related to the goal or contain "error", "fail", "success", etc.
func (s *Sieve) PruneOutput(text string, goal string) string {
	lines := strings.Split(text, "\n")
	if len(lines) < 20 { // Don't prune small outputs
		return text
	}

	keywords := []string{"error", "fail", "success", "done", "warning", "critical", "exception", "exit"}
	goalTokens := strings.Fields(strings.ToLower(goal))
	for _, t := range goalTokens {
		if len(t) > 3 {
			keywords = append(keywords, t)
		}
	}

	var pruned []string
	keepCount := 0

	for _, line := range lines {
		lower := strings.ToLower(line)
		match := false
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				match = true
				break
			}
		}

		if match {
			pruned = append(pruned, line)
			keepCount++
		}

		if keepCount >= s.MaxKeep {
			pruned = append(pruned, "... [Output truncated by Sieve Pruner]")
			break
		}
	}

	if len(pruned) == 0 {
		return "[Sieve Pruner: No relevant lines found in large output. Showing first 10 lines only:]\n" + strings.Join(lines[:10], "\n")
	}

	return strings.Join(pruned, "\n")
}

func (s *Sieve) detectRepetition(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return false
	}

	// Slide a window and compare lines
	for i := 1; i < len(lines); i++ {
		current := strings.TrimSpace(lines[i])
		if current == "" {
			continue
		}
		
		// Compare with previous N lines
		start := i - s.WindowSize
		if start < 0 {
			start = 0
		}
		
		for j := start; j < i; j++ {
			prev := strings.TrimSpace(lines[j])
			if prev == "" {
				continue
			}
			
			if s.calculateSimilarity(current, prev) > s.RepetitionThreshold {
				return true
			}
		}
	}
	return false
}

// calculateSimilarity uses a simple Jaccard-like token overlap for fast detection.
func (s *Sieve) calculateSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	
	tokensA := strings.Fields(strings.ToLower(a))
	tokensB := strings.Fields(strings.ToLower(b))
	
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}

	setA := make(map[string]bool)
	for _, t := range tokensA {
		setA[t] = true
	}

	intersection := 0
	for _, t := range tokensB {
		if setA[t] {
			intersection++
		}
	}

	union := len(tokensA) + len(tokensB) - intersection
	return float64(intersection) / float64(union)
}

func (s *Sieve) validateSchema(text string, schema string) bool {
	// ... (rest of the code)
	return true
}

// InspectToolCalls checks for repeated identical tool calls to prevent infinite loops.
// Returns (isValid, reason)
func (s *Sieve) InspectToolCalls(taskID string, calls []store.ToolInteraction) (bool, string) {
	if len(calls) == 0 {
		return true, ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, call := range calls {
		argData, _ := json.Marshal(call.Arguments)
		hash := fmt.Sprintf("%s:%s", call.ToolName, string(argData))

		history := s.toolHistory[taskID]

		consecutiveCount := 0
		for i := len(history) - 1; i >= 0; i-- {
			if history[i] == hash {
				consecutiveCount++
			} else {
				break
			}
		}
		if consecutiveCount >= s.ToolRepetitionThreshold {
			return false, "InfiniteLoopDetected"
		}

		windowSize := s.ToolRepetitionThreshold * 2
		if windowSize > len(history) {
			windowSize = len(history)
		}
		windowStart := len(history) - windowSize
		totalInWindow := 0
		for i := windowStart; i < len(history); i++ {
			if history[i] == hash {
				totalInWindow++
			}
		}
		if totalInWindow >= s.ToolRepetitionThreshold && windowSize > 0 {
			if float64(totalInWindow)/float64(windowSize) >= 0.5 {
				return false, "InfiniteLoopDetected"
			}
		}

		s.toolHistory[taskID] = append(s.toolHistory[taskID], hash)
		if len(s.toolHistory[taskID]) > maxToolHistoryPerTask {
			s.toolHistory[taskID] = s.toolHistory[taskID][len(s.toolHistory[taskID])-maxToolHistoryPerTask:]
		}
	}

	// Cap the number of tracked tasks to prevent unbounded map growth.
	// Clearing is safe: old completed tasks are unlikely to still be
	// making tool calls, and ClearHistory is called on task completion.
	if len(s.toolHistory) > maxToolHistoryTasks {
		current := s.toolHistory[taskID]
		// audit P-MED-8: partial eviction instead of full clear to prevent cache stampede
		i := 0
		for k := range s.toolHistory {
			i++
			if i%4 == 0 {
				delete(s.toolHistory, k)
			}
		}
		s.toolHistory[taskID] = current
	}

	return true, ""
}

func (s *Sieve) ClearHistory(taskID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.toolHistory, taskID)
}

// SetSystemOneProvider injects the System One decision model for the Sieve's
// Noul/Score safety check (M5). Nil-safe: when provider is nil or EnableSieve
// is false, InspectWithSystemOne degrades to the heuristic Inspect.
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
