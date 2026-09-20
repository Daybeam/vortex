package core

import (
	"fmt"
	"sort"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// EventSOPCandidatesOffered records that one or more SOPs were surfaced as
// candidates for a task. It intentionally does NOT record whether the LLM
// went on to follow the SOP -- that can only be inferred later (or not at
// all) from the actual steps the model chooses to submit. This event exists
// so that low real-world adoption of a given SOP is visible in the logs
// rather than silently invisible, which is the main tool for diagnosing
// "this SOP's keywords are too narrow / too specific" over time.
const EventSOPCandidatesOffered EventType = "sop_candidates_offered"

// SOPCandidate pairs a matched SOP with its raw keyword-hit count, used only
// for ranking candidates before truncating to maxCandidates. The hit count
// is NOT a confidence score and must not be treated as one -- it only
// reflects how many distinct trigger keywords appeared in the task text.
type SOPCandidate struct {
	SOP  *schemas.SOP
	Hits int
}

// MatchSOPCandidates performs a deliberately permissive first-pass recall
// over all registered SOPs: any SOP with at least one whole-word keyword hit
// in taskText is included, ranked by hit count, and truncated to maxResults.
//
// DESIGN NOTE: this function does not, and must not, decide whether a SOP
// actually applies to the task. Keyword matching over natural-language task
// descriptions cannot reliably distinguish "this SOP is relevant" from "this
// SOP's keywords happen to appear for an unrelated reason" -- the tool_router
// regressions (see core/tool_router.go's FIX comments, 2026-06-27/07-01)
// demonstrated this same class of problem for tool-name relevance and took
// three attempts to stabilize even in the much narrower domain of matching
// single tool names against a fixed affinity table. Task-description-to-SOP
// matching is a strictly harder, more open-ended problem, so this function
// intentionally does NOT try to be the final arbiter. Instead it produces a
// short, ranked candidate list; the caller is expected to surface these
// candidates to the LLM actually planning the task (as reference material,
// not a mandatory instruction) and let semantic understanding make the final
// call. See tools/tools.go's use of this function in orchestrator_submit_task.
//
// containsWholeWord (defined in tool_router.go) is reused deliberately: it
// already solves the "gitnexus should not match git" class of false-positive
// that a naive strings.Contains would reintroduce here.
func MatchSOPCandidates(reg *config.Registry, taskText string, maxResults int) []SOPCandidate {
	if reg == nil || taskText == "" || maxResults <= 0 {
		return nil
	}

	taskLower := strings.ToLower(taskText)
	// Optimization (2026-08-06): for SOPs with thousands of keywords (stress tests),
	// iterating the keyword list and calling containsWholeWord O(N) is too slow.
	// Tokenize the task text once into a map for O(1) single-word lookups.
	taskWords := make(map[string]bool)
	for _, word := range strings.FieldsFunc(taskLower, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		taskWords[word] = true
	}

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	var scored []SOPCandidate
	for _, sop := range reg.SOPs {
		if sop == nil {
			continue
		}
		hits := 0
		for _, trigger := range sop.Triggers {
			for _, kw := range trigger.Keywords {
				kw = strings.TrimSpace(kw)
				if kw == "" {
					continue
				}
				kwLower := strings.ToLower(kw)

				// Optimized match: if it's a single word, use the map.
				// Otherwise fall back to the slower whole-word substring match.
				isSingleWord := true
				for _, r := range kwLower {
					if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
						isSingleWord = false
						break
					}
				}

				if isSingleWord {
					if taskWords[kwLower] {
						hits++
					}
				} else {
					if containsWholeWord(taskLower, kwLower) {
						hits++
					}
				}
			}
		}
		if hits > 0 {
			scored = append(scored, SOPCandidate{SOP: sop, Hits: hits})
		}
	}

	// Stable sort by hit count descending; ties keep map-iteration order,
	// which is fine since ties are broken arbitrarily either way and we
	// don't want to introduce a false sense of precision by tie-breaking
	// on some other heuristic.
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Hits > scored[j].Hits
	})

	if len(scored) > maxResults {
		scored = scored[:maxResults]
	}
	return scored
}

// FormatSOPHints renders a short, explicitly-non-mandatory reference block
// summarizing the given candidates, meant to be appended to a step's task
// text before it reaches the planning LLM. The wording is deliberately
// hedged ("may be relevant", "if applicable", "use your own judgment") so
// that a model does not treat this as a forced instruction to follow a SOP
// that turns out not to fit -- the LLM remains the actual decision-maker
// (see MatchSOPCandidates doc comment for why).
func FormatSOPHints(candidates []SOPCandidate) string {
	if len(candidates) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n[Reference: the following previously-documented procedures may be relevant to this task. ")
	b.WriteString("They are suggestions, not requirements -- use your own judgment about whether any of them ")
	b.WriteString("actually fits what you've been asked to do, and ignore any that don't.]\n")

	for _, c := range candidates {
		b.WriteString(fmt.Sprintf("- SOP %q (v%s): %s\n", c.SOP.ID, c.SOP.Version, c.SOP.Description))
	}

	return b.String()
}
