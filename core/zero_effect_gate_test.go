package core

import "testing"

// TestIsReadOnlyToolName pins the classifier behind the zero-effect completion
// gate (ADDED 2026-10-21, eval v14).
//
// Why this test exists at all: the gate asks "could anything this step did have
// changed state?" and the only input is tool names. Getting the classification
// wrong in the permissive direction silently disables the gate (it never fires
// on the tasks it was built for); wrong in the strict direction it fires on
// real work and the model gets told to redo what it already did. Both failure
// modes were observed while building this — see the note on download_ below.
func TestIsReadOnlyToolName(t *testing.T) {
	readOnly := []string{
		// The exact trace of AppWorld task d4e9306_1 in 4/5 runs.
		"supervisor__show_profile",
		"supervisor__show_account_passwords",
		"supervisor__show_active_task",
		"spotify__show_album", "spotify__search_songs", "spotify__get_song",
		"venmo__search_payments", "venmo__list_transactions",
		"gmail__fetch_emails", "amazon__count_orders",
		// Core tools classified by the same rule.
		"read_file",
	}
	for _, name := range readOnly {
		if !IsReadOnlyToolName(name) {
			t.Errorf("%s must be read-only", name)
		}
	}

	notReadOnly := []string{
		// Real AppWorld writes that IsWriteMCPTool ALSO misses (the reason this
		// gate uses a read-only whitelist instead of the write-verb blacklist).
		"spotify__download_song",
		"venmo__remind_payment_request",
		"venmo__deny_payment_request",
		"spotify__unfollow_artist",
		"spotify__unlike_song",
		"amazon__place_order",
		"file_system__move_file",
		// Explicitly must not be treated as "the work is done".
		"supervisor__complete_task",
		// Core tools with effects.
		"write_file", "execute_code",
		// Unknown verb → assume it might mutate.
		"frobnicate_widget",
	}
	for _, name := range notReadOnly {
		if IsReadOnlyToolName(name) {
			t.Errorf("%s must NOT be read-only (gate would be suppressed or misfire)", name)
		}
	}

	if IsReadOnlyToolName("") {
		t.Error("empty name must not be read-only")
	}
}

// TestZeroEffectGateTraceVerdict replays the trace shapes actually observed in
// the AppWorld runs through the same reduction the gate performs.
func TestZeroEffectGateTraceVerdict(t *testing.T) {
	// hasEffect mirrors the loop in spawner_dospawn.go: any non-read-only call
	// counts as having done something.
	hasEffect := func(tools []string) bool {
		for _, name := range tools {
			if !IsReadOnlyToolName(name) {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name  string
		tools []string
		want  bool
	}{
		{"d4e9306_1 (observed 4/5 runs, then claimed ok at 0.90)", []string{
			"supervisor__show_profile",
			"supervisor__show_account_passwords",
		}, false},
		{"d4e9306_1 after a real follow", []string{
			"supervisor__show_profile",
			"spotify__follow_artist",
			"supervisor__complete_task",
		}, true},
		{"3ab5b8b_1 (download all liked songs)", []string{
			"supervisor__show_profile",
			"spotify__show_playlist",
			"spotify__download_song",
		}, true},
		{"23cf851_3 (pure count question)", []string{
			"venmo__show_transactions",
			"venmo__show_profile",
		}, false},
		{"empty trace (handled by noToolCallsYet, not this gate)", nil, false},
	}
	for _, c := range cases {
		if got := hasEffect(c.tools); got != c.want {
			t.Errorf("%s: hasEffect=%v, want %v", c.name, got, c.want)
		}
	}
}
