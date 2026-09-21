package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestUpdateRouteWeight_TimeDecay_PreventsPermanentLockout(t *testing.T) {
	es := &ExperienceStore{
		Mu:            sync.RWMutex{},
		RoutingMatrix: make(map[string]map[string]map[string]map[string]*RouteWeight),
	}
	ctx := context.Background()

	es.UpdateRouteWeight(ctx, "role1", "model1", "skill1", "cap1", 0.9, true)
	es.UpdateRouteWeight(ctx, "role1", "model1", "skill1", "cap1", 0.0, false)

	es.Mu.RLock()
	rw := es.RoutingMatrix["role1"]["model1"]["cap1"]["skill1"]
	weightAfterInitialFailure := rw.Weight
	es.Mu.RUnlock()

	if weightAfterInitialFailure >= 0.5 {
		t.Fatalf("weight should drop below 0.5 after a failure, got %.2f", weightAfterInitialFailure)
	}

	for i := 0; i < 10; i++ {
		es.Mu.Lock()
		rw = es.RoutingMatrix["role1"]["model1"]["cap1"]["skill1"]
		rw.LastUpdated = time.Now().Add(-25 * time.Hour)
		es.Mu.Unlock()

		es.UpdateRouteWeight(ctx, "role1", "model1", "skill1", "cap1", 0.0, false)
	}

	es.Mu.RLock()
	rw = es.RoutingMatrix["role1"]["model1"]["cap1"]["skill1"]
	weightAfterRepeatedFailures := rw.Weight
	es.Mu.RUnlock()

	if weightAfterRepeatedFailures < 0.1 {
		t.Fatalf("weight should not collapse to near-zero with time decay, got %.4f", weightAfterRepeatedFailures)
	}
}

func TestUpdateRouteWeight_SuccessNoDecay(t *testing.T) {
	es := &ExperienceStore{
		Mu:            sync.RWMutex{},
		RoutingMatrix: make(map[string]map[string]map[string]map[string]*RouteWeight),
	}
	ctx := context.Background()

	es.UpdateRouteWeight(ctx, "role1", "model1", "skill1", "cap1", 0.9, true)
	es.UpdateRouteWeight(ctx, "role1", "model1", "skill1", "cap1", 0.9, true)

	es.Mu.RLock()
	rw := es.RoutingMatrix["role1"]["model1"]["cap1"]["skill1"]
	es.Mu.RUnlock()

	if rw.Weight < 0.8 {
		t.Fatalf("weight should be high after two successes, got %.2f", rw.Weight)
	}
	if rw.SuccessCount != 2 {
		t.Fatalf("expected 2 successes, got %d", rw.SuccessCount)
	}
}
