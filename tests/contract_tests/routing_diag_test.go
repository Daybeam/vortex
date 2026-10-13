package contract_tests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
)

// Routing diagnostic system ("lightweight PluginEval"), see docs/ROUTING_DIAG.md for the
// full design writeup. This file is Layer 2 (execution collector) + Layer 3
// (report + gate) combined into one; Layer 1 (gold test cases) lives as JSON
// under ../routing_cases/.
//
// Unlike the rest of this package's tests (which exercise a self-contained toy
// domain model in domain_model.go), this file deliberately imports and calls the
// REAL production core.ToolRouter / core.SkillRouter directly. That is the whole
// point: core/tool_router.go has regressed on this exact "does the router select
// the right tool" question at least three separate times (F11, F15, and the
// 06-30-addendum's third regression), and had zero automated test coverage
// protecting it before this file -- every one of those regressions was only
// ever caught by a human re-running a manual live probe against production and
// re-verifying by hand each time. These tests turn those manual probes into a
// permanent, automated regression lock.
//
// Design decisions (see the design discussion this file implements):
//   - "missed calls" (a tool that should have been offered to the agent but
//     wasn't) is the priority signal, not "spurious calls" -- this project's
//     own history shows missed calls are the expensive failure mode (an agent
//     that never even sees take_screenshot in its tool list cannot succeed no
//     matter how good the model is), while a spurious extra tool is at worst
//     noise the model can ignore.
//   - "critical" severity cases gate the build (any failure = t.Fatalf); there
//     is deliberately NO global percentage/threshold knob. A blanket "missed
//     rate <= 20%" gate would conflate "a tool is completely unreachable" (must
//     be 100% caught) with "an edge-case phrasing is ambiguous" (fine to just
//     report). This project's own gating conventions elsewhere are always
//     concrete boolean assertions (a field is/isn't nil, a count is/isn't
//     zero), never a fuzzy aggregate score -- this file follows that same
//     discipline. "informational" severity cases are reported but never fail
//     the test.
//   - a "substring_trap" case type exists specifically to encode this project's
//     most expensive historical bug class (F11/F15/06-30's third regression:
//     the router's keyword-affinity matching falsely firing on a substring
//     collision like "gitnexus" containing "git", or "handle_dialog" containing
//     "log") as permanent, named regression tests, not just generic must_call
//     cases.

// ─── Layer 1 case schema (tests/routing_cases/*.json) ─────────────────────

type toolRouterCaseFile struct {
	Cases []toolRouterCase `json:"cases"`
}

type toolRouterCase struct {
	ID          string             `json:"id"`
	Type        string             `json:"type"` // must_call | confusion_negative | substring_trap
	Description string             `json:"description"`
	Task        string             `json:"task"`
	Bindings    []mcpFixture       `json:"bindings"`
	Assertions  []routingAssertion `json:"assertions"`
}

type mcpFixture struct {
	MCPID          string   `json:"mcp_id"`
	AvailableTools []string `json:"available_tools"`
	AllowedTools   []string `json:"allowed_tools"`
}

type routingAssertion struct {
	MCPID          string   `json:"mcp_id"`
	MustInclude    []string `json:"must_include,omitempty"`
	MustNotInclude []string `json:"must_not_include,omitempty"`
	Severity       string   `json:"severity"` // critical | informational
	Note           string   `json:"note,omitempty"`
}

type skillRouterCaseFile struct {
	Cases []skillRouterCase `json:"cases"`
}

type skillRouterCase struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Description    string   `json:"description"`
	Task           string   `json:"task"`
	Skills         []string `json:"skills"`
	GoldSkills     []string `json:"gold_skills,omitempty"`
	MustNotInclude []string `json:"must_not_include,omitempty"`
	Severity       string   `json:"severity"`
}

func loadToolRouterCases(t *testing.T) []toolRouterCase {
	t.Helper()
	path := filepath.Join("..", "routing_cases", "tool_router_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("routing diagnostic: case file %s not available: %v", path, err)
	}
	var f toolRouterCaseFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("routing diagnostic: cannot parse case file %s: %v", path, err)
	}
	if len(f.Cases) == 0 {
		t.Fatalf("routing diagnostic: %s declares zero cases", path)
	}
	return f.Cases
}

func loadSkillRouterCases(t *testing.T) []skillRouterCase {
	t.Helper()
	path := filepath.Join("..", "routing_cases", "skill_router_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("routing diagnostic: case file %s not available: %v", path, err)
	}
	var f skillRouterCaseFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("routing diagnostic: cannot parse case file %s: %v", path, err)
	}
	if len(f.Cases) == 0 {
		t.Fatalf("routing diagnostic: %s declares zero cases", path)
	}
	return f.Cases
}

// buildFixtureRegistry constructs a minimal, self-contained *config.Registry
// (no disk config.json involved) with exactly the MCP definitions declared by
// one test case's bindings. This deliberately does NOT depend on the real
// production config_windows.json staying stable -- these tests are locking in
// the ROUTING ALGORITHM's behavior, not any particular deployment's current
// role/MCP inventory.
func buildFixtureRegistry(bindings []mcpFixture) *config.Registry {
	reg := &config.Registry{
		MCPs: make(map[string]*config.MCPDef),
	}
	for _, b := range bindings {
		reg.MCPs[b.MCPID] = &config.MCPDef{
			ID:             b.MCPID,
			AvailableTools: b.AvailableTools,
		}
	}
	return reg
}

func toMCPBindings(fixtures []mcpFixture) []config.MCPBinding {
	out := make([]config.MCPBinding, 0, len(fixtures))
	for _, f := range fixtures {
		out = append(out, config.MCPBinding{MCPID: f.MCPID, AllowedTools: f.AllowedTools})
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestRoutingDiagnostic_ToolRouter is Layer 2+3 for core.ToolRouter.Route.
// Every "critical" assertion failure is reported via t.Errorf inside a
// t.Run subtest (so CI/`go test` output pinpoints exactly which case and
// which tool failed) AND additionally tracked so the summary at the end
// causes the overall test to fail if any critical case failed -- matching
// the design's "critical cases gate the build, no percentage threshold"
// requirement.
func TestRoutingDiagnostic_ToolRouter(t *testing.T) {
	cases := loadToolRouterCases(t)

	var criticalFailures []string
	var informationalMisses []string

	for _, c := range cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			reg := buildFixtureRegistry(c.Bindings)
			router := core.NewToolRouter(reg, nil)
			routed := router.Route(context.Background(), core.RouteRequest{
			Task:     c.Task,
			Bindings: toMCPBindings(c.Bindings),
		})

			routedByMCP := make(map[string][]string, len(routed))
			for _, b := range routed {
				routedByMCP[b.MCPID] = b.AllowedTools
			}

			for _, a := range c.Assertions {
				got := routedByMCP[a.MCPID]
				// An empty AllowedTools slice from Route() means "all tools
				// available" (wildcard) for an unrestricted binding -- resolve
				// against the fixture's AvailableTools for must_include checks
				// in that case, mirroring what a real caller downstream would
				// see as "usable tools", not the literal empty slice.
				effective := got
				if len(effective) == 0 {
					for _, b := range c.Bindings {
						if b.MCPID == a.MCPID && len(b.AllowedTools) == 0 {
							effective = b.AvailableTools
						}
					}
				}

				for _, want := range a.MustInclude {
					if !contains(effective, want) {
						msg := fmt.Sprintf("[%s] case %q: mcp %q missing expected tool %q (routed=%v)%s",
							a.Severity, c.ID, a.MCPID, want, got, noteSuffix(a.Note))
						if a.Severity == "critical" {
							t.Errorf("%s", msg)
							criticalFailures = append(criticalFailures, msg)
						} else {
							t.Logf("MISSED (informational): %s", msg)
							informationalMisses = append(informationalMisses, msg)
						}
					}
				}
				for _, forbidden := range a.MustNotInclude {
					if contains(effective, forbidden) {
						msg := fmt.Sprintf("[%s] case %q: mcp %q wrongly included forbidden tool %q (routed=%v)%s",
							a.Severity, c.ID, a.MCPID, forbidden, got, noteSuffix(a.Note))
						if a.Severity == "critical" {
							t.Errorf("%s", msg)
							criticalFailures = append(criticalFailures, msg)
						} else {
							t.Logf("SPURIOUS (informational): %s", msg)
						}
					}
				}
			}
		})
	}

	t.Logf("=== Routing Diagnostic Report: ToolRouter ===")
	t.Logf("Cases run: %d, Critical failures: %d, Informational misses: %d",
		len(cases), len(criticalFailures), len(informationalMisses))
	for _, m := range informationalMisses {
		t.Logf("  (informational, not gating) %s", m)
	}
	if len(criticalFailures) > 0 {
		t.Fatalf("routing diagnostic: %d critical failure(s), see subtests above for detail", len(criticalFailures))
	}
}

// TestRoutingDiagnostic_SkillRouter is Layer 2+3 for core.SkillRouter.Route.
func TestRoutingDiagnostic_SkillRouter(t *testing.T) {
	cases := loadSkillRouterCases(t)

	var criticalFailures []string

	for _, c := range cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			reg := &config.Registry{Skills: make(map[string]*config.Skill)}
			for _, id := range c.Skills {
				reg.Skills[id] = &config.Skill{ID: id}
			}
			router := core.NewSkillRouter(reg)
			routed := router.Route(core.SkillRouteRequest{
			Task:            c.Task,
			AvailableSkills: c.Skills,
		})

			for _, want := range c.GoldSkills {
				if !contains(routed, want) {
					msg := fmt.Sprintf("[%s] case %q: missing expected skill %q (routed=%v)", c.Severity, c.ID, want, routed)
					if c.Severity == "critical" {
						t.Errorf("%s", msg)
						criticalFailures = append(criticalFailures, msg)
					} else {
						t.Logf("MISSED (informational): %s", msg)
					}
				}
			}
			for _, forbidden := range c.MustNotInclude {
				if contains(routed, forbidden) {
					msg := fmt.Sprintf("[%s] case %q: wrongly included forbidden skill %q (routed=%v)", c.Severity, c.ID, forbidden, routed)
					if c.Severity == "critical" {
						t.Errorf("%s", msg)
						criticalFailures = append(criticalFailures, msg)
					} else {
						t.Logf("SPURIOUS (informational): %s", msg)
					}
				}
			}
		})
	}

	t.Logf("=== Routing Diagnostic Report: SkillRouter ===")
	t.Logf("Cases run: %d, Critical failures: %d", len(cases), len(criticalFailures))
	if len(criticalFailures) > 0 {
		t.Fatalf("routing diagnostic: %d critical failure(s), see subtests above for detail", len(criticalFailures))
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " -- " + strings.TrimSpace(note)
}
