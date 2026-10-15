package core

import (
	"context"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"time"
)

// OrchestrationEngine defines the abstraction for different scheduling strategies.
type OrchestrationEngine interface {
	Submit(inputs []schemas.StepInput) (string, error)
	GetStatus(taskID string, view ...string) (map[string]any, bool)
	WaitTask(ctx context.Context, taskID string, timeout time.Duration, view ...string) (map[string]any, bool)
	SubmitDecision(taskID, decisionID, choice string) error
	SubmitDecisionWithPayload(taskID, decisionID, choice, payload string) error
	Clear(taskID string) error
	GetStepResult(taskID, stepID string) *store.StepResult
	GetManifest(taskID string) (map[string]any, bool)
	CancelTask(taskID string) error
	PatchWorkspace(taskID string, data map[string]any) error
	Refresh() error
}
