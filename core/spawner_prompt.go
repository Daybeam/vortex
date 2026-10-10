package core

import (
	"context"
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
) ([]schemas.ContentBlock, error) {
	if s.pa == nil {
		s.pa = NewPromptAssembler(s.expStore, s.ResourceLoader, s.registry, s.logger)
	}
	if s.archive != nil {
		s.pa.SetArchive(s.archive)
	}
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
	blocks, err := s.buildSystemPrompt(promptCtx, hub, role, []string{}, role.BaseCapability, &config.ProviderConfig{}, []string{}, ctxMap, []string{}, false, nil, task, "", "")
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
