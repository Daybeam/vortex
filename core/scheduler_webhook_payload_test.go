package core

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_webhook_payload_test.go — D7 regression test
// (DELEGATION_HANDOVER_REDESIGN.md §4.7, webhook payload).
//
// Bug: addDecision's publishEvent payload only had decision_id/type/options.
// Webhook receivers knew a decision existed but not what prompt to execute.
// Fix: payload now includes "prompt" (JSON, truncated to 8KB) and "workspace".

// TestDelegationEvent_PayloadIncludesPromptAndWorkspace verifies that the
// EventDecisionRequired payload includes the prompt and workspace when a
// delegation decision is created.
func TestDelegationEvent_PayloadIncludesPromptAndWorkspace(t *testing.T) {
	var mu sync.Mutex
	var gotEvent AgentEvent
	var eventReceived bool

	unsub := DefaultBus.SubscribeWithCancel(string(EventDecisionRequired), func(ev AgentEvent) error {
		mu.Lock()
		defer mu.Unlock()
		gotEvent = ev
		eventReceived = true
		return nil
	})
	defer unsub()

	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Do work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for the delegation decision (and the event to fire).
	waitForDelegationDecision(t, engine, taskID)

	// Wait for the event to be received.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := eventReceived
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if !eventReceived {
		t.Fatalf("timed out waiting for EventDecisionRequired")
	}

	// Verify the payload includes "prompt" (truncated JSON string).
	prompt, ok := gotEvent.Payload["prompt"].(string)
	if !ok || prompt == "" {
		t.Fatalf("expected non-empty 'prompt' string in event payload, got %T %v",
			gotEvent.Payload["prompt"], gotEvent.Payload["prompt"])
	}
	if !strings.Contains(prompt, "system_prompt") {
		t.Fatalf("expected prompt JSON to contain 'system_prompt'")
	}
	// Verify truncation: prompt should be at most 8192 bytes.
	if len(prompt) > 8192 {
		t.Fatalf("expected prompt truncated to 8192 bytes, got %d", len(prompt))
	}

	// Verify the payload includes "workspace".
	ws, ok := gotEvent.Payload["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'workspace' map in event payload, got %T %v",
			gotEvent.Payload["workspace"], gotEvent.Payload["workspace"])
	}
	if _, ok := ws["task_output_dir"]; !ok {
		t.Fatalf("expected 'task_output_dir' in workspace")
	}
}
