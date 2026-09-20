package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/daybeam/vortex/pkg/jsonrepair"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_audit.go — Phase 3: Semantic audit pipeline.
// Extracted from scheduler_decision.go per GOD_CLASS_REFLECTION_EXECUTION_PLAN.md.
// Contains: verifyExitCriteria and its helpers (runCriticAudit,
// runProposerSynthesize, executeDebateAndAudit, generateDynamicRubrics, joinCaps).

// regexCache caches compiled exit-criteria regexes by pattern to avoid
// recompilation on every audit check (audit P2).
var regexCache sync.Map // map[string]*regexp.Regexp

// compileRegexCache returns a cached compiled regex, compiling on first use.
func compileRegexCache(pattern string) (*regexp.Regexp, error) {
	if v, ok := regexCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCache.Store(pattern, re)
	return re, nil
}

// runCriticAudit runs a single VerifierModel audit pass on the given content.
// Returns (passed, critiqueText, failureType).
func (s *DirectedEngine) runCriticAudit(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, content string) (bool, string, schemas.VerificationFailureType) {
	auditPrompt := fmt.Sprintf(`You are a strict security and correctness auditor.
Evaluate if the following agent output successfully satisfies the criteria for task: "%s".

CRITERIA / EXIT CRITERIA:
%s

AGENT OUTPUT:
%s

Respond ONLY with 'PASS' or 'FAIL: <category>: <reason>'.
Categories: TIMEOUT, PERMISSION, CONFLICT, LOGIC, SCHEMA`, step.Task, step.ExitCriteria, content)

	auditRes, err := s.spawner.Spawn(ctx, &SpawnRequest{
		TaskID:      graph.TaskID,
		StepID:      step.ID + "_critic",
		RoleID:      "auditor",
		Task:        auditPrompt,
		RoutingMode: schemas.RoutingModeLegacy,
		Hub:         NewContextHub(s.registry, graph, s.expStore),
	})
	if err != nil {
		return false, fmt.Sprintf("critic error: %v", err), schemas.FailureLogicError
	}
	auditText := fmt.Sprintf("%v", auditRes.Output.Result)
	upperText := strings.ToUpper(auditText)
	if strings.HasPrefix(upperText, "PASS") {
		return true, "", schemas.FailureNone
	}
	failType := schemas.FailureLogicError
	if strings.Contains(upperText, "TIMEOUT") {
		failType = schemas.FailureTimeout
	} else if strings.Contains(upperText, "PERMISSION") || strings.Contains(upperText, "ACCESS DENIED") {
		failType = schemas.FailurePermission
	} else if strings.Contains(upperText, "CONFLICT") {
		failType = schemas.FailureConflict
	} else if strings.Contains(upperText, "SCHEMA") || strings.Contains(upperText, "JSON") {
		failType = schemas.FailureSchemaViolation
	}
	return false, auditText, failType
}

// runProposerSynthesize asks the main model to revise its output given the critique.
// Returns the revised content.
func (s *DirectedEngine) runProposerSynthesize(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, currentContent, critique string) (string, error) {
	synthPrompt := fmt.Sprintf(`You previously produced the following output for task: "%s".

YOUR PREVIOUS OUTPUT:
%s

A cross-family critic model has reviewed your output and found these issues:
%s

Please produce a REVISED output that addresses all the critic's concerns.
Output ONLY the revised result, no explanations.`, step.Task, currentContent, critique)

	res, err := s.spawner.Spawn(ctx, &SpawnRequest{
		TaskID:      graph.TaskID,
		StepID:      step.ID + "_synthesize",
		RoleID:      step.RoleID,
		Task:        synthPrompt,
		RoutingMode: schemas.RoutingModeLegacy,
		Hub:         NewContextHub(s.registry, graph, s.expStore),
	})
	if err != nil {
		return currentContent, err
	}
	return fmt.Sprintf("%v", res.Output.Result), nil
}

// executeDebateAndAudit runs the Proposer→Critic→Synthesizer debate loop.
// Design ref: docs/architecture/CROSS_FAMILY_DEBATE_DESIGN.md
// Hard-capped at 2 rounds. Budget-guarded by the existing BudgetGuard.
func (s *DirectedEngine) executeDebateAndAudit(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, result *SpawnResult) (bool, string, schemas.VerificationFailureType) {
	s.logger.Log(EventDebateStarted, graph.TaskID, step.ID, map[string]any{
		"proposer": step.ProviderOverride,
		"critic":   step.VerifierModel,
	})

	currentContent := fmt.Sprintf("%v", result.Output.Result)
	rounds := step.MaxDebateRounds
	if rounds <= 0 {
		rounds = 1
	}
	if rounds > 2 {
		rounds = 2 // hard cap
	}

	var lastCritique string
	var lastFailType schemas.VerificationFailureType

	for r := 1; r <= rounds; r++ {
		passed, critique, failType := s.runCriticAudit(ctx, graph, step, currentContent)
		lastCritique = critique
		lastFailType = failType

		s.logger.Log(EventDebateCritiqueReceived, graph.TaskID, step.ID, map[string]any{
			"round":    r,
			"passed":   passed,
			"critique": critique,
		})

		if passed {
			s.logger.Log(EventDebateConcluded, graph.TaskID, step.ID, map[string]any{
				"consensus_round": r,
				"outcome":         "pass",
			})
			result.Output.Result["debate_content"] = currentContent
			return true, "", schemas.FailureNone
		}

		if r < rounds {
			revised, err := s.runProposerSynthesize(ctx, graph, step, currentContent, critique)
			if err != nil {
				s.logger.Log(EventDebateConcluded, graph.TaskID, step.ID, map[string]any{
					"outcome": "synthesize_error",
					"error":   err.Error(),
				})
				return false, critique, failType
			}
			currentContent = revised
		}
	}

	s.logger.Log(EventDebateConcluded, graph.TaskID, step.ID, map[string]any{
		"outcome":       "fail",
		"rounds":        rounds,
		"last_critique": lastCritique,
	})
	result.Output.Result["debate_content"] = currentContent
	return false, lastCritique, lastFailType
}

func (s *DirectedEngine) verifyExitCriteria(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, result *SpawnResult) (bool, string, schemas.VerificationFailureType) {
	output := result.Output
	content := fmt.Sprintf("%v", output.Result)

	// Case 1: Deterministic Artifact Contract validation (ADDED 2026-08-27)
	// Artifact Constraint Shield (arXiv:2608.24569)
	// Zero cost, zero LLM hallucination — runs before any VerifierModel audit.
	// NOTE: graph.Artifacts is only populated inside persistGraph, which runs
	// AFTER this verify call (see scheduler.go executeStep L1607). So we read
	// the step's OutputFiles directly from graph and construct an ephemeral
	// ArtifactContract for validation — no mutation of graph state.
	if len(step.OutputContract.RequiredFields) > 0 || len(step.OutputContract.TypeSchema) > 0 {
		for _, of := range graph.OutputFiles {
			if of.StepID != step.ID || !of.IsPrimary {
				continue
			}
			ephAc := schemas.ArtifactContract{
				Path:           of.Path,
				RequiredFields: step.OutputContract.RequiredFields,
				TypeSchema:     step.OutputContract.TypeSchema,
			}
			if err := ValidateArtifact(ephAc); err != nil {
				return false, fmt.Sprintf("artifact contract violated: %s", err.Error()),
					schemas.FailureSchemaViolation
			}
		}
	}

	// Case 2: Regex/File-based deterministic check (ADDED 2026-09-07)
	// Rationale: deterministic checks are zero-cost and zero-hallucination.
	if strings.HasPrefix(step.ExitCriteria, "regex:") || strings.HasPrefix(step.ExitCriteria, "file:") {
		var checkOK bool
		var checkErr string

		if strings.HasPrefix(step.ExitCriteria, "regex:") {
			pattern := strings.TrimPrefix(step.ExitCriteria, "regex:")
			re, err := compileRegexCache(pattern)
			if err != nil {
				return false, "invalid regex criteria", schemas.FailureLogicError
			}
			checkOK = re.MatchString(content)
			checkErr = "output did not match regex pattern: " + pattern
		} else { // file:
			path := strings.TrimPrefix(step.ExitCriteria, "file:")
			if info, err := os.Stat(path); err == nil && info.Size() > 1024 {
				checkOK = true
			} else {
				checkOK = false
				checkErr = "file not found or too small: " + path
			}
		}

		s.logger.Log("EventAutoAuditStarted", graph.TaskID, step.ID, map[string]any{
			"checker":  "deterministic",
			"criteria": step.ExitCriteria,
			"matched":  checkOK,
		})

		if checkOK && step.VerifierModel == "" {
			return true, "", schemas.FailureNone
		}
		if !checkOK && step.VerifierModel == "" {
			return false, checkErr, schemas.FailureLogicError
		}
		// VerifierModel set: defer final decision to LLM below.
	}

	// Case 2.5: Structured Deterministic Checks (ADDED 2026-09-13)
	// Zero-token hard verification via DeterministicVerifier registry.
	// Runs before VerifierModel semantic audit — if any check fails,
	// the step is immediately rejected without spending any LLM tokens.
	// See core/deterministic_verifier.go + docs/TIERED_SHORT_CIRCUIT_AUDIT.md
	if len(step.DeterministicChecks) > 0 {
		passed, msg, failTypeStr := runDeterministicChecks(ctx, s.outputBase, step.DeterministicChecks)
		if !passed {
			failType := schemas.FailureLogicError
			if failTypeStr == "schema_violation" {
				failType = schemas.FailureSchemaViolation
			}
			s.logger.Log("DeterministicVerificationFailed", graph.TaskID, step.ID, map[string]any{
				"reason": msg,
			})
			return false, msg, failType
		}
		s.logger.Log("DeterministicVerificationPassed", graph.TaskID, step.ID, nil)
	}

	// Case 3: Semantic Audit with Verifier Model (Relay Model)
	if step.VerifierModel != "" {
		// Cross-Family Debate (ADDED 2026-09-14): when EnableDebate is true,
		// run the Proposer→Critic→Synthesizer loop instead of a single audit.
		// See docs/architecture/CROSS_FAMILY_DEBATE_DESIGN.md
		if step.EnableDebate {
			return s.executeDebateAndAudit(ctx, graph, step, result)
		}
		s.logger.Log("EventAutoAuditStarted", graph.TaskID, step.ID, map[string]any{"model": step.VerifierModel})

		// Phase 2: Dynamic Rubric Generation (Self-Evolution Design)
		var dynamicRules string
		if len(step.DynamicRubrics) > 0 {
			dynamicRules = strings.Join(step.DynamicRubrics, "\n")
		} else {
			// Generate rubrics dynamically based on task intent
			s.logger.Log("EventDynamicRubricGen", graph.TaskID, step.ID, nil)
			rubrics, err := s.generateDynamicRubrics(ctx, graph, step)
			if err == nil && len(rubrics) > 0 {
				step.DynamicRubrics = rubrics
				dynamicRules = strings.Join(rubrics, "\n")
			} else {
				// Fallback to static baseline rules if generation fails
				dynamicRules = "1. If the output claims success without showing actual tool execution results or valid evidence.\n2. If any required field is missing, truncated, or placeholder/dummy data is used."
				if strings.Contains(strings.ToLower(step.Task), "code") || strings.Contains(strings.ToLower(step.Task), "go") || strings.Contains(strings.ToLower(step.Task), "patch") {
					dynamicRules += "\n3. If code changes introduce syntax errors, incomplete implementations, or violate compilation."
				}
			}
		}

		auditPrompt := fmt.Sprintf(`You are a strict security and correctness auditor. 
Evaluate if the following agent output successfully satisfies the criteria for task: "%s".

CRITERIA / EXIT CRITERIA:
%s

AGENT OUTPUT:
%s

STRICT RULES TO FAIL (Respond 'FAIL: LOGIC: <reason>'):
%s

Respond ONLY with 'PASS' or 'FAIL: <category>: <reason>'.
Categories: TIMEOUT, PERMISSION, CONFLICT, LOGIC, SCHEMA`, step.Task, step.ExitCriteria, content, dynamicRules)

		auditRes, err := s.spawner.Spawn(ctx, &SpawnRequest{
			TaskID:      graph.TaskID,
			StepID:      step.ID + "_audit",
			RoleID:      "auditor",
			Task:        auditPrompt,
			RoutingMode: schemas.RoutingModeLegacy,
			Hub:         NewContextHub(s.registry, graph, s.expStore),
		})
		if err == nil {
			auditText := fmt.Sprintf("%v", auditRes.Output.Result)
			upperText := strings.ToUpper(auditText)
			if strings.HasPrefix(upperText, "PASS") {
				return true, "", schemas.FailureNone
			}

			// Enhanced classification logic (Event-driven recovery seed)
			failType := schemas.FailureLogicError
			if strings.Contains(upperText, "TIMEOUT") {
				failType = schemas.FailureTimeout
			} else if strings.Contains(upperText, "PERMISSION") || strings.Contains(upperText, "ACCESS DENIED") {
				failType = schemas.FailurePermission
			} else if strings.Contains(upperText, "CONFLICT") {
				failType = schemas.FailureConflict
			} else if strings.Contains(upperText, "SCHEMA") || strings.Contains(upperText, "JSON") {
				failType = schemas.FailureSchemaViolation
			}

			return false, auditText, failType
		}
	}

	// Case 4: JSON validation
	if step.ExitCriteria == "json" {
		var j any
		if err := json.Unmarshal([]byte(content), &j); err != nil {
			// Attempt truncation repair if LLM output was cut by max_tokens.
			// This is a best-effort fallback — if the original parse failed
			// but the repaired version succeeds, we accept it and log the
			// event for observability.
			if repaired, ok := jsonrepair.Repair(content); ok {
				if err2 := json.Unmarshal([]byte(repaired), &j); err2 == nil {
					s.logger.Log("EventJSONTruncationRepaired", graph.TaskID, step.ID, map[string]any{
						"original_len":  len(content),
						"repaired_len":  len(repaired),
						"exit_criteria": step.ExitCriteria,
					})
					return true, "", schemas.FailureNone
				}
			}
			return false, "output is not valid JSON", schemas.FailureSchemaViolation
		}
		return true, "", schemas.FailureNone
	}

	// Case 4: Substring matching (default)
	if step.ExitCriteria != "" && !strings.Contains(strings.ToLower(content), strings.ToLower(step.ExitCriteria)) {
		return false, fmt.Sprintf("output missing required keyword: %s", step.ExitCriteria), schemas.FailureLogicError
	}

	return true, "", schemas.FailureNone
}

func joinCaps(caps []string) string {
	result := ""
	for i, c := range caps {
		if i > 0 {
			result += "+"
		}
		result += c
	}
	return result
}

func (s *DirectedEngine) generateDynamicRubrics(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step) ([]string, error) {
	prompt := fmt.Sprintf("Analyze the following task and generate 3 specific, micro-level audit constraints (Rubrics) to verify its success. Output ONLY the list items, one per line:\n\nTask: %s", step.Task)

	// Use the spawner to execute a zero-shot generation call. We use the
	// VerifierModel specified for the step.
	resp, err := s.spawner.Spawn(ctx, &SpawnRequest{
		TaskID:           graph.TaskID,
		StepID:           "rubric_" + step.ID,
		RoleID:           "auditor",
		Task:             prompt,
		ProviderOverride: step.VerifierModel,
		Isolation:        true,
	})
	if err != nil {
		return nil, err
	}

	content := ""
	if c, ok := resp.Output.Result["content"].(string); ok {
		content = c
	} else if r, ok := resp.Output.Result["raw"].(string); ok {
		content = r
	} else {
		// Fallback to json string representation
		content = fmt.Sprintf("%v", resp.Output.Result)
	}

	lines := strings.Split(content, "\n")
	var rubrics []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			rubrics = append(rubrics, l)
		}
	}
	return rubrics, nil
}
