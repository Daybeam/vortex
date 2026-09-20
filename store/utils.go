package store

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"regexp"
	"time"
)

// StepRecord represents a single step's outcome for learning.
type StepRecord struct {
	StepID         string            `json:"step_id"`
	RoleID         string            `json:"role_id"`
	ModelID        string            `json:"model_id"` // 记录实际使用的模型 ID
	Skills         []string          `json:"skills"`
	Capability     string            `json:"capability"`
	Confidence     float64           `json:"confidence"`
	Status         string            `json:"status"` // "ok", "partial", "failed", "skipped"
	RetryCount     int               `json:"retry_count"`
	MissingContext []string          `json:"missing_context"`
	TaskType       string            `json:"task_type"`
	Task           string            `json:"task"`
	Timestamp      time.Time         `json:"timestamp"`
	Trace          []ToolInteraction `json:"trace,omitempty"`
	LastError      string            `json:"last_error,omitempty"`     // Triggering signal (ADDED 2026-08-16)
	FailureMode    string            `json:"failure_mode,omitempty"`   // FMC: Structured label (ADDED 2026-09-06)
	StatesVisited  []string          `json:"states_visited,omitempty"` // PGPO: Visited environment states (ADDED 2026-09-08)

	// Perception 2.0
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`

	// Metabolic cost (ADDED 2026-09-13) — see docs/METABOLIC_PRUNING_DESIGN.md
	TokenUsed int     `json:"token_used,omitempty"`
	LatencyMs float64 `json:"latency_ms,omitempty"`
}

func deriveTaskType(records []StepRecord) string {
	caps := make([]string, 0, len(records))
	for _, r := range records {
		if r.Status != "skipped" {
			caps = append(caps, r.Capability)
		}
	}
	return joinStrings(caps, "+")
}

func joinStrings(s []string, sep string) string {
	result := ""
	for i, v := range s {
		if i > 0 {
			result += sep
		}
		result += v
	}
	return result
}

func toSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

func intersectionSize(a, b map[string]bool) int {
	count := 0
	for k := range a {
		if b[k] {
			count++
		}
	}
	return count
}

func unionSize(a, b map[string]bool) int {
	m := make(map[string]bool)
	for k := range a {
		m[k] = true
	}
	for k := range b {
		m[k] = true
	}
	return len(m)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsCombo(combos [][]string, target []string) bool {
	for _, combo := range combos {
		if sliceEqual(combo, target) {
			return true
		}
	}
	return false
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedCopy(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	for i := 0; i < len(c)-1; i++ {
		for j := i + 1; j < len(c); j++ {
			if c[j] < c[i] {
				c[i], c[j] = c[j], c[i]
			}
		}
	}
	return c
}

func round2(f float64) float64 {
	return math.Round(f*1000) / 1000
}

// sanitizeErrorRe masks API keys in error strings — compiled once (audit: was recompiled per call).
var sanitizeErrorRe = regexp.MustCompile(`(?i)(key|api_key|token)=[^&\s"']+`)

func SanitizeError(err string) string {
	if err == "" {
		return ""
	}
	// Mask API keys in URLs (e.g., ?key=AIza... or &api_key=...)
	return sanitizeErrorRe.ReplaceAllString(err, "$1=***")
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func writeJSON(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("WARN: writeJSON: failed to marshal %s: %v", path, err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("WARN: writeJSON: failed to write %s: %v", path, err)
	}
}

// writeBytes writes pre-marshaled JSON bytes to disk. Used by PersistAll to
// split marshaling (under lock) from disk I/O (outside lock).
func writeBytes(path string, data []byte) {
	if len(data) == 0 {
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("WARN: writeBytes: failed to write %s: %v", path, err)
	}
}

func readJSON(path string, v any) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, v); err != nil {
		log.Printf("WARN: readJSON: failed to unmarshal %s: %v", path, err)
	}
}
