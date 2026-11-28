package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// RefinementWorklogEntry records one round of the render-then-review
// refinement loop. Appended per round to refinement_worklog.json in the
// task workdir. The SOP prompt for round N+1 injects rounds 1..N
// (bounded — last 3 full, earlier summarized to outcome lines only).
type RefinementWorklogEntry struct {
	Round        int                    `json:"round"`
	Plan         string                 `json:"plan"`
	ScoresBefore map[string]float64     `json:"scores_before"` // {"overall": 6.0, "axes": {"layout": 5}}
	ScoresAfter  map[string]float64     `json:"scores_after"`
	Deltas       map[string]float64     `json:"deltas"`
	Outcome      string                 `json:"outcome"`
}

// BuildWorklogEntry constructs a worklog entry from the round number,
// refiner's plan, before/after scores, and the gate verdict. Deltas are
// computed over axes present in both before and after (matching
// GateRefinement's axisDeltas semantics). The "overall" key is included
// in deltas.
func BuildWorklogEntry(round int, plan string, prev, next ReviewScores, verdict RefinementVerdict) RefinementWorklogEntry {
	deltas := make(map[string]float64)
	deltas["overall"] = next.Overall - prev.Overall
	for k, pv := range prev.Axes {
		nv, ok := next.Axes[k]
		if ok {
			deltas[k] = nv - pv
		}
	}

	return RefinementWorklogEntry{
		Round:        round,
		Plan:         plan,
		ScoresBefore: scoresToMap(prev),
		ScoresAfter:  scoresToMap(next),
		Deltas:       deltas,
		Outcome:      verdict.String(),
	}
}

// scoresToMap flattens ReviewScores into a single map with "overall" + axes.
func scoresToMap(s ReviewScores) map[string]float64 {
	m := make(map[string]float64, len(s.Axes)+1)
	m["overall"] = s.Overall
	for k, v := range s.Axes {
		m[k] = v
	}
	return m
}

// AppendWorklog appends a worklog entry to the JSON file at workdir/refinement_worklog.json.
// The file is a JSON array of entries. If the file does not exist, it is created.
// If it exists, the new entry is appended to the existing array.
func AppendWorklog(workdir string, entry RefinementWorklogEntry) error {
	path := filepath.Join(workdir, "refinement_worklog.json")

	var entries []RefinementWorklogEntry
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &entries); err != nil {
			return fmt.Errorf("refinement_worklog: parse existing: %w", err)
		}
	}

	entries = append(entries, entry)

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("refinement_worklog: marshal: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("refinement_worklog: write: %w", err)
	}
	return nil
}

// SummarizeWorklog produces a compact text summary of the worklog for
// prompt injection in round N+1. The last `recentN` entries are shown
// in full; earlier entries are summarized to one line each (round + outcome).
// Axes in deltas are sorted alphabetically for deterministic output.
func SummarizeWorklog(entries []RefinementWorklogEntry, recentN int) string {
	if len(entries) == 0 {
		return ""
	}
	if recentN < 0 {
		recentN = 3
	}

	var full, summarized []RefinementWorklogEntry
	split := len(entries) - recentN
	if split < 0 {
		split = 0
	}
	summarized = entries[:split]
	full = entries[split:]

	var out string
	if len(summarized) > 0 {
		out += "Earlier rounds (summary):\n"
		for _, e := range summarized {
			out += fmt.Sprintf("  Round %d: %s\n", e.Round, e.Outcome)
		}
	}
	if len(full) > 0 {
		if len(summarized) > 0 {
			out += "\n"
		}
		out += "Recent rounds (full):\n"
		for _, e := range full {
			out += formatWorklogEntry(e)
		}
	}
	return out
}

func formatWorklogEntry(e RefinementWorklogEntry) string {
	// Sort delta keys for deterministic output.
	keys := make([]string, 0, len(e.Deltas))
	for k := range e.Deltas {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	deltaParts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := e.Deltas[k]
		sign := "+"
		if v < 0 {
			sign = ""
		}
		deltaParts = append(deltaParts, fmt.Sprintf("%s=%s%.1f", k, sign, v))
	}

	return fmt.Sprintf("  Round %d: outcome=%s, deltas[%s]\n",
		e.Round, e.Outcome, joinStrings(deltaParts, ", "))
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}
