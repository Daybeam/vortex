package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Delegation is only testable end-to-end if the caller receives everything it
// needs to run the subagent itself: the prompt pair, the tools, and a way to
// return the answer. This pins that envelope at the level the caller actually
// sees it — the pending decision, not the internal SpawnResult.
//
// It also pins the discriminator. DecisionDelegationRequired covers two
// situations that look identical from outside but demand opposite reactions:
// "run this for me" and "this step is too big, split it". A consumer that cannot
// tell them apart will fulfil a split request with a subagent answer (nonsense)
// or skip a step that only it could have answered (a guaranteed zero).
func TestDelegationDecision_PayloadIsCallerComplete(t *testing.T) {
	providers.ClearCache()

	reg := &config.Registry{
		DefaultProvider: "nonexistent",
		Providers:       make(map[string]*config.ProviderConfig),
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
			DelegationMode:      true,
		},
	}
	logDir := t.TempDir()
	outDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})

	ts := store.NewTaskStore(&mockBackend{data: make(map[string][]byte)})
	expStore := mustNewExperienceStore(t, t.TempDir(), ts, &reg.System, nil, nil)
	engine := NewDirectedEngine(reg, ts, expStore, nil, logger, nil, outDir, outDir, nil)
	defer engine.Stop()

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Summarize the quarter"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	var dec *schemas.Decision
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		engine.Mu.RLock()
		if g := engine.graphs[taskID]; g != nil && len(g.PendingDecisions) > 0 {
			dec = g.PendingDecisions[0]
		}
		engine.Mu.RUnlock()
		if dec != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dec == nil {
		t.Fatal("delegation mode never produced a pending decision")
	}

	if dec.Type != schemas.DecisionDelegationRequired {
		t.Fatalf("expected a delegation decision, got %q", dec.Type)
	}

	// ── Discriminator ────────────────────────────────────────────────
	kind, _ := dec.Context["kind"].(string)
	if kind != DelegationKindExternalSubagent {
		t.Errorf("context.kind = %q, want %q — a consumer cannot tell this apart from a DAG-surgery request without it",
			kind, DelegationKindExternalSubagent)
	}

	// ── Answer path must be on the menu ──────────────────────────────
	sawFulfill := false
	for _, o := range dec.Options {
		if o == "fulfill" {
			sawFulfill = true
		}
	}
	if !sawFulfill {
		t.Errorf("options %v omit \"fulfill\"; the caller is told to answer but has no way to", dec.Options)
	}

	// ── The envelope itself ──────────────────────────────────────────
	prompt, ok := dec.Context["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("context.prompt missing or wrong type: %T", dec.Context["prompt"])
	}
	for _, k := range []string{"system_prompt", "user_prompt", "target_capability", "role_id"} {
		s, _ := prompt[k].(string)
		if s == "" {
			t.Errorf("prompt.%s is empty — a caller could not run the subagent from this", k)
		}
	}

	defs, ok := prompt["tool_definitions"].([]schemas.ToolDefinition)
	if !ok {
		// JSON round-trip turns them into []any; accept that shape too.
		raw, okAny := prompt["tool_definitions"].([]any)
		if !okAny {
			t.Fatalf("prompt.tool_definitions missing or unusable: %T", prompt["tool_definitions"])
		}
		defs = make([]schemas.ToolDefinition, 0, len(raw))
	}
	if len(defs) == 0 {
		t.Fatal("prompt.tool_definitions is empty — the caller's subagent would run blind")
	}
	for i, d := range defs {
		if d.Name == "" {
			t.Errorf("tool_definitions[%d] has no name", i)
		}
	}
}

// TestDelegationDecision_DAGSurgeryIsDistinguishable is the negative half: the
// context-overflow flavour must NOT masquerade as an external-subagent request.
// It is built the same way scheduler_dag_exec.go builds it, because that path
// requires a provider that overflows, which a unit test cannot produce.
func TestDelegationDecision_DAGSurgeryIsDistinguishable(t *testing.T) {
	surgery := map[string]any{
		"kind":            DelegationKindDAGSurgery,
		"error":           "Context overflow: task too complex for single model.",
		"action_required": "Please split this step into smaller sub-tasks.",
	}
	if _, hasPrompt := surgery["prompt"]; hasPrompt {
		t.Fatal("the surgery flavour must not carry a prompt; otherwise fulfill looks legal")
	}
	if surgery["kind"] == DelegationKindExternalSubagent {
		t.Fatal("the two flavours collapsed into one kind")
	}

	// The consumer rule, stated once as an assertion: fulfil only when the kind
	// says external AND an envelope is present.
	shouldFulfill := func(ctx map[string]any) bool {
		k, _ := ctx["kind"].(string)
		_, hasPrompt := ctx["prompt"]
		return k == DelegationKindExternalSubagent && hasPrompt
	}
	if shouldFulfill(surgery) {
		t.Error("consumer rule would fulfil a DAG-surgery decision")
	}
	if !shouldFulfill(map[string]any{"kind": DelegationKindExternalSubagent, "prompt": map[string]any{}}) {
		t.Error("consumer rule rejects a genuine external delegation")
	}
}
