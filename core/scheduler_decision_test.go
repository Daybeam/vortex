/*
 * Cross-Family Auditing — selects a verifier provider from a different model family.
 */

package core

import (
	"github.com/daybeam/vortex/config"
	"sync"
	"testing"
)

func TestGetCrossFamilyVerifier(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		Providers: map[string]*config.ProviderConfig{
			"p1": {Family: "A"},
			"p2": {Family: "B"},
			"p3": {Family: "A"},
		},
	}

	engine := &DirectedEngine{
		registry: registry,
	}
	engine.walker = NewDAGGraphWalker(nil, nil, "", nil, registry)

	// Case 1: p1 (Family A) should select p2 (Family B)
	verifier := engine.GetCrossFamilyVerifier("p1")
	if verifier != "p2" {
		t.Errorf("expected p2, got %s", verifier)
	}

	// Case 2: No different family provider
	registry2 := &config.Registry{
		Mu: sync.RWMutex{},
		Providers: map[string]*config.ProviderConfig{
			"p1": {Family: "A"},
			"p2": {Family: "A"},
		},
	}
	engine2 := &DirectedEngine{
		registry: registry2,
	}
	engine2.walker = NewDAGGraphWalker(nil, nil, "", nil, registry2)
	verifier2 := engine2.GetCrossFamilyVerifier("p1")
	if verifier2 != "" {
		t.Errorf("expected empty string, got %s", verifier2)
	}
}
