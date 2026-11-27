package chat

import "github.com/daybeam/vortex/schemas"

func scheduleToolDefs() []schemas.ToolDefinition {
	return []schemas.ToolDefinition{
		{
			Name:        "schedule_list",
			Description: "List all scheduled tasks. Returns each schedule's id, name, type, status, next run time, and run count.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name: "schedule_create",
			Description: "Create a new scheduled task. The schedule will automatically submit an orchestrator task " +
				"at the specified time or interval. Use type='once' for a one-time future run, 'interval' for " +
				"recurring runs at a fixed duration, or 'cron' for cron-expression-based scheduling.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Human-readable name for this schedule.",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"once", "interval", "cron"},
						"description": "Scheduling type: 'once' runs at a specific time, 'interval' runs repeatedly at a fixed duration, 'cron' uses a cron expression.",
					},
					"run_at": map[string]any{
						"type":        "string",
						"description": "ISO8601 timestamp for 'once' type, e.g. '2026-12-01T09:00:00Z'. Required when type='once'.",
					},
					"interval": map[string]any{
						"type":        "string",
						"description": "Duration for 'interval' type, e.g. '1h', '30m', '6h'. Required when type='interval'.",
					},
					"cron_expr": map[string]any{
						"type":        "string",
						"description": "5-field cron expression for 'cron' type, e.g. '0 9 * * MON-FRI'. Required when type='cron'.",
					},
					"task_inputs": map[string]any{
						"type":        "array",
						"description": "Step definitions for the task to run. Each step has a 'task' (the prompt) and optional 'role_id'.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"task": map[string]any{
									"type":        "string",
									"description": "The task description / prompt for this step.",
								},
								"role_id": map[string]any{
									"type":        "string",
									"description": "Optional role to assign to this step.",
								},
							},
							"required": []string{"task"},
						},
					},
					"max_runs": map[string]any{
						"type":        "number",
						"description": "Maximum number of runs (0 = unlimited).",
					},
					"on_failure": map[string]any{
						"type":        "string",
						"enum":        []string{"ignore", "retry_next_window", "alert"},
						"description": "What to do when a scheduled run fails.",
					},
				},
				"required": []string{"name", "type", "task_inputs"},
			},
		},
		{
			Name:        "schedule_pause",
			Description: "Pause an active schedule. It will not run until resumed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"schedule_id": map[string]any{
						"type":        "string",
						"description": "The schedule id (form: sch_xxxx).",
					},
				},
				"required": []string{"schedule_id"},
			},
		},
		{
			Name:        "schedule_resume",
			Description: "Resume a paused schedule. The next run is recalculated from the current time.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"schedule_id": map[string]any{
						"type":        "string",
						"description": "The schedule id (form: sch_xxxx).",
					},
				},
				"required": []string{"schedule_id"},
			},
		},
		{
			Name:        "schedule_delete",
			Description: "Delete a schedule permanently.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"schedule_id": map[string]any{
						"type":        "string",
						"description": "The schedule id (form: sch_xxxx).",
					},
				},
				"required": []string{"schedule_id"},
			},
		},
	}
}
