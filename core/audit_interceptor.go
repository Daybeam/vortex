package core

import (
	"context"
	"time"

	"github.com/daybeam/vortex/pkg/observability"
)

// AuditInterceptor logs detailed entry/exit of every subagent call with tracing.
func AuditInterceptor(logger *Logger) Interceptor {
	metrics := observability.GetGlobalMetrics()

	return func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
		start := time.Now()
		traceID := GetTraceID(ctx)
		spanID := GetSpanID(ctx)

		metrics.Inc("orchestrator_subagent_calls_total", 1)

		logger.LogCtx(ctx, "audit_entry", req.TaskID, req.StepID, map[string]any{
			"role_id":  req.RoleID,
			"task":     req.Task,
			"trace_id": traceID,
			"span_id":  spanID,
		})

		res, err := next(ctx, req)

		duration := time.Since(start).Seconds()
		metrics.Set("orchestrator_last_subagent_duration_seconds", duration)

		detail := map[string]any{
			"duration_sec": duration,
			"trace_id":     traceID,
			"span_id":      spanID,
		}

		if err != nil {
			metrics.Inc("orchestrator_subagent_errors_total", 1)
			detail["error"] = err.Error()
			logger.LogCtx(ctx, "audit_exit_error", req.TaskID, req.StepID, detail)
			return res, err
		}

		detail["status"] = res.Output.Status
		detail["confidence"] = res.Output.Confidence
		detail["provider"] = res.ProviderID
		detail["model"] = res.ModelID

		if res.Output.Usage != nil {
			detail["usage"] = res.Output.Usage
			metrics.Inc("orchestrator_tokens_total", int64(res.Output.Usage.TotalTokens))
		}

		logger.LogCtx(ctx, "audit_exit_success", req.TaskID, req.StepID, detail)
		return res, nil
	}
}
