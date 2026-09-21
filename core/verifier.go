package core

import (
	"context"
)

// VerificationState is a generic container for environment snapshots.
type VerificationState any

// ToolExecutor provides a minimal interface for Verifiers to execute a tool
// without triggering the full SAV loop (preventing infinite recursion).
type ToolExecutor interface {
	DirectExecute(ctx context.Context, mcpID, toolName string, args map[string]any) (any, error)
}

// Verifier defines the contract for auditing tool execution outcomes.
type Verifier interface {
	// Sense: captures the environment state before the action.
	Sense(ctx context.Context, mcpID, toolName string, executor ToolExecutor) (VerificationState, error)

	// Verify: compares the state after action against the 'before' snapshot.
	Verify(ctx context.Context, mcpID, toolName string, before VerificationState, result any, executor ToolExecutor) (bool, error)

	// Recover: attempts to fix a failed state (e.g., page refresh).
	Recover(ctx context.Context, mcpID, toolName string, args map[string]any, err error, executor ToolExecutor) (any, error)
}
