package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// acceptAllFilter stands in for a bloom filter: Test returns true for names it
// was never given, which is exactly why the guard has to fail open on it.
type acceptAllFilter struct{}

func (acceptAllFilter) Add(string)         {}
func (acceptAllFilter) Test(string) bool   { return true }
func (acceptAllFilter) Delete(string) bool { return false }

func appworldMCPDef() *config.MCPDef {
	return &config.MCPDef{
		ID: "appworld",
		AvailableTools: []string{
			"supervisor__complete_task",
			"supervisor__show_profile",
			"venmo__login",
			"venmo__show_sent_payment_requests",
			"venmo__remind_payment_request",
			"venmo__search_friends",
			"phone__get_current_date_and_time",
		},
	}
}

// TestUnknownToolFeedback_KnownToolIsForwarded: the guard must not intercept a
// call the server actually serves.
func TestUnknownToolFeedback_KnownToolIsForwarded(t *testing.T) {
	var s Spawner
	if msg := s.unknownToolFeedback("venmo__login", "appworld", appworldMCPDef()); msg != "" {
		t.Fatalf("known tool must be forwarded, got: %s", msg)
	}
}

// TestUnknownToolFeedback_PhantomIsReported reproduces the AppWorld run of
// 2026-10-19: the role instruction lists nine apps, the binding exposed two, and
// the agent called app__login names for the seven it did not have. Every call
// came back empty because ResolveToolMCP's Tier 3 forwarded a nonexistent name.
func TestUnknownToolFeedback_PhantomIsReported(t *testing.T) {
	var s Spawner
	msg := s.unknownToolFeedback("splitwise__login", "appworld", appworldMCPDef())
	if msg == "" {
		t.Fatal("a tool absent from the catalog must not be forwarded")
	}
	if !strings.Contains(msg, "splitwise__login") {
		t.Errorf("message must name the attempted tool so the model can stop retrying it: %s", msg)
	}
	if !strings.Contains(msg, "Calling it again cannot succeed") {
		t.Errorf("message must forbid the retry explicitly: %s", msg)
	}
	// "login" appears in venmo__login, so a verb-token match should surface it.
	if !strings.Contains(msg, "venmo__login") {
		t.Errorf("message should suggest a near miss the model can actually use: %s", msg)
	}
}

// TestUnknownToolFeedback_SuggestsSameNamespaceFirst: an invented tool in a
// namespace that exists should point at that namespace before anything else.
func TestUnknownToolFeedback_SuggestsSameNamespaceFirst(t *testing.T) {
	var s Spawner
	msg := s.unknownToolFeedback("venmo__remind_everyone", "appworld", appworldMCPDef())
	if msg == "" {
		t.Fatal("expected a rejection")
	}
	// Every suggestion offered must be a venmo tool, since that namespace exists.
	_, after, ok := strings.Cut(msg, "does have are: ")
	if !ok {
		t.Fatalf("expected a suggestion list, got: %s", msg)
	}
	suggestions := strings.Split(strings.SplitN(after, ".", 2)[0], ", ")
	for _, s := range suggestions {
		if !strings.HasPrefix(s, "venmo__") {
			t.Errorf("suggested %q from the wrong namespace; venmo tools exist and share the prefix", s)
		}
	}
}

// TestUnknownToolFeedback_ColdStartIsNotJudged: with no discovery yet, every
// name looks unknown. Silently refusing everything would be worse than
// forwarding and letting the server answer.
func TestUnknownToolFeedback_ColdStartIsNotJudged(t *testing.T) {
	var s Spawner
	mcp := &config.MCPDef{ID: "appworld"}
	if msg := s.unknownToolFeedback("venmo__login", "appworld", mcp); msg != "" {
		t.Fatalf("empty catalog must not trigger the guard, got: %s", msg)
	}
}

// TestUnknownToolFeedback_NilDefIsNotJudged covers the unresolvable-MCP path,
// which the caller already reports separately.
func TestUnknownToolFeedback_NilDefIsNotJudged(t *testing.T) {
	var s Spawner
	if msg := s.unknownToolFeedback("venmo__login", "appworld", nil); msg != "" {
		t.Fatalf("nil def must not trigger the guard, got: %s", msg)
	}
}

// TestUnknownToolFeedback_FilterAcceptedNameIsForwarded: a bloom filter can
// report true for names it never held, and a filtered tool may still be real.
// A name the filter accepts is a cache gap, not a phantom — forward it.
func TestUnknownToolFeedback_FilterAcceptedNameIsForwarded(t *testing.T) {
	var s Spawner
	mcp := appworldMCPDef()
	mcp.ToolFilter = acceptAllFilter{}
	if msg := s.unknownToolFeedback("gmail__login", "appworld", mcp); msg != "" {
		t.Fatalf("filter-accepted name must be forwarded, got: %s", msg)
	}
}
