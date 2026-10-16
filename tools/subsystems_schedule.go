package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

func registerScheduleSubsystem(app *App) {
	s := &Subsystem{
		Name:        "schedule",
		Description: "Task scheduling and automation.",
		Actions:     make(map[string]Action),
	}

	s.Actions["list"] = Action{
		Name:        "list",
		Description: "List all schedules.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return app.SStore.List(), nil
		},
	}

	s.Actions["create"] = Action{
		Name:        "create",
		Description: "Create a new schedule for a task.",
		Parameters: map[string]any{
			"name":        "string (required)",
			"type":        "string (required) - once|interval|cron",
			"run_at":      "string (optional) - ISO8601 for 'once'",
			"interval":    "string (optional) - duration like '1h', '30m' for 'interval'",
			"cron_expr":   "string (optional) - cron expression for 'cron'",
			"task_inputs": "[]object (required) - step definitions",
			"max_runs":    "int (optional)",
			"on_failure":  "string (optional) - ignore|retry_next_window|alert",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			data, err := json.Marshal(args)
			if err != nil {
				return nil, err
			}
			var req struct {
				Name       string                  `json:"name"`
				Type       schemas.ScheduleType    `json:"type"`
				RunAt      *time.Time              `json:"run_at"`
				Interval   string                  `json:"interval"`
				CronExpr   string                  `json:"cron_expr"`
				TaskInputs []schemas.StepInput     `json:"task_inputs"`
				MaxRuns    int                     `json:"max_runs"`
				OnFailure  schemas.OnFailurePolicy `json:"on_failure"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return nil, err
			}

			if req.Name == "" || req.Type == "" || len(req.TaskInputs) == 0 {
				return nil, fmt.Errorf("missing name, type, or task_inputs")
			}

			sch := &schemas.Schedule{
				ID:         fmt.Sprintf("sch_%s", uuid.New().String()[:6]),
				Name:       req.Name,
				Type:       req.Type,
				RunAt:      req.RunAt,
				Interval:   req.Interval,
				CronExpr:   req.CronExpr,
				TaskInputs: req.TaskInputs,
				Status:     schemas.ScheduleActive,
				MaxRuns:    req.MaxRuns,
				OnFailure:  req.OnFailure,
				CreatedAt:  time.Now(),
			}

			// Initial NextRun calculation is handled by ScheduleManager.scan
			// but we can set it now to be safe.
			// We can't easily call ScheduleManager.calculateNextRun here due to circular deps
			// (App -> ScheduleManager -> Scheduler -> App).
			// Instead, we just set it to Now to trigger it on the next scan, or let scan handle it.
			sch.NextRun = time.Now()
			if sch.Type == schemas.ScheduleOnce && sch.RunAt != nil {
				sch.NextRun = *sch.RunAt
			}

			app.SStore.Set(sch)
			return map[string]any{"ok": true, "schedule_id": sch.ID}, nil
		},
	}

	s.Actions["pause"] = Action{
		Name:        "pause",
		Description: "Pause a schedule.",
		Parameters:  map[string]any{"schedule_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "schedule_id")
			sch := app.SStore.Get(id)
			if sch == nil {
				return nil, fmt.Errorf("not found")
			}
			sch.Status = schemas.SchedulePaused
			app.SStore.Set(sch)
			return map[string]any{"ok": true}, nil
		},
	}

	s.Actions["delete"] = Action{
		Name:        "delete",
		Description: "Delete a schedule.",
		Parameters:  map[string]any{"schedule_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "schedule_id")
			app.SStore.Mu.Lock()
			delete(app.SStore.Schedules, id)
			app.SStore.Mu.Unlock()
			app.SStore.Save()
			return map[string]any{"ok": true}, nil
		},
	}

	s.Actions["reload"] = Action{
		Name:        "reload",
		Description: "Reload configuration from disk and clear provider caches.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			providers.ClearCache()
			if err := app.Registry.Load(); err != nil {
				return nil, fmt.Errorf("failed to reload registry: %w", err)
			}
			return map[string]any{"ok": true}, nil
		},
	}

	registry.Register(s)
}
