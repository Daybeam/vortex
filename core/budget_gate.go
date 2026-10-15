package core

// budget_gate.go
//
// Automatic task suspension when cumulative token usage exceeds a configured
// threshold. Subscribes to EventStepCompleted on the EventBus, extracts token
// usage from the payload, and calls SuspendTask when the per-task or per-role
// budget is exhausted.
//
// Design constraints (from WEBHOOK_GOVERNANCE_DEVELOPMENT_PLAN.md §3):
//   - Reuses EventTaskSuspended / EventTaskResumed (no new event types).
//   - No dollar-amount conversion; pure token count.
//   - No distributed budget pool; single-instance only.
//   - Does not block the Publish hot path (check is synchronous but cheap).
//   - v1: no rolling window (WindowMinutes reserved), no auto-resume.

import (
	"log"
	"sync"

	"github.com/daybeam/vortex/config"
)

// BudgetGate monitors token usage and suspends tasks when budgets are exceeded.
type BudgetGate struct {
	cfg       config.BudgetGateConfig
	scheduler *DirectedEngine

	mu        sync.Mutex
	usage     map[string]int64 // task_id → cumulative token count
	roleUsage map[string]int64 // role_id → cumulative token count
	suspended map[string]bool  // task_id → already suspended (avoid double-suspend)

	unsubscribe func()
}

// NewBudgetGate creates a BudgetGate. Returns nil if disabled.
func NewBudgetGate(cfg config.BudgetGateConfig, scheduler *DirectedEngine) *BudgetGate {
	if !cfg.Enabled || scheduler == nil {
		return nil
	}
	return &BudgetGate{
		cfg:         cfg,
		scheduler:   scheduler,
		usage:       make(map[string]int64),
		roleUsage:   make(map[string]int64),
		suspended:   make(map[string]bool),
	}
}

// Wire subscribes the BudgetGate to EventStepCompleted on the global EventBus.
// Call once at startup; defer the returned func to tear down.
func (g *BudgetGate) Wire() func() {
	if g == nil {
		return func() {}
	}
	g.unsubscribe = DefaultBus.SubscribeWithCancel(string(EventStepCompleted), g.OnStepCompleted)
	return g.Unwire
}

// Unwire tears down the EventBus subscription. Safe to call multiple times.
func (g *BudgetGate) Unwire() {
	if g == nil || g.unsubscribe == nil {
		return
	}
	g.unsubscribe()
	g.unsubscribe = nil
}

// OnStepCompleted is the EventBus callback. It extracts token usage from the
// event payload, accumulates per-task and per-role totals, and suspends the
// task if a threshold is exceeded.
func (g *BudgetGate) OnStepCompleted(ev AgentEvent) error {
	if !g.cfg.Enabled || ev.TaskID == "" {
		return nil
	}

	tokens := extractTokens(ev.Payload)
	if tokens == 0 {
		return nil
	}

	roleID := extractRoleID(ev.Payload)

	g.mu.Lock()
	g.usage[ev.TaskID] += tokens
	taskUsage := g.usage[ev.TaskID]

	var roleUsage int64
	if roleID != "" {
		g.roleUsage[roleID] += tokens
		roleUsage = g.roleUsage[roleID]
	}

	alreadySuspended := g.suspended[ev.TaskID]
	g.mu.Unlock()

	if alreadySuspended {
		return nil
	}

	reason := g.checkThreshold(taskUsage, roleUsage, roleID)
	if reason == "" {
		return nil
	}

	if err := g.scheduler.SuspendTask(ev.TaskID); err != nil {
		log.Printf("WARN budget_gate: SuspendTask(%s) failed: %v", ev.TaskID, err)
		return nil
	}

	g.mu.Lock()
	g.suspended[ev.TaskID] = true
	g.mu.Unlock()

	log.Printf("INFO budget_gate: task %s suspended (%s): task_usage=%d role_usage=%d role=%s",
		ev.TaskID, reason, taskUsage, roleUsage, roleID)
	return nil
}

// checkThreshold returns a suspension reason string if a threshold is exceeded,
// or "" if within budget.
func (g *BudgetGate) checkThreshold(taskUsage, roleUsage int64, roleID string) string {
	if g.cfg.PerTaskTokens > 0 && taskUsage > g.cfg.PerTaskTokens {
		return "budget_exhausted:per_task"
	}
	if g.cfg.PerRoleTokens > 0 && roleID != "" && roleUsage > g.cfg.PerRoleTokens {
		return "budget_exhausted:per_role"
	}
	return ""
}

// GetTaskUsage returns the cumulative token usage for a task. Thread-safe.
func (g *BudgetGate) GetTaskUsage(taskID string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.usage[taskID]
}

// GetRoleUsage returns the cumulative token usage for a role. Thread-safe.
func (g *BudgetGate) GetRoleUsage(roleID string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.roleUsage[roleID]
}

// IsSuspended returns true if the BudgetGate has suspended this task.
func (g *BudgetGate) IsSuspended(taskID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.suspended[taskID]
}

// extractTokens pulls the total token count from a step-completed event payload.
// Returns 0 when usage data is absent (skip, don't block).
func extractTokens(payload map[string]any) int64 {
	if payload == nil {
		return 0
	}
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		return 0
	}
	// Prefer total_tokens; fall back to prompt + completion.
	if v, ok := usage["total_tokens"].(int); ok && v > 0 {
		return int64(v)
	}
	if v, ok := usage["total_tokens"].(float64); ok && v > 0 {
		return int64(v)
	}
	prompt := extractInt(usage, "prompt_tokens")
	completion := extractInt(usage, "completion_tokens")
	if prompt+completion > 0 {
		return prompt + completion
	}
	return 0
}

func extractInt(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// extractRoleID pulls the role_id from the event payload.
func extractRoleID(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload["role_id"].(string); ok {
		return v
	}
	return ""
}
