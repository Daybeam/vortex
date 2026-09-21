package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// SlopVerdict is the classification a single subagent response can receive
// from the Anti-Slop quality guard.
type SlopVerdict string

const (
	// VerdictClean means the response is specific enough — pass through.
	VerdictClean SlopVerdict = "clean"
	// VerdictWarn means slop phrases were detected but strict mode is off
	// — response is returned but a warning is appended.
	VerdictWarn SlopVerdict = "warn"
	// VerdictBlock means strict mode is on and min_hits exceeded — the
	// response is rejected outright.
	VerdictBlock SlopVerdict = "block"
)

// AntiSlopInterceptor detects filler/generic LLM phrasing in subagent
// output and either warns or blocks the response depending on config.
// ADDED (2026-08-27).
type AntiSlopInterceptor struct {
	registry *config.Registry
	logger   *Logger
}

// NewAntiSlopInterceptor creates a new interceptor.
func NewAntiSlopInterceptor(reg *config.Registry, logger *Logger) *AntiSlopInterceptor {
	return &AntiSlopInterceptor{registry: reg, logger: logger}
}

// Wrap returns an Interceptor that plugs into the Spawner chain.
func (i *AntiSlopInterceptor) Wrap(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
	cfg := i.registry.System.AntiSlop

	// Fast-path: not enabled
	if !cfg.Enabled {
		return next(ctx, req)
	}

	result, err := next(ctx, req)
	if err != nil || result == nil {
		return result, err
	}

	verdict, hits := i.ScanOutput(result.Output)

	switch verdict {
	case VerdictBlock:
		err := fmt.Errorf("anti_slop_block: %d filler phrase(s) detected in subagent output (threshold=%d)", hits, cfg.MinHits)
		i.logger.Log("EventSlopBlocked", req.TaskID, req.StepID, map[string]any{
			"hits":     hits,
			"min_hits": cfg.MinHits,
			"strict":   cfg.Strict,
		})
		return result, err

	case VerdictWarn:
		if result.Output.Warnings == nil {
			result.Output.Warnings = []string{}
		}
		warn := fmt.Sprintf("⚠️ Anti-Slop: %d filler phrase(s) detected — response may be generic", hits)
		result.Output.Warnings = append(result.Output.Warnings, warn)
		i.logger.Log("EventSlopWarned", req.TaskID, req.StepID, map[string]any{
			"hits":     hits,
			"min_hits": cfg.MinHits,
		})
	}

	return result, nil
}

// ScanOutput recursively walks a SubagentOutput, counts slop hits, and
// returns the verdict.
func (i *AntiSlopInterceptor) ScanOutput(output schemas.SubagentOutput) (SlopVerdict, int) {
	cfg := i.registry.System.AntiSlop
	minHits := cfg.MinHits
	if minHits <= 0 {
		minHits = 3 // sensible default
	}

	pendingPhrases := cfg.Phrases
	if len(pendingPhrases) == 0 {
		pendingPhrases = defaultSlopPhrases
	}

	totalHits := 0

	// 1. Scan the result map recursively for string values
	if output.Result != nil {
		totalHits += i.scanMap(output.Result, pendingPhrases)
	}

	// 2. Scan attachments labels (Attachment has no Text field)
	for _, a := range output.Attachments {
		totalHits += i.scanText(a.Label, pendingPhrases)
	}

	// 3. Scan capability string
	if output.Capability != "" {
		totalHits += i.scanText(output.Capability, pendingPhrases)
	}

	// 4. Scan assumptions & warnings (these ARE the output, not meta)
	totalHits += i.scanStrings(output.Assumptions, pendingPhrases)
	totalHits += i.scanStrings(output.Warnings, pendingPhrases)
	totalHits += i.scanStrings(output.MissingContext, pendingPhrases)

	if totalHits >= minHits {
		if cfg.Strict {
			return VerdictBlock, totalHits
		}
		return VerdictWarn, totalHits
	}
	return VerdictClean, totalHits
}

// scanMap recurses into arbitrary JSON-decoded values looking for strings.
func (i *AntiSlopInterceptor) scanMap(m map[string]any, phrases []string) int {
	total := 0
	for _, v := range m {
		total += i.scanValue(v, phrases)
	}
	return total
}

func (i *AntiSlopInterceptor) scanValue(v any, phrases []string) int {
	switch val := v.(type) {
	case string:
		return i.scanText(val, phrases)
	case map[string]any:
		return i.scanMap(val, phrases)
	case []any:
		total := 0
		for _, item := range val {
			total += i.scanValue(item, phrases)
		}
		return total
	case []string:
		return i.scanStrings(val, phrases)
	default:
		return 0
	}
}

func (i *AntiSlopInterceptor) scanStrings(items []string, phrases []string) int {
	total := 0
	for _, s := range items {
		total += i.scanText(s, phrases)
	}
	return total
}

func (i *AntiSlopInterceptor) scanText(text string, phrases []string) int {
	lower := strings.ToLower(text)
	total := 0
	for _, phrase := range phrases {
		p := strings.ToLower(phrase)
		if strings.Contains(lower, p) {
			total++
		}
	}
	return total
}

// defaultSlopPhrases is the built-in set of common LLM filler phrases.
// Users can override/extend via config SystemSettings.AntiSlop.Phrases.
var defaultSlopPhrases = []string{
	"it's important to note",
	"it's worth mentioning",
	"as an AI",
	"as a language model",
	"in conclusion",
	"moreover",
	"nevertheless",
	"furthermore",
	"it should be noted",
	"at the end of the day",
	"in this modern world",
	"in today's fast-paced",
	"it is worth noting",
	"it is important to remember",
	"it is important to understand",
	"as previously mentioned",
	"to be honest",
	"frankly speaking",
	"in all likelihood",
	"without a doubt",
	"undoubtedly",
	"needless to say",
	"it goes without saying",
	"it bears mentioning",
	"it stands to reason",
	"one could argue",
	"it could be argued",
	"it is interesting to note",
	"at this juncture",
	"in the grand scheme of things",
}
