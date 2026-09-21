package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestMandatoryHumanApprovalGate_SubmitDecisionChoices(t *testing.T) {
	tmpDir := t.TempDir()
	logger, _ := NewLogger(tmpDir, nil)
	defer logger.Close()
	engine := &DirectedEngine{
		graphs:    make(map[string]*schemas.TaskGraph),
		logger:    logger,
		doneChans: make(map[string]chan struct{}),
	}

	graph := &schemas.TaskGraph{
		TaskID: "task_human_1",
		Status: schemas.GraphBlocked,
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:                   "s1",
				Task:                 "Original High-Risk Deployment Task",
				Status:               schemas.StepBlocked,
				RequireHumanApproval: true,
			},
		},
		PendingDecisions: []*schemas.Decision{
			{
				ID:     "dec_human_1",
				StepID: "s1",
				Type:   schemas.DecisionHumanApprovalRequired,
			},
		},
	}
	engine.graphs["task_human_1"] = graph
	engine.doneChans["task_human_1"] = make(chan struct{})

	// 1. Test "approve"
	err := engine.SubmitDecision("task_human_1", "dec_human_1", "approve")
	if err != nil {
		t.Fatalf("SubmitDecision approve failed: %v", err)
	}
	if graph.Steps["s1"].Status != schemas.StepOK {
		t.Errorf("Expected s1 status to be StepOK, got %s", graph.Steps["s1"].Status)
	}

	// Reset for "reject"
	graph.Steps["s1"].Status = schemas.StepBlocked
	graph.PendingDecisions = []*schemas.Decision{
		{
			ID:     "dec_human_2",
			StepID: "s1",
			Type:   schemas.DecisionHumanApprovalRequired,
		},
	}
	// 2. Test "reject"
	err = engine.SubmitDecision("task_human_1", "dec_human_2", "reject")
	if err != nil {
		t.Fatalf("SubmitDecision reject failed: %v", err)
	}
	if graph.Steps["s1"].Status != schemas.StepFailed || graph.Steps["s1"].LastError != "human_rejected" {
		t.Errorf("Expected s1 status to be StepFailed with last_error human_rejected, got status=%s, err=%s", graph.Steps["s1"].Status, graph.Steps["s1"].LastError)
	}

	// Reset for "modify_and_resume"
	graph.Steps["s1"].Status = schemas.StepBlocked
	graph.PendingDecisions = []*schemas.Decision{
		{
			ID:     "dec_human_3",
			StepID: "s1",
			Type:   schemas.DecisionHumanApprovalRequired,
		},
	}
	// 3. Test "modify_and_resume"
	payload := `{"task":"Updated Approved Deployment Task"}`
	err = engine.SubmitDecisionWithPayload("task_human_1", "dec_human_3", "modify_and_resume", payload)
	if err != nil {
		t.Fatalf("SubmitDecision modify_and_resume failed: %v", err)
	}
	if graph.Steps["s1"].Status != schemas.StepPending || graph.Steps["s1"].Task != "Updated Approved Deployment Task" {
		t.Errorf("Expected s1 status to be StepPending and text updated, got status=%s, task=%s", graph.Steps["s1"].Status, graph.Steps["s1"].Task)
	}
}

func TestToolRouter_PrunesDecisionSubmission(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: []string{"tool_a", "orchestrator_submit_decision", "tool_b"},
			},
		},
	}
	router := NewToolRouter(reg, nil)

	req := RouteRequest{
		Task: "run some operations",
		Bindings: []config.MCPBinding{
			{
				MCPID: "mcp1",
			},
		},
	}

	routed := router.Route(req)
	if len(routed) != 1 {
		t.Fatalf("Expected 1 binding, got %d", len(routed))
	}

	for _, tool := range routed[0].AllowedTools {
		if tool == "orchestrator_submit_decision" {
			t.Fatalf("Security breach: subagent router exposed orchestrator_submit_decision!")
		}
	}
}

func TestSQLiteTaskBackend_ImmutabilityValidation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "sqlite_task_immutability_test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test_tasks.db")
	db, err := store.InitDB(dbPath)
	if err != nil {
		t.Fatalf("store.InitDB failed: %v", err)
	}
	defer db.Close()

	backend := store.NewSQLiteTaskBackend(db)
	ctx := context.Background()

	// Initial valid graph with require_human_approval = true
	g1 := &schemas.TaskGraph{
		TaskID: "task_immutable_1",
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:                   "s1",
				RequireHumanApproval: true,
			},
		},
	}

	g1Data, _ := json.Marshal(g1)
	err = backend.SaveTask(ctx, g1.TaskID, string(g1.Status), g1Data)
	if err != nil {
		t.Fatalf("Initial SaveTask failed: %v", err)
	}

	// Tampered version attempting to clear require_human_approval = false
	g2 := &schemas.TaskGraph{
		TaskID: "task_immutable_1",
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:                   "s1",
				RequireHumanApproval: false,
			},
		},
	}

	g2Data, _ := json.Marshal(g2)
	err = backend.SaveTask(ctx, g2.TaskID, string(g2.Status), g2Data)
	if err == nil {
		t.Fatalf("Security bypass: expected SaveTask to block clearing require_human_approval, but it succeeded!")
	}

	if err.Error() != "immutable field violation: require_human_approval cannot be cleared" {
		t.Errorf("Unexpected error message: %v", err.Error())
	}
}
