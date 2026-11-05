package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/daybeam/vortex/store"
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

	// Q5-1 (register O7): every internal construction site writes this telemetry
	// and NextPareto reads it, but there was no query face, so "what did the
	// router learn" had no answer. Read-only, as the ruling asked.
	s.Actions["get_capability_profile"] = Action{
		Name: "get_capability_profile",
		Description: "Read-only view of the routing telemetry recorded per (model, capability): runs, success rate, average turns, latency and token cost. " +
			"Optional model_id and capability narrow the query; capability is matched exactly against the recorded string, which is the role's base_capability. " +
			"theta reads 0 unless a batch recalculation ran -- Spawner.RecalculateThetas has no production caller, so 0 means 'never computed', not 'average ability'.",
		Parameters: map[string]any{"model_id": "string (optional)", "capability": "string (optional, exact match)"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			if app.CapProfile == nil {
				// An empty list here would read as "no runs recorded yet", which is a
				// different fact: this process never wired the routing that records them.
				return map[string]any{
					"wired":  false,
					"reason": "this process did not wire capability routing (it needs a SQLite DB), so it records no telemetry of its own",
				}, nil
			}
			modelID := strArg(args, "model_id")
			capability := strArg(args, "capability")
			var (
				profiles []store.ModelCapabilityProfile
				err      error
			)
			switch {
			case modelID != "" && capability != "":
				p, e := app.CapProfile.GetProfile(ctx, modelID, capability)
				err = e
				if p != nil {
					profiles = []store.ModelCapabilityProfile{*p}
				}
			case modelID != "":
				profiles, err = app.CapProfile.GetProfilesForModel(ctx, modelID)
			default:
				profiles, err = app.CapProfile.GetAllProfiles(ctx)
			}
			if err != nil {
				return nil, err
			}
			page, dropped := pageCapabilityProfiles(profiles, capabilityProfilePageLimit)
			out := map[string]any{"wired": true, "profile_count": len(profiles), "profiles": page}
			if dropped > 0 {
				out["truncated"] = dropped
			}
			return out, nil
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

// capabilityProfilePageLimit bounds how many rows an unfiltered query can drop
// into a caller's context. Ordering is most-runs-first, so the cap sheds the
// least-evidenced profiles and the response says how many it shed.
const capabilityProfilePageLimit = 50

func pageCapabilityProfiles(profiles []store.ModelCapabilityProfile, limit int) ([]store.ModelCapabilityProfile, int) {
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].TotalRuns > profiles[j].TotalRuns })
	if len(profiles) <= limit {
		return profiles, 0
	}
	return profiles[:limit], len(profiles) - limit
}
