package tools

import (
	"testing"
)

// TestShadowLearning_ProxySubsystemRegistered pins registration of the
// subsystem that actually hosts the shadow-learning goroutine.
//
// Background (audit TEST-SHADOW, 2026-10-23): the shadow-learning goroutine —
// audit H2 (bounded context.WithTimeout, not a bare context.Background) and
// audit R-3 (panic recovery) — lives in the "call" action of the PROXY
// subsystem (tools/subsystems_proxy.go), NOT the experience subsystem.
//
// Its predecessor, TestShadowLearning_BoundedContextHasDeadline, had two
// defects: (1) it referenced registerExperienceSubsystem — the wrong symbol, so
// it guarded code that has nothing to do with shadow learning; and (2) it
// asserted nothing about the deadline, so it would pass even if the bounded
// context were removed. A behavioural test of the goroutine requires a full
// App + live MCP and is impractical here, so this test pins the observable
// contract instead: registerProxySubsystem registers the subsystem and exposes
// the "call" action. It is also a compile-time guard on the correct symbol —
// a rename or removal of registerProxySubsystem fails the build.
func TestShadowLearning_ProxySubsystemRegistered(t *testing.T) {
	registerProxySubsystem(&App{})

	sub, ok := registry.Subsystems["proxy"]
	if !ok {
		t.Fatal("proxy subsystem (host of shadow learning) is not registered")
	}
	if _, ok := sub.Actions["call"]; !ok {
		t.Fatal("proxy subsystem is missing the 'call' action that hosts shadow learning")
	}
}