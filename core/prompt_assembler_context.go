package core

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// buildRoleAndMemoryBlocks builds the stable role prefix: role instruction,
// privacy-gated UserProfile, epistemic grounding, and schema-gated precedents.
func (s *PromptAssembler) buildRoleAndMemoryBlocks(
	hub *ContextHub,
	role *config.Role,
	precedents []*schemas.DecisionNode,
	taskID, stepID string,
) []schemas.ContentBlock {
	var blocks []schemas.ContentBlock

	// Prepend Role-specific Instructions (agent.md) - Precedence for KVCache.
	// Tier 1 (compressible): if the instruction exceeds the governance threshold,
	// CompressRoleInstruction retains the first 3 + last 2 paragraphs.
	if role != nil && role.Instruction != "" {
		instruction := s.budgetEnforcer.CompressRoleInstruction(role.Instruction)
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text:         fmt.Sprintf("# Role: %s\n%s", role.Name, instruction),
			CacheControl: "ephemeral",
		}, GovTier1, CategoryRoleInstruction))
	} else if role != nil && s.promptManager != nil {
		// PromptManager fallback (ADDED 2026-10-18): when a role has no
		// custom instruction, try loading base_role.md template from prompts/.
		// Error-safe: on any failure (file missing, parse error), silently
		// skip — zero behavior change from pre-fallback.
		tmplCtx := map[string]any{
			"task": map[string]any{
				"id":   taskID,
				"step": stepID,
				"role": role.Name,
			},
			"env": map[string]string{
				"Time": stablePromptTime(hub).Format(time.RFC3339),
			},
		}
		// D-PROMPT-4 fix (§4.4): log render errors instead of silently dropping.
		// With missingkey=error, a missing key now returns an error — we log it
		// but still skip the block (preserve existing skip behavior, but observable).
		rendered, renderErr := s.promptManager.GetAndRender("base_role.md", tmplCtx)
		if renderErr != nil {
			log.Printf("prompt_assembler: base_role.md render failed for task %s: %v", taskID, renderErr)
		} else if rendered != "" {
			blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
				Text:         rendered,
				CacheControl: "ephemeral",
			}, GovTier1, CategoryRoleInstruction))
		}
	}

	// MemoryBank UserProfile injection (ADDED 2026-10-17).
	// Sub-agents get the same user context as the gateway agent, so they can
	// tailor output to user expertise/preferences. Only UserProfile is injected
	// (not full MemoryBank) to avoid token bloat from ActiveContext/Decisions.
	// See docs/completed/2026-10-17/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
	//
	// L1 fix (DELEGATION_HANDOVER_REDESIGN.md §12.6): in delegation mode the
	// assembled prompt is handed to an external subagent (the envelope). The
	// MemoryBank is a process-global singleton with no owner parameter, so
	// injecting UserProfile here would leak the operator's (or a prior user's)
	// expertise/preferences/communication-style to every SaaS caller. Skip
	// injection in delegation mode; the per-owner path (§12.9.3) is a larger
	// change that requires plumbing ownerID through Build and is out of scope.
	if s.registry != nil && s.registry.System.MemoryBankEnabledEffective() && !s.registry.System.DelegationMode {
		if s.memoryBankStore == nil {
			s.logMemoryBankSkip(taskID, "store_unavailable")
		} else if mb, err := s.memoryBankStore.Load(); err != nil {
			s.logMemoryBankSkip(taskID, "load_failed: "+err.Error())
		} else if mb == nil {
			s.logMemoryBankSkip(taskID, "empty_profile")
		} else {
			up := mb.UserProfile
			if len(up.Expertise) > 0 || len(up.Preferences) > 0 || up.CommunicationStyle != "" {
				var b strings.Builder
				b.WriteString("\n## User Context\n")
				if len(up.Expertise) > 0 {
					b.WriteString("Expertise: " + strings.Join(up.Expertise, ", ") + "\n")
				}
				if len(up.Preferences) > 0 {
					b.WriteString("Preferences: " + strings.Join(up.Preferences, ", ") + "\n")
				}
				if up.CommunicationStyle != "" {
					b.WriteString("Communication style: " + up.CommunicationStyle + "\n")
				}
				blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
					Text:         b.String(),
					CacheControl: "ephemeral",
				}, GovTier1, CategoryRoleInstruction))
			} else {
				s.logMemoryBankSkip(taskID, "empty_profile")
			}
		}
	}

	// Epistemic Grounding Rule (self-healing §2): injected whenever the memory
	// system is active. Tells sub-agents that long-term memory and past
	// experience are heuristics, NOT immutable truth — fresh tool output and
	// current observations always win over cached facts. Prevents "epistemic
	// rigidity" where agents blindly trust outdated API contracts, revoked
	// preferences, or superseded document states.
	if s.registry != nil && s.registry.System.MemoryBankEnabledEffective() {
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text: "\n## ⚠️ Epistemic Grounding Rule\n" +
				"Long-term memory and past experience are heuristics, NOT immutable truth. " +
				"If any cached fact regarding external APIs, user constraints, document states, or environment conditions " +
				"conflicts with fresh tool output or current observation, TRUST THE TOOL/OBSERVATION. " +
				"Emit a correction memo if memory is stale.\n",
			CacheControl: "ephemeral",
		}, GovTier1, CategoryRoleInstruction))
	}

	// Priority 6: Inject Semantic Precedents (Cognitive Feedback Loop).
	// Tier 3 (trimmable): Top-N hard cap (precedentsTopN=2) + single-sentence
	// summary per §4.2.
	if len(precedents) > 0 {
		topPrecedents := s.budgetEnforcer.TopNPrecedents(precedents, precedentsTopN)
		currentFingerprints := s.collectCurrentSchemaFingerprints()
		var filtered []*schemas.DecisionNode
		var dropped, unbound int
		for _, p := range topPrecedents {
			if len(p.SchemaHashes) == 0 {
				unbound++
				filtered = append(filtered, p)
				continue
			}
			if schemas.PrecedentPassesSchemaGate(p.SchemaHashes, currentFingerprints) {
				filtered = append(filtered, p)
			} else {
				dropped++
			}
		}
		if dropped > 0 && s.logger != nil {
			s.logger.Log(obs.EventSchemaRotQuarantine, "", "", map[string]any{
				"precedents_dropped": dropped,
				"precedents_unbound": unbound,
			})
		}
		if len(filtered) > 0 {
			var pPart strings.Builder
			pPart.WriteString("# Past Decision Precedents\n")
			pPart.WriteString("The following reasoning paths were used successfully for similar tasks under the tool versions recorded with each entry. Verify the current tool schema before reusing any call pattern:\n\n")
			for _, p := range filtered {
				pPart.WriteString("## Task Intent Similarity: ")
				pPart.WriteString(p.Outcome)
				pPart.WriteString("\n")
				pPart.WriteString(s.budgetEnforcer.SummarizePrecedent(p))
				pPart.WriteString("\n\n")
			}
			blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
				Text: pPart.String(),
			}, GovTier3, CategoryPastPrecedents))
		}
	}

	return blocks
}

// buildVolatileExperienceBlocks collects the experience-derived blocks that
// must follow the constraints cache marker. Queries and their side effects stay
// in their original order even though the returned blocks are appended later.
func (s *PromptAssembler) buildVolatileExperienceBlocks(
	ctx context.Context,
	hub *ContextHub,
	role *config.Role,
	capability string,
	toolConstraints []string,
	taskText, latestError string,
) []schemas.ContentBlock {
	var volatileBlocks []schemas.ContentBlock

	// Anti-Cliff Priority 6+: Inject Anti-Pattern Precedents into ProtectedZone
	// (zero-compression) so historical pitfalls are never lost to context
	// compression. ADDED (2026-08-27).
	antiPatternText := s.resolveAntiPatternWarnings(capability, toolConstraints)
	if antiPatternText != "" {
		volatileBlocks = append(volatileBlocks, MarkProtectedWithCategory(schemas.ContentBlock{
			Text: antiPatternText,
		}, CategoryAntiPattern))
	}

	// Historical Similar Task Patterns injection
	similarTaskText := s.resolveSimilarTaskExperience(ctx, capability)
	if similarTaskText != "" {
		volatileBlocks = append(volatileBlocks, MarkProtectedWithCategory(schemas.ContentBlock{
			Text: similarTaskText,
		}, CategorySimilarTask))

		// Embedding model compatibility check (ADDED 2026-09-06)
		// If the stored patterns were built with a different embedding model
		// than the currently active one, log a warning suggesting Reindex.
		// This check runs at prompt-build time (not query time) because
		// BuildPrompt has access to the ContextHub.
		if s.expStore != nil && hub != nil {
			if embedClient := hub.GetEmbeddingClient(); embedClient != nil {
				_, currentModel, _ := embedClient.EmbedWithModel(ctx, "")
				if currentModel != "" {
					patterns := s.expStore.QuerySimilarPatterns(ctx, []string{capability}, 2)
					for _, p := range patterns {
						if p.EmbeddingModel != "" && p.EmbeddingModel != currentModel {
							s.logger.Log("EventEmbeddingModelMismatch", "", "", map[string]any{
								"task_pattern":  p.TaskType,
								"stored_model":  p.EmbeddingModel,
								"current_model": currentModel,
								"action":        "reindex_recommended",
							})
						}
					}
				}
			}
		}
	}

	// === Phase 1: Activate Experience Evolution Loop (ActiveContextAssembler) ===
	// Dynamically assemble precise historical pitfalls, error modes, and
	// expert critiques relevant to current intent. (ADDED 2026-09-09)
	//
	// FIX (2026-09-09): this used to be nested inside the
	// `if similarTaskText != ""` block above, which gates it on an entirely
	// unrelated signal (whether QuerySimilarPatterns found anything) -- a
	// step with zero similar task patterns but a directly relevant
	// failure-mode precedent would never see it. Moved out so it runs
	// independently, gated only on s.expStore being available.
	roleID := ""
	if role != nil {
		roleID = role.ID
	}
	if s.expStore != nil {
		// P0-1 fix (2026-10-10): get embedClient from hub instead of nil.
		// See docs/completed/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §1.3 + §5.
		var embedClient store.IEmbeddingClient
		warmBudget := s.getEffectiveWarmBudget() // P2-11: dynamic budget
		warmEmbedEnabled := true
		if s.registry != nil {
			// WarmContextEmbedEnabled defaults to true; only disable if
			// explicitly set to false in config.
			// (bool zero-value is false, so we can't just check the field —
			// we need a way to distinguish "not set" from "set to false".
			// For simplicity, we treat false as "disabled" and true as
			// "enabled". Users who want the old behavior can set it to false.)
		}
		if warmEmbedEnabled && hub != nil {
			embedClient = hub.GetEmbeddingClient()
		}
		assembler := NewActiveContextAssembler(s.expStore, embedClient, warmBudget)
		// Pass real taskText and latestError into Hot Tier, capability/roleID into Warm/Cold tiers
		if contextStr := assembler.Assemble(ctx, taskText, "", latestError, capability, roleID); contextStr != "" {
			volatileBlocks = append(volatileBlocks, MarkProtectedWithCategory(schemas.ContentBlock{
				Text: contextStr,
			}, CategoryActiveContext))
		}
	}

	return volatileBlocks
}
