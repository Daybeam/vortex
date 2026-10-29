package core

import (
	"context"
	"fmt"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// ── ACP Spawner Integration ─────────────────────────────────────────────────
//
// SpawnACP delegates a DAG step to an external ACP-compliant worker via
// JSON-RPC over stdio. The interceptor chain (MCP setup, health checks, etc.)
// is bypassed — the external agent manages its own tools. Orchestrator's
// L1-L3 verification pipeline still runs on the returned diff downstream.
//
// See docs/completed/2026-10-28/ACP_CLIENT_SPAWNER_INTEGRATION_DESIGN.md §3.3.

// SpawnACP executes a step by delegating to an external ACP worker.
func (s *Spawner) SpawnACP(ctx context.Context, req *SpawnRequest, acpCfg *schemas.ACPStepConfig) (*SpawnResult, error) {
	// Apply timeout if configured.
	if acpCfg.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(acpCfg.TimeoutSec)*time.Second)
		defer cancel()
	}

	client, err := NewACPClient(acpCfg.BinaryPath, acpCfg.Args, acpCfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("acp spawn %q: %w", acpCfg.BinaryPath, err)
	}
	defer func() { _ = client.Close() }()

	s.logger.Log("acp_spawn_start", req.TaskID, req.StepID, map[string]any{
		"binary": acpCfg.BinaryPath,
		"args":   acpCfg.Args,
	})

	// 1. Handshake.
	if err := client.Initialize(ctx); err != nil {
		s.logger.Log("acp_spawn_init_failed", req.TaskID, req.StepID, map[string]any{
			"error": err.Error(),
		})
		return nil, fmt.Errorf("acp initialize: %w", err)
	}

	// 2. Execute task with streaming event callback.
	result, err := client.ExecuteTask(ctx, req.Task, req.RoleID, func(method string, params map[string]any) {
		s.logger.Log("acp_event", req.TaskID, req.StepID, map[string]any{
			"method": method,
			"params": params,
		})
	})
	if err != nil {
		s.logger.Log("acp_spawn_failed", req.TaskID, req.StepID, map[string]any{
			"error": err.Error(),
		})
		return nil, err
	}

	s.logger.Log("acp_spawn_done", req.TaskID, req.StepID, map[string]any{
		"result_keys": len(result),
	})

	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status: schemas.StatusOK,
			Result: result,
		},
		Ref: req.TaskID + ":" + req.StepID,
	}, nil
}
