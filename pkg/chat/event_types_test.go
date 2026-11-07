package chat

import "testing"

func TestIsTerminalEvent(t *testing.T) {
	tests := []struct {
		evType      string
		waitForTask bool
		want        bool
	}{
		{EvSlashCommand, false, true},
		{EvSlashCommand, true, true},
		{EvError, false, true},
		{EvError, true, true},
		{EvTaskCallback, false, true},
		{EvTaskCallback, true, true},
		{EvDone, false, true},
		{EvDone, true, false},
		{EvDelta, false, false},
		{EvDelta, true, false},
		{EvThinking, false, false},
		{EvToolCall, false, false},
		{EvToolResult, false, false},
		{EvHeartbeat, false, false},
		{EvDecisionNeeded, false, false},
		{EvSpeech, false, false},
		{EvAction, false, false},
		{EvArtifact, false, false},
		{EvDecision, false, false},
		{EvProgress, false, false},
		{"unknown_type", false, false},
	}
	for _, tt := range tests {
		got := IsTerminalEvent(tt.evType, tt.waitForTask)
		if got != tt.want {
			t.Errorf("IsTerminalEvent(%q, waitForTask=%v) = %v, want %v",
				tt.evType, tt.waitForTask, got, tt.want)
		}
	}
}

func TestEventConstants_DistinctFromLiterals(t *testing.T) {
	all := []string{
		EvDelta, EvThinking, EvToolCall, EvToolResult, EvToolError,
		EvDone, EvError, EvSlashCommand, EvSteer, EvLoopGuard,
		EvPlanDrift, EvTruncationViolation, EvResultRepetition,
		EvRepeatedWrite, EvFollowupTurn, EvHookWarning, EvHookAbort,
		EvHeartbeat, EvDecisionNeeded, EvTaskCallback,
		EvArtifactsComplete, EvArtifactsIncomplete, EvCompletionCheck,
		EvSpeech, EvAction, EvArtifact, EvDecision, EvProgress,
	}
	seen := make(map[string]bool)
	for _, v := range all {
		if v == "" {
			t.Fatal("event constant is empty string")
		}
		if seen[v] {
			t.Fatalf("duplicate event constant: %q", v)
		}
		seen[v] = true
	}
	if len(all) != 28 {
		t.Errorf("expected 28 event constants, got %d", len(all))
	}
}
