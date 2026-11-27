package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// O14 extraction regression: prompt assembly must retain the C2 security gate
// when moved out of executeStep into buildStepTaskText.
func TestDAGExecExtraction_PartialOutputSecurityGate(t *testing.T) {
	engine := &DirectedEngine{}
	graph := &schemas.TaskGraph{TaskID: "task-o14", OwnerID: "tenant_alice", Steps: map[string]*schemas.Step{}}
	dep := &schemas.Step{ID: "up", PartialOutput: "owner:tenant_bob secret", LastError: "prompt_injection"}
	graph.Steps[dep.ID] = dep
	step := &schemas.Step{ID: "down", Task: "work", DependsOn: []string{"up"}}

	got := engine.buildStepTaskText(context.Background(), graph, step, graph.TaskID)
	if got != "work" {
		t.Fatalf("security-class partial output must not be passed through, got %q", got)
	}

	dep.LastError = "repetition"
	got = engine.buildStepTaskText(context.Background(), graph, step, graph.TaskID)
	if !strings.Contains(got, "<untrusted_data") || !strings.Contains(got, "[OTHER_TENANT:REDACTED]") {
		t.Fatalf("non-security partial output must stay wrapped and redacted, got %q", got)
	}
}

// O14 extraction regression: automatic upstream context remains JSON, bounded,
// and artifact-linked after prompt assembly moves to its own helper.
func TestDAGExecExtraction_UpstreamContextContract(t *testing.T) {
	ts := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	engine := &DirectedEngine{taskStore: ts}
	graph := &schemas.TaskGraph{TaskID: "task-o14-upstream", Steps: map[string]*schemas.Step{
		"up": {ID: "up", Status: schemas.StepOK},
	}}
	step := &schemas.Step{ID: "down", Task: "work", DependsOn: []string{"up"}}
	long := strings.Repeat("x", 1400)
	if err := ts.Set(context.Background(), graph.TaskID, "up", &store.StepResult{Data: map[string]any{"value": long}}); err != nil {
		t.Fatal(err)
	}

	got := engine.buildStepTaskText(context.Background(), graph, step, graph.TaskID)
	for _, want := range []string{`"value":"`, "...(truncated)", filepath.Join("outputs", "task-o14-upstream", "up") + "/", "do not re-fetch"} {
		if !strings.Contains(got, want) {
			t.Errorf("upstream context missing %q", want)
		}
	}
}

// O14 extraction regression: rate-limit retry accounting and provider cooldown
// stay intact in the error helper, including cancellation before the wait.
func TestDAGExecExtraction_RateLimitRetryState(t *testing.T) {
	logger, err := obs.NewLogger(t.TempDir(), &config.SystemSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	engine := &DirectedEngine{logger: logger, registry: &config.Registry{}}
	graph := &schemas.TaskGraph{TaskID: "task-o14-rate"}
	step := &schemas.Step{ID: "s1", ProviderID: "provider-a", RetryCount: 0, MaxRetries: 2}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	action := engine.handleStepSpawnError(ctx, graph, step, nil, &providers.RateLimitError{Msg: "busy", RetryAfter: 1})
	if action != stepErrorReturn {
		t.Fatalf("cancelled retry should return, got %v", action)
	}
	if step.RetryCount != 1 {
		t.Fatalf("retry count = %d, want 1", step.RetryCount)
	}
	if cooling, _ := engine.registry.IsCoolingDown("provider-a"); !cooling {
		t.Fatal("provider cooldown was not recorded")
	}
}

// O14 extraction regression: the traced context created during request
// assembly must flow back to executeStep for output handling and verification.
func TestDAGExecExtraction_RequestReturnsTracedContext(t *testing.T) {
	type contextKey struct{}
	base := context.WithValue(context.Background(), contextKey{}, "preserved")
	engine := &DirectedEngine{
		registry:   &config.Registry{},
		outputBase: t.TempDir(),
		graphs:     map[string]*schemas.TaskGraph{},
	}
	graph := &schemas.TaskGraph{TaskID: "task-o14-trace", SubmitterIsAdmin: true, Steps: map[string]*schemas.Step{}}
	step := &schemas.Step{ID: "s1", Task: "work"}

	req, traced := engine.buildStepSpawnRequest(base, graph, step, nil)
	if !req.SubmitterIsAdmin {
		t.Fatal("request assembly lost the owning graph's submitter tier")
	}
	if req.TraceID == "" || req.SpanID == "" {
		t.Fatalf("missing trace identifiers: trace=%q span=%q", req.TraceID, req.SpanID)
	}
	if traced == base || traced.Value(contextKey{}) != "preserved" {
		t.Fatal("request assembly did not derive and return the traced context")
	}
}

// O14 extraction regression: exhausting the Sieve retry allowance must return
// to executeStep's failure finalizer, not silently finish as a successful step.
func TestDAGExecExtraction_GuardianExhaustionReturnsFailureAction(t *testing.T) {
	logger, err := obs.NewLogger(t.TempDir(), &config.SystemSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	engine := &DirectedEngine{logger: logger, sieve: sieve.NewSieve(10)}
	graph := &schemas.TaskGraph{TaskID: "task-o14-guardian"}
	step := &schemas.Step{ID: "s1", MaxRetries: 0}
	repeated := strings.Repeat("This is the same repeated sentence number one.\n", 6)
	result := &SpawnResult{Output: schemas.SubagentOutput{Result: map[string]any{"text": repeated}}}

	action := engine.finishSuccessfulStep(context.Background(), graph, step, result, nil, nil)
	if action != stepSuccessExhausted {
		t.Fatalf("exhausted Guardian retry action = %v, want stepSuccessExhausted", action)
	}
	if step.RetryCount != 1 || !strings.HasPrefix(step.LastError, "sieve_intercepted:") {
		t.Fatalf("Guardian failure state not preserved: retries=%d error=%q", step.RetryCount, step.LastError)
	}
}
