package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestMatchesTrigger(t *testing.T) {
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "video_generation", Task: "render"},
			"s2": {ID: "s2", RoleID: "code_review", Task: "review"},
		},
	}

	tests := []struct {
		name      string
		route     config.NotificationRoute
		eventType EventType
		want      bool
	}{
		{
			name:      "empty_task_type_matches_all",
			route:     config.NotificationRoute{TriggerOn: []string{"task_completed"}, TaskType: ""},
			eventType: EventTaskCompleted,
			want:      true,
		},
		{
			name:      "wildcard_task_type_matches_all",
			route:     config.NotificationRoute{TriggerOn: []string{"task_completed"}, TaskType: "*"},
			eventType: EventTaskCompleted,
			want:      true,
		},
		{
			name:      "matching_task_type",
			route:     config.NotificationRoute{TriggerOn: []string{"task_completed"}, TaskType: "video_generation"},
			eventType: EventTaskCompleted,
			want:      true,
		},
		{
			name:      "non_matching_task_type",
			route:     config.NotificationRoute{TriggerOn: []string{"task_completed"}, TaskType: "nonexistent"},
			eventType: EventTaskCompleted,
			want:      false,
		},
		{
			name:      "non_matching_event",
			route:     config.NotificationRoute{TriggerOn: []string{"task_completed"}, TaskType: ""},
			eventType: EventTaskFailed,
			want:      false,
		},
		{
			name:      "multiple_triggers_match",
			route:     config.NotificationRoute{TriggerOn: []string{"task_completed", "task_failed"}, TaskType: ""},
			eventType: EventTaskFailed,
			want:      true,
		},
		{
			name:      "empty_trigger_list",
			route:     config.NotificationRoute{TriggerOn: []string{}, TaskType: ""},
			eventType: EventTaskCompleted,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesTrigger(tt.route, tt.eventType, graph)
			if got != tt.want {
				t.Errorf("matchesTrigger() = %v, want %v", got, tt.want)
			}
		})
	}
}
