package core

import (
	"context"
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// mustTempDir creates a temporary directory or fails the test immediately.
// The directory is automatically removed when the test finishes via t.Cleanup.
//
// Usage:
//
//	dir := mustTempDir(t, "debate-log")
func mustTempDir(t *testing.T, prefix string) string {
	t.Helper()
	d, err := os.MkdirTemp("", prefix+"-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp(\"\", %q-*) failed: %v", prefix, err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// mustNewLogger creates a Logger or fails the test immediately.
// Use this in tests to avoid repeating the error-check boilerplate.
//
// Usage:
//
//	logger := mustNewLogger(t, dir, &config.SystemSettings{})
//	defer logger.Close()
func mustNewLogger(t *testing.T, dir string, cfg *config.SystemSettings) *obs.Logger {
	t.Helper()
	logger, err := obs.NewLogger(dir, cfg)
	if err != nil {
		t.Fatalf("NewLogger(%q, ...) failed: %v", dir, err)
	}
	t.Cleanup(func() { logger.Close() })
	return logger
}

// mustNewExperienceStore creates an ExperienceStore or fails the test immediately.
// Use this in tests to avoid repeating the error-check boilerplate.
//
// Usage:
//
//	es := mustNewExperienceStore(t, dir, ts, &config.SystemSettings{}, nil, nil)
func mustNewExperienceStore(
	t *testing.T,
	dir string,
	ts store.ITaskStore,
	sys *config.SystemSettings,
	expBackend store.IExperienceBackend,
	apBackend store.IAntiPatternBackend,
) *store.ExperienceStore {
	t.Helper()
	es, err := store.NewExperienceStore(dir, ts, sys, expBackend, apBackend)
	if err != nil {
		t.Fatalf("NewExperienceStore(%q, ...) failed: %v", dir, err)
	}
	return es
}

// injectTestGraph inserts a task graph into the scheduler's internal map
// under the proper mutex. Use this instead of directly accessing s.graphs
// to avoid test-coupled-to-implementation patterns.
//
// Usage:
//
//	injectTestGraph(t, s, graph)
func injectTestGraph(t *testing.T, s *DirectedEngine, graph *schemas.TaskGraph) {
	t.Helper()
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()
}

// injectRunningTestGraph inserts a task graph with a cancel func and done
// channel, simulating a fully running task. Returns the context and cancel
// func so tests can assert on context cancellation. Use this instead of
// directly accessing s.graphs/s.cancelFuncs/s.doneChans.
//
// Usage:
//
//	ctx, cancel := injectRunningTestGraph(t, s, graph)
//	defer cancel()
func injectRunningTestGraph(t *testing.T, s *DirectedEngine, graph *schemas.TaskGraph) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.cancelFuncs[graph.TaskID] = cancel
	s.doneChans[graph.TaskID] = make(chan struct{})
	s.Mu.Unlock()
	return ctx, cancel
}

// --- Spawner test helpers (audit F-1) ---
//
// These helpers encapsulate access to unexported Spawner fields and methods.
// Use them instead of directly calling spawner.buildSystemPrompt(),
// spawner.collectDelegationToolDefs(), etc. This localizes the coupling to
// implementation details in one file so internal refactors only need to update
// these wrappers.

// buildSystemPromptForTest wraps Spawner.buildSystemPrompt for test use.
func buildSystemPromptForTest(
	t *testing.T,
	s *Spawner,
	ctx context.Context,
	hub *ContextHub,
	role *config.Role,
	skillIDs []string,
	capability string,
	providerCfg *config.ProviderConfig,
	toolConstraints []string,
	mergedContext map[string]any,
	fewShots []string,
	isolation bool,
	precedents []*schemas.DecisionNode,
	taskText string,
	latestError string,
	taskID string,
	stepID string,
	trace []store.ToolInteraction,
) []schemas.ContentBlock {
	t.Helper()
	blocks, err := s.buildSystemPrompt(ctx, hub, role, skillIDs, capability, providerCfg, toolConstraints, mergedContext, fewShots, isolation, precedents, taskText, latestError, taskID, stepID, trace)
	if err != nil {
		t.Fatalf("buildSystemPrompt failed: %v", err)
	}
	return blocks
}

// collectDelegationToolDefsForTest wraps Spawner.collectDelegationToolDefs.
func collectDelegationToolDefsForTest(t *testing.T, s *Spawner, hub *ContextHub, bindings []config.MCPBinding) []schemas.ToolDefinition {
	t.Helper()
	return s.collectDelegationToolDefs(hub, bindings)
}

// recordEffectForTest wraps Spawner.recordEffect.
func recordEffectForTest(t *testing.T, s *Spawner, taskID, stepID, toolName string, args map[string]any, result string, status string) {
	t.Helper()
	s.recordEffect(taskID, stepID, toolName, args, result, status)
}

// incrTaskToolFailForTest wraps Spawner.incrTaskToolFail.
func incrTaskToolFailForTest(t *testing.T, s *Spawner, taskID, toolName string, stepCount int) int {
	t.Helper()
	return s.incrTaskToolFail(taskID, toolName, stepCount)
}

// setSpawnerArchiveForTest sets the unexported archive and pa fields on a
// Spawner for tests that need to verify context archive integration.
func setSpawnerArchiveForTest(t *testing.T, s *Spawner, archive *ContextArchive, reg *config.Registry, logger *obs.Logger) {
	t.Helper()
	s.archive = archive
	s.pa = NewPromptAssembler(nil, nil, reg, logger)
}
