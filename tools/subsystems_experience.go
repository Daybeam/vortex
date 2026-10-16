package tools

import (
	"context"
	"fmt"
)

func registerExperienceSubsystem(app *App) {
	s := &Subsystem{
		Name:        "exp",
		Description: "Historical patterns and success rates.",
		Actions:     make(map[string]Action),
	}

	s.Actions["get_brief"] = Action{
		Name:        "get_brief",
		Description: "Get orchestration brief for specific capabilities.",
		Parameters:  map[string]any{"capabilities": "string array"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			caps := strSliceArg(args, "capabilities")
			return app.ExpStore.GetOrchestrationBrief(ctx, caps), nil
		},
	}

	s.Actions["get_summary"] = Action{
		Name:        "get_summary",
		Description: "High-level summary of experience store.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			profiles := app.ExpStore.GetRoleProfilesSnapshot()
			topRoles := make([]map[string]any, 0)
			for _, p := range profiles {
				if p.TotalRuns < 2 {
					continue
				}
				topRoles = append(topRoles, map[string]any{
					"role_id": p.RoleID, "total_runs": p.TotalRuns, "success_rate": float64(p.SuccessCount) / float64(p.TotalRuns),
				})
			}
			return map[string]any{
				"task_patterns": len(app.ExpStore.GetTaskPatternsSnapshot()),
				"top_roles":     topRoles,
			}, nil
		},
	}

	s.Actions["get_roi_summary"] = Action{
		Name:        "get_roi_summary",
		Description: "Get efficiency and ROI metrics based on historical execution.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			// GetTaskPatternsSnapshot handles its own locking internally.
			patterns := app.ExpStore.GetTaskPatternsSnapshot()
			totalTasks := 0
			totalSteps := 0
			confSum := 0.0
			for _, p := range patterns {
				totalTasks += p.SampleCount
				totalSteps += p.SampleCount * len(p.StepSequence)
				confSum += p.AvgConfidence * float64(p.SampleCount)
			}

			avgConf := 0.0
			if totalTasks > 0 {
				avgConf = confSum / float64(totalTasks)
			}

			// Efficiency calculation: tokens saved estimate (planning avoidance)
			// Each repeated pattern saves a planning call (~1500 tokens)
			tokensSaved := 0
			for _, p := range patterns {
				if p.SampleCount > 1 {
					tokensSaved += (p.SampleCount - 1) * 1500
				}
			}

			return map[string]any{
				"total_tasks_orchestrated":   totalTasks,
				"total_steps_executed":       totalSteps,
				"estimated_tokens_saved":     tokensSaved,
				"estimated_time_saved_secs":  totalTasks * 30, // 30s per task orchestrated
				"avg_confidence_vs_baseline": fmt.Sprintf("%.2f", avgConf),
				"efficiency_score":           fmt.Sprintf("%.2f", 0.8+avgConf*0.1),
			}, nil
		},
	}

	registry.Register(s)
}
