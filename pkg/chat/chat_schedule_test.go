package chat

import (
	"context"
	"fmt"
	"testing"
)

func TestScheduleTools_NotPresentWhenScheduleExecNil(t *testing.T) {
	h := &ChatHarness{}
	defs := h.ToolDefinitions()
	for _, d := range defs {
		if d.Name == "schedule_list" || d.Name == "schedule_create" ||
			d.Name == "schedule_pause" || d.Name == "schedule_resume" ||
			d.Name == "schedule_delete" {
			t.Errorf("schedule tool %q should not be present when ScheduleExec is nil", d.Name)
		}
	}
}

func TestScheduleTools_PresentWhenScheduleExecSet(t *testing.T) {
	h := &ChatHarness{
		ScheduleExec: func(action string, args map[string]any) (string, error) {
			return "{}", nil
		},
	}
	defs := h.ToolDefinitions()
	want := map[string]bool{
		"schedule_list":   false,
		"schedule_create": false,
		"schedule_pause":  false,
		"schedule_resume": false,
		"schedule_delete": false,
	}
	for _, d := range defs {
		if _, ok := want[d.Name]; ok {
			want[d.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("schedule tool %q not found in ToolDefinitions", name)
		}
	}
}

func TestScheduleTools_DispatchCallsScheduleExec(t *testing.T) {
	var calledAction string
	var calledArgs map[string]any
	h := &ChatHarness{
		ScheduleExec: func(action string, args map[string]any) (string, error) {
			calledAction = action
			calledArgs = args
			return `{"ok":true}`, nil
		},
	}
	result, err := h.execTool(context.Background(), "schedule_list", map[string]any{}, "task_test")
	if err != nil {
		t.Fatalf("schedule_list: unexpected error: %v", err)
	}
	if calledAction != "list" {
		t.Errorf("called action = %q, want %q", calledAction, "list")
	}
	if result != `{"ok":true}` {
		t.Errorf("result = %q, want %q", result, `{"ok":true}`)
	}

	_, err = h.execTool(context.Background(), "schedule_pause",
		map[string]any{"schedule_id": "sch_abc"}, "task_test")
	if err != nil {
		t.Fatalf("schedule_pause: unexpected error: %v", err)
	}
	if calledAction != "pause" {
		t.Errorf("called action = %q, want %q", calledAction, "pause")
	}
	if calledArgs["schedule_id"] != "sch_abc" {
		t.Errorf("called args = %v, want schedule_id=sch_abc", calledArgs)
	}
}

func TestScheduleTools_DispatchAllActions(t *testing.T) {
	cases := []struct {
		toolName string
		wantAction string
	}{
		{"schedule_list", "list"},
		{"schedule_create", "create"},
		{"schedule_pause", "pause"},
		{"schedule_resume", "resume"},
		{"schedule_delete", "delete"},
	}
	for _, c := range cases {
		t.Run(c.toolName, func(t *testing.T) {
			var got string
			h := &ChatHarness{
				ScheduleExec: func(action string, args map[string]any) (string, error) {
					got = action
					return "{}", nil
				},
			}
			_, err := h.execTool(context.Background(), c.toolName, map[string]any{}, "task_test")
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", c.toolName, err)
			}
			if got != c.wantAction {
				t.Errorf("%s: action = %q, want %q", c.toolName, got, c.wantAction)
			}
		})
	}
}

func TestScheduleTools_ErrorFromScheduleExec(t *testing.T) {
	h := &ChatHarness{
		ScheduleExec: func(action string, args map[string]any) (string, error) {
			return "", fmt.Errorf("schedule %s failed: not found", action)
		},
	}
	_, err := h.execTool(context.Background(), "schedule_pause",
		map[string]any{"schedule_id": "sch_missing"}, "task_test")
	if err == nil {
		t.Fatal("expected error from ScheduleExec, got nil")
	}
	if err.Error() != "schedule pause failed: not found" {
		t.Errorf("error = %q, want %q", err.Error(), "schedule pause failed: not found")
	}
}

func TestScheduleTools_CreateSchemaHasRequiredFields(t *testing.T) {
	h := &ChatHarness{
		ScheduleExec: func(action string, args map[string]any) (string, error) { return "{}", nil },
	}
	defs := h.ToolDefinitions()
	var createDef *struct {
		Name       string
		InputSchema map[string]any
	}
	for _, d := range defs {
		if d.Name == "schedule_create" {
			createDef = &struct {
				Name       string
				InputSchema map[string]any
			}{d.Name, d.InputSchema}
			break
		}
	}
	if createDef == nil {
		t.Fatal("schedule_create tool not found")
	}
	required, _ := createDef.InputSchema["required"].([]string)
	wantRequired := map[string]bool{"name": false, "type": false, "task_inputs": false}
	for _, r := range required {
		if _, ok := wantRequired[r]; ok {
			wantRequired[r] = true
		}
	}
	for field, found := range wantRequired {
		if !found {
			t.Errorf("schedule_create: required field %q not found in schema", field)
		}
	}
}
