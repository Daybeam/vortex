package core

import (
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// -----------------------------------------------------------------------------
// Regression test for audit CS-1 (2026-10-24): data race on
// graph.PendingDecisions in checkApprovalAuthority. The function released
// s.Mu.RLock() before iterating graph.PendingDecisions, while 4 other code
// paths write PendingDecisions under s.Mu.Lock(). A torn slice header read
// could cause OOB access, nil deref, or silent corruption.
//
// Before fix: `go test -race` detects a data race on PendingDecisions.
// After fix:  the RLock is held through the entire function via defer.
// Source: docs/archive/audits/AUDIT_REPORT_2026-10-24_COMPREHENSIVE.md §CS-1
// -----------------------------------------------------------------------------
func TestCheckApprovalAuthority_NoRaceOnPendingDecisions(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID:  "task-race",
		OwnerID: "owner-1",
		Steps:   map[string]*schemas.Step{},
		Status:  schemas.GraphBlocked,
		PendingDecisions: []*schemas.Decision{
			{ID: "dec-race", StepID: "s1", Type: schemas.DecisionHumanApprovalRequired, Created: time.Now(),
				Context: map[string]any{"approval_required_mounts": []string{}}, Options: []string{"approve", "reject"}},
		},
	}
	s.Mu.Lock()
	s.graphs["task-race"] = graph
	s.Mu.Unlock()

	// Reader: call checkApprovalAuthority repeatedly (reads PendingDecisions).
	// Writer: concurrently append to PendingDecisions under s.Mu.Lock().
	// Run with `go test -race` to detect the data race.
	var wg sync.WaitGroup
	const iterations = 200

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = s.checkApprovalAuthority("task-race", "dec-race", Resolver{IsAdmin: true})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			s.Mu.Lock()
			graph.PendingDecisions = append(graph.PendingDecisions, &schemas.Decision{
				ID: "dec-extra", StepID: "s1", Type: schemas.DecisionHumanApprovalRequired, Created: time.Now(),
			})
			s.Mu.Unlock()
		}
	}()

	wg.Wait()
}
