package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/store"
	"github.com/google/uuid"
)

// MutationVariant represents a single mutation of a forked task history.
type MutationVariant struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// ReplayMutationRecord tracks the lifecycle of a mutation attempt.
// See docs/architecture/OFFLINE_REPLAY_SELF_OPTIMIZATION_DESIGN.md.
type ReplayMutationRecord struct {
	ID              string    `json:"id"`
	SourceTaskID    string    `json:"source_task_id"`
	SourceStepID    string    `json:"source_step_id"`
	MutationType    string    `json:"mutation_type"`
	MutationPayload string    `json:"mutation_payload"`
	VerifierResult  string    `json:"verifier_result"`
	VerifierReason  string    `json:"verifier_reason"`
	CandidateID     string    `json:"candidate_id"`
	Timestamp       time.Time `json:"timestamp"`
}

// ReplayScheduler drives the offline self-optimization loop:
// scan → fork → mutate → verify → write-back.
type ReplayScheduler interface {
	ScanFailedTasks(ctx context.Context, since time.Time) ([]string, error)
	Mutate(history []AgentEvent, atStepID string) ([]MutationVariant, error)
	RunReplayCycle(ctx context.Context) error
}

type replayScheduler struct {
	replayer            *Replayer
	verifier            Verifier
	expStore            *store.ExperienceStore
	db                  *sql.DB
	executor            ToolExecutor
	maxVariants         int
	grayscale           *GrayscaleController
	crossFamilyResolver func() string
	spawner             *Spawner
}

// NewReplayScheduler creates a ReplayScheduler from the existing infra.
// verifier and executor may be nil (Phase 1: verification skipped).
// Returns the concrete type so callers can access Start.
func NewReplayScheduler(replayer *Replayer, verifier Verifier, es *store.ExperienceStore, db *sql.DB, executor ToolExecutor) *replayScheduler {
	return &replayScheduler{
		replayer:    replayer,
		verifier:    verifier,
		expStore:    es,
		db:          db,
		executor:    executor,
		maxVariants: 5,
		grayscale:   NewGrayscaleController(0.05, 10),
	}
}

// SetCrossFamilyVerifier wires the cross-family verifier auto-resolution.
// When verifier is nil, verifyMutation will call resolver to obtain a
// provider ID from a different model family, then use spawner to run an
// LLM-based audit with that provider. See OFFLINE_REPLAY_SELF_OPTIMIZATION_DESIGN.md
// §安全约束清单 item 6.
func (rs *replayScheduler) SetCrossFamilyVerifier(resolver func() string, spawner *Spawner) {
	rs.crossFamilyResolver = resolver
	rs.spawner = spawner
}

// ReplaySandbox is a mock ToolExecutor that returns canned responses instead
// of calling real MCP services. Used during replay verification to isolate
// the environment from production side effects.
type ReplaySandbox struct {
	responses map[string]any
}

func NewReplaySandbox() *ReplaySandbox {
	return &ReplaySandbox{
		responses: map[string]any{
			"ok": map[string]any{"status": "ok", "sandbox": true},
		},
	}
}

func (s *ReplaySandbox) DirectExecute(ctx context.Context, mcpID, toolName string, args map[string]any) (any, error) {
	if r, ok := s.responses["ok"]; ok {
		return r, nil
	}
	return map[string]any{"status": "ok", "sandbox": true}, nil
}

// GrayscaleController manages the grayscale traffic control lifecycle for
// replay-generated JIT candidates: 5% trial → promote after N successes →
// archive on failure. See OFFLINE_REPLAY_SELF_OPTIMIZATION_DESIGN.md §决策3.
type GrayscaleController struct {
	mu                 sync.Mutex
	trials             map[string]*trialState
	trialRate          float64
	promotionThreshold int
}

type trialState struct {
	consecutiveSuccesses int
	trialAttempts        int
	promoted             bool
	archived             bool
}

func NewGrayscaleController(trialRate float64, promotionThreshold int) *GrayscaleController {
	if trialRate < 0 {
		trialRate = 0.05
	}
	if promotionThreshold <= 0 {
		promotionThreshold = 10
	}
	return &GrayscaleController{
		trials:             make(map[string]*trialState),
		trialRate:          trialRate,
		promotionThreshold: promotionThreshold,
	}
}

// ShouldTrial decides whether a candidate should be included in this trial
// round. Returns true with probability trialRate for candidates not yet
// promoted or archived.
func (g *GrayscaleController) ShouldTrial(candidateID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.trials[candidateID]
	if st == nil {
		st = &trialState{}
		g.trials[candidateID] = st
	}
	if st.promoted || st.archived {
		return st.promoted
	}
	st.trialAttempts++
	return rand.Float64() <= g.trialRate
}

// RecordOutcome updates the consecutive success/failure count for a candidate.
func (g *GrayscaleController) RecordOutcome(candidateID string, success bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.trials[candidateID]
	if st == nil {
		st = &trialState{}
		g.trials[candidateID] = st
	}
	if success {
		st.consecutiveSuccesses++
	} else {
		st.consecutiveSuccesses = 0
		st.archived = true
	}
}

// ShouldPromote returns true if the candidate has reached the promotion
// threshold of consecutive successes.
func (g *GrayscaleController) ShouldPromote(candidateID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.trials[candidateID]
	if st == nil || st.promoted || st.archived {
		return false
	}
	if st.consecutiveSuccesses >= g.promotionThreshold {
		st.promoted = true
		return true
	}
	return false
}

// IsArchived returns true if the candidate was archived due to failure.
func (g *GrayscaleController) IsArchived(candidateID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.trials[candidateID]
	return st != nil && st.archived
}

// ScanFailedTasks finds tasks with failed/aborted status since the given time.
// Uses the SQLite tasks table (indexed by status) rather than scanning the
// trajectory log, which is append-only and unindexed.
func (rs *replayScheduler) ScanFailedTasks(ctx context.Context, since time.Time) ([]string, error) {
	if rs.db == nil {
		return nil, nil
	}
	cutoff := since.Format("2006-01-02 15:04:05")
	rows, err := rs.db.QueryContext(ctx,
		`SELECT task_id FROM tasks WHERE status IN ('failed', 'aborted') AND updated_at >= ? ORDER BY updated_at DESC LIMIT 100`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Mutate generates mutation variants for a forked history using three
// deterministic strategies: RewriteStrategy, AlternativeToolchain, and
// ParamPerturbation. See OFFLINE_REPLAY_SELF_OPTIMIZATION_DESIGN.md §决策2.
func (rs *replayScheduler) Mutate(history []AgentEvent, atStepID string) ([]MutationVariant, error) {
	variants := make([]MutationVariant, 0, rs.maxVariants)

	for _, ev := range history {
		if ev.StepID != atStepID || ev.EventType != "tool_call" {
			continue
		}
		toolName, _ := ev.Payload["tool"].(string)
		args, _ := ev.Payload["args"].(map[string]any)
		if args == nil {
			args = make(map[string]any)
		}

		// ── RewriteStrategy (3 variants) ────────────────────────────────
		v1 := make(map[string]any, len(args)+1)
		for k, v := range args {
			v1[k] = v
		}
		if _, ok := v1["exit_criteria"]; !ok {
			v1["exit_criteria"] = "task completed with deterministic verification"
		}
		v1["_mutation"] = "add_exit_criteria"
		variants = append(variants, MutationVariant{Type: "rewrite", Payload: v1})

		v2 := make(map[string]any)
		for k, v := range args {
			if k == "context" || k == "verbose" || k == "debug" {
				continue
			}
			v2[k] = v
		}
		v2["_mutation"] = "trim_redundant"
		v2["_tool"] = toolName
		variants = append(variants, MutationVariant{Type: "rewrite", Payload: v2})

		v3 := make(map[string]any, len(args)+1)
		for k, v := range args {
			v3[k] = v
		}
		v3["_mutation"] = "inject_experience_ref"
		v3["_experience_hint"] = "consider similar past patterns for this capability"
		variants = append(variants, MutationVariant{Type: "rewrite", Payload: v3})

		// ── AlternativeToolchain (1 variant) ────────────────────────────
		if alt := alternativeTool(toolName); alt != "" {
			v4 := make(map[string]any, len(args)+1)
			for k, v := range args {
				v4[k] = v
			}
			v4["_mutation"] = "toolchain_swap"
			v4["_original_tool"] = toolName
			v4["_tool"] = alt
			variants = append(variants, MutationVariant{Type: "toolchain", Payload: v4})
		}

		// ── ParamPerturbation (1 variant) ──────────────────────────────
		v5 := perturbParams(args)
		if v5 != nil {
			v5["_mutation"] = "param_perturbation"
			v5["_tool"] = toolName
			variants = append(variants, MutationVariant{Type: "perturbation", Payload: v5})
		}

		break
	}

	return variants, nil
}

// alternativeTool returns an equivalent tool for the given tool name, or ""
// if no known equivalent exists. Based on capability equivalence.
func alternativeTool(toolName string) string {
	equivalents := map[string]string{
		"exec_command": "orchestrator_invoke",
		"write_file":   "patch",
		"read_file":    "grep",
	}
	return equivalents[toolName]
}

// perturbParams applies ±10% random perturbation to numeric parameters.
// Returns nil if no numeric parameters were found to perturb.
func perturbParams(args map[string]any) map[string]any {
	result := make(map[string]any, len(args)+1)
	perturbed := false
	for k, v := range args {
		switch n := v.(type) {
		case int:
			delta := int(float64(n) * (rand.Float64()*0.2 - 0.1))
			if delta == 0 && n != 0 {
				delta = 1
			}
			result[k] = n + delta
			perturbed = true
		case int64:
			delta := int64(float64(n) * (rand.Float64()*0.2 - 0.1))
			if delta == 0 && n != 0 {
				delta = 1
			}
			result[k] = n + delta
			perturbed = true
		case float64:
			delta := n * (rand.Float64()*0.2 - 0.1)
			result[k] = n + delta
			perturbed = true
		case float32:
			delta := n * float32(rand.Float64()*0.2-0.1)
			result[k] = n + delta
			perturbed = true
		default:
			result[k] = v
		}
	}
	if !perturbed {
		return nil
	}
	return result
}

// extractRootCause reads the root_cause field from the last step_failed
// event in the task history. Returns "" if not found (e.g. older tasks
// logged before root_cause was added to event payloads).
func extractRootCause(history []AgentEvent) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].EventType == "step_failed" || history[i].EventType == string(EventStepFailed) {
			if cause, ok := history[i].Payload["root_cause"].(string); ok && cause != "" {
				return cause
			}
		}
	}
	return ""
}

// mapCauseToMutation maps a FailureClass (from core/failure_classify.go) to
// the appropriate mutation type. Returns (mutationType, fixable).
// Unfixable failures return ("", false) and are skipped by RunReplayCycle.
func mapCauseToMutation(cause string) (mutType string, fixable bool) {
	switch FailureClass(cause) {
	case FailureClassContextDeficit, FailureClassGenerativeUncertainty, FailureClassContractViolation:
		return "rewrite", true
	case FailureClassCapabilityRequired, FailureClassMissingDependency:
		return "toolchain", true
	case FailureClassRateLimit:
		return "perturbation", true
	default:
		return "", false
	}
}

// RunReplayCycle executes one full cycle: scan → fork → mutate → verify → write-back.
func (rs *replayScheduler) RunReplayCycle(ctx context.Context) error {
	log.Printf("[ReplayScheduler] starting replay cycle...")

	since := time.Now().AddDate(0, 0, -7)
	taskIDs, err := rs.ScanFailedTasks(ctx, since)
	if err != nil {
		return err
	}
	if len(taskIDs) == 0 {
		log.Printf("[ReplayScheduler] no failed tasks found since %s", since.Format("2006-01-02"))
		return nil
	}

	totalMutations := 0
	totalVerified := 0
	skipped := 0

	for _, taskID := range taskIDs {
		history, err := rs.replayer.LoadHistory(taskID)
		if err != nil || len(history) == 0 {
			continue
		}

		cause := extractRootCause(history)
		mutType, fixable := mapCauseToMutation(cause)
		if !fixable {
			if cause == "" {
				mutType = ""
			} else {
				skipped++
				continue
			}
		}

		failedStepID := findFailedStep(history)
		if failedStepID == "" {
			continue
		}

		forkHistory, err := rs.replayer.ForkTask(taskID, failedStepID)
		if err != nil {
			continue
		}

		variants, err := rs.Mutate(forkHistory, failedStepID)
		if err != nil || len(variants) == 0 {
			continue
		}

		if mutType != "" {
			filtered := variants[:0]
			for _, v := range variants {
				if v.Type == mutType {
					filtered = append(filtered, v)
				}
			}
			variants = filtered
		}

		for _, v := range variants {
			totalMutations++
			record := rs.processMutation(ctx, taskID, failedStepID, v)
			if record.VerifierResult == "verified" {
				totalVerified++
			}
			rs.persistRecord(ctx, record)
		}
	}

	log.Printf("[ReplayScheduler] cycle complete: %d tasks, %d skipped, %d mutations, %d verified", len(taskIDs), skipped, totalMutations, totalVerified)
	return nil
}

func (rs *replayScheduler) processMutation(ctx context.Context, taskID, stepID string, v MutationVariant) ReplayMutationRecord {
	record := ReplayMutationRecord{
		ID:           "rmr_" + uuid.New().String()[:8],
		SourceTaskID: taskID,
		SourceStepID: stepID,
		MutationType: v.Type,
		Timestamp:    time.Now(),
	}
	payloadBytes, _ := json.Marshal(v.Payload)
	record.MutationPayload = string(payloadBytes)

	verified, reason := rs.verifyMutation(ctx, v)
	if verified {
		record.VerifierResult = "verified"
	} else if reason == "no_verifier" {
		record.VerifierResult = "skipped"
	} else {
		record.VerifierResult = "rejected"
	}
	record.VerifierReason = reason

	if verified || record.VerifierResult == "skipped" {
		candidateID := "jit_" + uuid.New().String()[:8]
		record.CandidateID = candidateID
		rs.writeBack(ctx, candidateID, v, verified)
	}

	return record
}

func (rs *replayScheduler) verifyMutation(ctx context.Context, v MutationVariant) (bool, string) {
	if rs.verifier == nil {
		if rs.crossFamilyResolver != nil && rs.spawner != nil {
			providerID := rs.crossFamilyResolver()
			if providerID != "" {
				return rs.verifyWithCrossFamily(ctx, providerID, v)
			}
		}
		return false, "no_verifier"
	}
	executor := rs.executor
	if executor == nil {
		executor = NewReplaySandbox()
	}
	toolName, _ := v.Payload["_tool"].(string)
	if toolName == "" {
		toolName = "unknown"
	}
	before, err := rs.verifier.Sense(ctx, "replay", toolName, executor)
	if err != nil {
		return false, "sense_error: " + err.Error()
	}
	ok, err := rs.verifier.Verify(ctx, "replay", toolName, before, v.Payload, executor)
	if err != nil {
		return false, "verify_error: " + err.Error()
	}
	if !ok {
		return false, "assertion_failed"
	}
	return true, ""
}

// verifyWithCrossFamily runs an LLM-based audit using a provider from a
// different model family. This is the auto-resolution path when no
// deterministic Verifier is configured but a cross-family provider is
// available. See OFFLINE_REPLAY_SELF_OPTIMIZATION_DESIGN.md §安全约束清单 item 6.
func (rs *replayScheduler) verifyWithCrossFamily(ctx context.Context, providerID string, v MutationVariant) (bool, string) {
	toolName, _ := v.Payload["_tool"].(string)
	if toolName == "" {
		toolName = "unknown"
	}
	payloadBytes, _ := json.Marshal(v.Payload)
	auditPrompt := fmt.Sprintf(`You are a strict correctness auditor verifying a mutation variant.
Evaluate if the following mutation produces a correct result for the tool: %s.

MUTATION TYPE: %s
MUTATION PAYLOAD: %s

Respond ONLY with 'PASS' or 'FAIL: <reason>'.`, toolName, v.Type, string(payloadBytes))

	res, err := rs.spawner.Spawn(ctx, &SpawnRequest{
		TaskID:           "replay_audit",
		StepID:           "replay_audit_" + v.Type,
		RoleID:           "auditor",
		Task:             auditPrompt,
		ProviderOverride: providerID,
		Isolation:        true,
	})
	if err != nil {
		return false, "cross_family_spawn_error: " + err.Error()
	}
	auditText := fmt.Sprintf("%v", res.Output.Result)
	if strings.HasPrefix(strings.ToUpper(auditText), "PASS") {
		return true, "cross_family_verified:" + providerID
	}
	return false, "cross_family_rejected:" + auditText
}

func (rs *replayScheduler) writeBack(ctx context.Context, candidateID string, v MutationVariant, verified bool) {
	if rs.expStore == nil {
		return
	}
	status := "pending"
	if verified {
		status = "audited"
	}
	candidate := &store.JITCandidate{
		ID:             candidateID,
		SequenceKey:    v.Type,
		SampleCount:    1,
		AvgConfidence:  0.5,
		Status:         status,
		PromotionNotes: v.Type + " mutation from replay",
		CreatedAt:      time.Now(),
		LastSeen:       time.Now(),
	}
	_ = rs.expStore.AddJITCandidate(ctx, candidate)

	action := "promoted"
	if !verified {
		action = "pending"
	}
	rs.expStore.AddPromotionAuditLog(store.PromotionAuditLog{
		CandidateID: candidateID,
		Action:      action,
		Auditor:     "replay_scheduler",
		Reason:      "offline replay self-optimization",
		Timestamp:   time.Now(),
	})

	if rs.grayscale != nil {
		rs.grayscale.RecordOutcome(candidateID, verified)
		if rs.grayscale.ShouldPromote(candidateID) {
			rs.expStore.AddPromotionAuditLog(store.PromotionAuditLog{
				CandidateID: candidateID,
				Action:      "promoted",
				Auditor:     "grayscale",
				Reason:      "auto-promoted after 10 consecutive successes",
				Timestamp:   time.Now(),
			})
			log.Printf("[ReplayScheduler] candidate %s auto-promoted via grayscale", candidateID)
		}
	}
}

func (rs *replayScheduler) persistRecord(ctx context.Context, record ReplayMutationRecord) {
	if rs.db == nil {
		return
	}
	_, err := rs.db.ExecContext(ctx,
		`INSERT INTO replay_mutation_records (id, source_task_id, source_step_id, mutation_type, mutation_payload, verifier_result, verifier_reason, candidate_id, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.SourceTaskID, record.SourceStepID, record.MutationType,
		record.MutationPayload, record.VerifierResult, record.VerifierReason,
		record.CandidateID, record.Timestamp,
	)
	if err != nil {
		log.Printf("[ReplayScheduler] WARN: failed to persist mutation record: %v", err)
	}
}

func findFailedStep(history []AgentEvent) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].EventType == "step_failed" || history[i].EventType == "tool_error" {
			return history[i].StepID
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].EventType == "step_completed" {
			return ""
		}
		if history[i].StepID != "" {
			return history[i].StepID
		}
	}
	return ""
}

// Start runs the replay cycle on a fixed interval in a background goroutine.
func (rs *replayScheduler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rs.RunReplayCycle(ctx); err != nil {
				log.Printf("[ReplayScheduler] cycle failed: %v", err)
			}
		}
	}
}
