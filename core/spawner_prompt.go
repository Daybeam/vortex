package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func (s *Spawner) persistAttachments(taskID, stepID string, attachments []schemas.Attachment) {
	if len(attachments) == 0 {
		return
	}

	dir := filepath.Join(s.outputBase, taskID, stepID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		s.logger.Log(EventStepFailed, taskID, stepID, map[string]any{"error": "failed to create attachments dir", "details": err.Error()})
		return
	}

	for _, att := range attachments {
		s.logger.Log(EventStepStarted, taskID, stepID, map[string]any{
			"action": "persist_attachment",
			"label":  att.Label,
			"path":   att.Path,
		})
	}
}

// taskStoreGet is a nil-safe wrapper around s.taskStore.Get. A Spawner built
// via a bare struct literal (tests, or DirectExecute's documented contract)
// may have a nil taskStore; treat that as no stored result rather than
// panicking.

// taskStoreGet is a nil-safe wrapper around s.taskStore.Get. A Spawner built
// via a bare struct literal (tests, or DirectExecute's documented contract)
// may have a nil taskStore; treat that as no stored result rather than
// panicking.
func (s *Spawner) taskStoreGet(ctx context.Context, taskID, stepID string) *store.StepResult {
	if s.taskStore == nil {
		return nil
	}
	res, _ := s.taskStore.Get(ctx, taskID, stepID)
	return res
}

// taskStoreSet is a nil-safe wrapper around s.taskStore.Set.

// taskStoreSet is a nil-safe wrapper around s.taskStore.Set.
func (s *Spawner) taskStoreSet(ctx context.Context, taskID, stepID string, result *store.StepResult) {
	if s.taskStore == nil {
		return
	}
	s.taskStore.Set(ctx, taskID, stepID, result)
}

func (s *Spawner) resolveContextAttachments(ctx context.Context, taskID, stepID string) []providers.Attachment {
	res := s.taskStoreGet(ctx, taskID, stepID)
	if res == nil || len(res.Attachments) == 0 {
		return nil
	}

	var attachments []providers.Attachment
	for _, att := range res.Attachments {
		attachments = append(attachments, providers.Attachment{
			MimeType: att.MimeType,
			Path:     att.Path,
			Label:    att.Label,
		})
	}
	return attachments
}

// buildSystemPrompt delegates to PromptAssembler.Build (Phase 1 of the
// Spawner three-layer split). The body was extracted to core/prompt_assembler.go
// for independent testability and decoupling from Spawner internals.
func (s *Spawner) buildSystemPrompt(
	ctx context.Context, // L-2.1: propagated ctx for cancellation
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
	taskID string, // P1-5: for Archive Path D query
	trace []store.ToolInteraction, // ADDED 2026-10-25: for DataShape mismatch detection
) ([]schemas.ContentBlock, error) {
	if s.pa == nil {
		s.pa = NewPromptAssembler(s.expStore, s.ResourceLoader, s.registry, s.logger)
	}
	if s.archive != nil {
		s.pa.SetArchive(s.archive)
	}
	s.pa.SetOutputBase(s.outputBase) // D4: enable workspace path injection
	s.pa.SetRecentInteractions(trace)
	return s.pa.Build(ctx, hub, role, skillIDs, capability, providerCfg, toolConstraints, mergedContext, fewShots, isolation, precedents, taskText, latestError, taskID)
}

// PreviewPrompt assembles and returns the full system prompt for a role/task
// without executing any provider call. Useful for debugging and prompt review.
// Moved from spawner.go during god-class split.
func (s *Spawner) PreviewPrompt(roleID, task string, ctxMap map[string]any) (string, error) {
	hub := NewContextHub(s.registry, nil, s.expStore)
	role := hub.GetRole(roleID)
	if role == nil {
		return "", fmt.Errorf("role %s not found", roleID)
	}
	// audit L-NEW-13: add timeout to prevent blocking forever on I/O during prompt construction.
	promptCtx, promptCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer promptCancel()
	blocks, err := s.buildSystemPrompt(promptCtx, hub, role, []string{}, role.BaseCapability, &config.ProviderConfig{}, []string{}, ctxMap, []string{}, false, nil, task, "", "", nil)
	if err != nil {
		return "", err
	}
	var full strings.Builder
	for _, b := range blocks {
		full.WriteString(b.Text)
		full.WriteString("\n\n")
	}
	return full.String(), nil
}

// LastSystemPromptProfile returns the most recent real system-prompt token
// profile captured during an actual spawn. Returns nil if Build has not run
// yet (no task spawned). Safe for concurrent use.
func (s *Spawner) LastSystemPromptProfile() *ContextProfile {
	if s.pa == nil {
		return nil
	}
	return s.pa.LastProfile()
}

// defaultProviderConfig returns the configured default provider's
// *ProviderConfig under the registry read lock, or nil if unset / missing.
func (s *Spawner) defaultProviderConfig() *config.ProviderConfig {
	name := s.registry.DefaultProviderName()
	if name == "" {
		return nil
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	if pc, ok := s.registry.Providers[name]; ok {
		return pc
	}
	return nil
}

// SampleSystemPromptProfile assembles a *representative* system prompt for the
// first configured role and the default provider, and returns its token
// profile. Unlike a real spawn, this does NOT overwrite the live profile — it
// builds with a throwaway PromptAssembler — so it is safe to call for the
// observability panel before any task has actually run (gives the UI something
// real to render rather than an empty state).
func (s *Spawner) SampleSystemPromptProfile() (*ContextProfile, error) {
	var role *config.Role
	for _, r := range s.registry.Roles {
		if r != nil {
			role = r
			break
		}
	}
	if role == nil {
		return nil, fmt.Errorf("no role configured; cannot sample system prompt")
	}
	hub := NewContextHub(s.registry, nil, s.expStore)
	pCfg := s.defaultProviderConfig()
	if pCfg == nil {
		pCfg = &config.ProviderConfig{}
	}
	pa := NewPromptAssembler(s.expStore, s.ResourceLoader, s.registry, s.logger)
	if s.archive != nil {
		pa.SetArchive(s.archive)
	}
	// A short, neutral task text keeps the sample deterministic; precedents /
	// few-shots / history are intentionally empty (this is a config-level
	// sample, not a real task).
	blocks, err := pa.Build(context.Background(), hub, role, []string{}, role.BaseCapability, pCfg, []string{}, map[string]any{}, []string{}, false, nil, "sample task", "", "")
	if err != nil {
		return nil, err
	}
	return pa.ComputeContextProfile(
		blocks,
		pCfg.GetMaxSystemPromptTokens(),
		pCfg.GetReserveTokens(),
		pCfg.Model,
		pCfg.Provider,
	), nil
}

// GetUsageSnapshot returns the global per-model / per-provider token usage
// ledger (ADDED 2026-10-12). Backs the /api/usage endpoint so the
// observability UI can show consumption per chat model, per provider, and
// per embedding model — the raw material for future cost accounting and
// consumption benchmarking.
func (s *Spawner) GetUsageSnapshot() map[string]any {
	return GetGlobalStats().GetSnapshot()
}

// UpdateSystemPromptOuterRing folds the per-turn outer-ring composition into
// the live system-prompt profile (ADDED 2026-10-12). The spawner calls this
// after each successful provider turn so /api/context/profile can render the
// full context budget (system / tools / history) instead of the system ring
// alone:
//   - tools / history: estimated with the CJK-aware providers heuristic
//     (provider APIs report no token count for tool schemas or history)
//   - prompt / completion: provider-reported counts from the last turn
//
// Nil-safe when no profile has been captured yet.
func (s *Spawner) UpdateSystemPromptOuterRing(tools, history, prompt, completion int) {
	if s.pa == nil {
		return
	}
	s.pa.UpdateOuterRing(tools, history, prompt, completion)
}

// estimateMCPServersTokens estimates the token cost of the MCP tool schemas
// sent to the model (outer-ring observability, ADDED 2026-10-12). Tool
// schemas are typically the second-largest context consumer after the system
// prompt itself, and no provider reports their token count, so we estimate
// from name + description + JSON-serialized input schema, reusing the same
// CJK-aware heuristic as the rest of the budget system.
func estimateMCPServersTokens(servers []providers.MCPServerDef) int {
	total := 0
	for i := range servers {
		for j := range servers[i].Tools {
			t := &servers[i].Tools[j]
			total += providers.EstimateTokens(t.Name)
			total += providers.EstimateTokens(t.Description)
			if len(t.InputSchema) > 0 {
				if b, err := json.Marshal(t.InputSchema); err == nil {
					total += providers.EstimateTokens(string(b))
				}
			}
		}
	}
	return total
}
