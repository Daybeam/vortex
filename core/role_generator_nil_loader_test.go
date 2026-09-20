package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestGenerateRoleObjects_NilResourceLoader_ReturnsErrorNotPanic is a direct
// regression test for the nil-pointer panic documented in the 2026-07-24
// playbook addendum:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	core.(*ResourceLoader).FetchCookbook(0x0, ...)
//	core.(*RoleGenerator).GenerateRoleObjects(...)
//	core.(*RoleGenerator).GenerateRole(...)
//	core.(*RoleGenerator).GetOrCreateRole(...)
//	core.NewSpawner.LogInterceptor.func2(...)
//
// This panic is reachable whenever a RoleGenerator's resourceLoader field is
// nil (e.g. because NewSpawner was constructed with a nil *ResourceLoader --
// core/session_provider_test.go's NewSpawner(reg, ts, nil, logger, nil) calls
// are one such real, existing shape) and dynamic role generation is
// triggered for a missing role. An unrecovered panic in any goroutine
// terminates the entire Go process, not just the one task, so this is a
// real production-crash risk, not merely a test-suite hygiene issue.
//
// This test calls GenerateRoleObjects directly with a nil resourceLoader and
// asserts it returns a clear error instead of panicking.
func TestGenerateRoleObjects_NilResourceLoader_ReturnsErrorNotPanic(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "main",
		Providers: map[string]*config.ProviderConfig{
			"main": {
				Provider:  "openai",
				Model:     "gpt-4o",
				APIKeyEnv: "SOME_KEY_THAT_NEED_NOT_EXIST",
			},
		},
		RoleCookbookSource: "github:anthropics/anthropic-cookbook",
	}

	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Close()

	// Deliberately nil resourceLoader -- reproduces the exact shape
	// NewSpawner(reg, ts, nil, logger, nil) produces, which is a real,
	// pre-existing call pattern in this codebase (see
	// core/session_provider_test.go), not a contrived edge case.
	gen := NewRoleGenerator(reg, nil, logger)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateRoleObjects panicked with a nil resourceLoader instead of returning an error: %v", r)
		}
	}()

	_, _, err = gen.GenerateRoleObjects(context.Background(), "task1", "step1", "missing_role", "do something useful")
	if err == nil {
		t.Fatal("expected an error when resourceLoader is nil, got nil (and no panic, which is good, but the error should be non-nil)")
	}
}

// TestResourceLoader_FetchCookbook_NilReceiver_ReturnsErrorNotPanic covers
// the FetchCookbook-level defense-in-depth guard directly (belt-and-suspenders
// for any future caller that isn't RoleGenerator.GenerateRoleObjects).
func TestResourceLoader_FetchCookbook_NilReceiver_ReturnsErrorNotPanic(t *testing.T) {
	var loader *ResourceLoader // nil receiver, deliberately

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FetchCookbook panicked on a nil receiver instead of returning an error: %v", r)
		}
	}()

	_, err := loader.FetchCookbook(context.Background(), "github:anthropics/anthropic-cookbook", "test task")
	if err == nil {
		t.Fatal("expected an error when the ResourceLoader receiver is nil, got nil")
	}
}
