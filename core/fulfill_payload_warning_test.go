package core

import (
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// Regression for O53 (docs/audit/o-dead-gates-52-53.md): delegation fulfill
// must expose payload-local criterion mismatches without running engine-side
// file/command checks or turning the host's fulfillment into a retry.
func TestFulfillPayloadMismatch_PayloadLocalOnly(t *testing.T) {
	structured := schemas.SubagentOutput{Result: map[string]any{"answer": "HELLO"}}
	cases := []struct {
		name      string
		criteria  string
		output    schemas.SubagentOutput
		wantWarn  bool
		wantToken string
	}{
		{"regex_mismatch", "regex:GOODBYE", structured, true, "GOODBYE"},
		{"regex_match", "regex:HELLO", structured, false, ""},
		{"invalid_regex", "regex:[", structured, true, "invalid regex"},
		{"json_structured", "json", structured, false, ""},
		{"json_raw_mismatch", "json", schemas.SubagentOutput{Result: map[string]any{"raw": "not json"}}, true, "not valid JSON"},
		{"json_raw_match", "json", schemas.SubagentOutput{Result: map[string]any{"raw": `{"ok":true}`}}, false, ""},
		{"substring_mismatch", "goodbye", structured, true, "goodbye"},
		{"substring_match", "hello", structured, false, ""},
		{"file_not_executed", "file:/definitely/not/an/engine/artifact", structured, false, ""},
		{"empty", "", structured, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fulfillPayloadMismatch(tc.output, tc.criteria)
			if (got != "") != tc.wantWarn {
				t.Fatalf("warning = %q, want warning=%v", got, tc.wantWarn)
			}
			if got != "" && !strings.Contains(got, tc.wantToken) {
				t.Errorf("warning %q does not name %q", got, tc.wantToken)
			}
		})
	}
}

func TestFulfillStep_PayloadMismatchWarnsButCompletes(t *testing.T) {
	engine := newDelegationTestEngine(t)
	events := engine.logger.SubscribeEvents()
	defer engine.logger.UnsubscribeEvents(events)

	taskID, err := engine.Submit([]schemas.StepInput{{
		ID: "step_1", RoleID: "worker", Task: "Return HELLO.",
		ExitCriteria: "regex:GOODBYE",
		DeterministicChecks: []schemas.DeterministicCheck{{
			Type: "command_pass", Params: map[string]any{"command": "exit 99"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	dec := waitForDelegationDecision(t, engine, taskID)
	if err := engine.FulfillStep(taskID, dec.ID, `{"status":"ok","confidence":0.95,"result":{"answer":"HELLO"}}`); err != nil {
		t.Fatal(err)
	}

	step := engine.GetTaskGraph(taskID).Steps["step_1"]
	if step.Status != schemas.StepOK || step.LastError != "" || step.AutoRefineCount != 0 {
		t.Fatalf("warning changed fulfillment outcome: status=%s error=%q refine=%d", step.Status, step.LastError, step.AutoRefineCount)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Event != "fulfill_payload_mismatch" || ev.TaskID != taskID {
				continue
			}
			if ev.Detail["criterion"] != "regex:GOODBYE" || ev.StepID != "step_1" {
				t.Fatalf("warning detail = %#v", ev.Detail)
			}
			if !strings.Contains(ev.Detail["warning"].(string), "fulfill_payload_mismatch") {
				t.Fatalf("warning = %#v", ev.Detail["warning"])
			}
			return
		case <-deadline:
			t.Fatal("no fulfill_payload_mismatch event")
		}
	}
}

// Regression for the O53 warning-event deadlock: synchronous subscribers may
// enrich webhook events with GetStatus, so publishing under s.Mu re-enters it.
func TestFulfillStep_PayloadMismatchSubscriberCanReadStatus(t *testing.T) {
	engine := newDelegationTestEngine(t)
	taskID, err := engine.Submit([]schemas.StepInput{{
		ID: "step_1", RoleID: "worker", Task: "Return HELLO.",
		ExitCriteria: "regex:GOODBYE",
	}})
	if err != nil {
		t.Fatal(err)
	}
	dec := waitForDelegationDecision(t, engine, taskID)

	unsubscribe := obs.DefaultBus.SubscribeWithCancel("fulfill_payload_mismatch", func(ev obs.AgentEvent) error {
		if ev.TaskID == taskID {
			engine.GetStatus(taskID, "full")
		}
		return nil
	})
	defer unsubscribe()

	done := make(chan error, 1)
	go func() {
		done <- engine.FulfillStep(taskID, dec.ID, `{"status":"ok","confidence":0.95,"result":{"answer":"HELLO"}}`)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FulfillStep: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fulfillment deadlocked while a warning subscriber read task status")
	}
}
