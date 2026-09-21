package store

import (
	"context"
	"sync"
	"testing"
)

// TestUpdateRouteWeight_ConcurrentNoRace is the regression test for the C1
// data race fix: UpdateRouteWeight must hold es.Mu while mutating
// RoutingMatrix, because SelectSkill reads RoutingMatrix under es.Mu.RLock()
// and RecordTaskCompletion writes under es.Mu.Lock(). Without the lock on
// UpdateRouteWeight, `go test -race` detects a concurrent map read/write.
//
// This test would FAIL under -race if the es.Mu.Lock()/Unlock() were removed
// from UpdateRouteWeight.
func TestUpdateRouteWeight_ConcurrentNoRace(t *testing.T) {
	es := newTestStore(t)

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half the goroutines write via UpdateRouteWeight (different role keys
	// to avoid serializing on the same map entry — we want to stress the
	// map structure itself, not a single slot).
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			roleID := "role_" + itoa(idx)
			for j := 0; j < iterations; j++ {
				_ = es.UpdateRouteWeight(
					context.Background(),
					roleID, "model_v1", "skill_a", "cap_x",
					0.8, true,
				)
			}
		}(i)
	}

	// Half the goroutines read via SelectSkill (which acquires RLock and
	// traverses RoutingMatrix through experienceScore).
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			roleID := "role_" + itoa(idx)
			for j := 0; j < iterations; j++ {
				_ = es.SelectSkill(
					context.Background(),
					[]string{"skill_a"}, "cap_x", roleID, "model_v1", 0.1,
				)
			}
		}(i)
	}

	wg.Wait()
}

// itoa is a minimal int→string converter to avoid pulling in strconv for a
// test helper (ponytail ladder: stdlib, but one-liner is even smaller).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
