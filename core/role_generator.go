package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/jsonrepair"
	"github.com/daybeam/vortex/providers"
)

// RoleGenerator automatically creates roles from a cookbook source when a requested role is missing.
type RoleGenerator struct {
	registry       *config.Registry
	resourceLoader *ResourceLoader
	logger         *Logger
}

func NewRoleGenerator(registry *config.Registry, loader *ResourceLoader, logger *Logger) *RoleGenerator {
	return &RoleGenerator{
		registry:       registry,
		resourceLoader: loader,
		logger:         logger,
	}
}

// GetOrCreateRole checks for semantic reuse or triggers generation.
// FIX (2026-06-27, F18): added taskID/stepID so internal diagnostic logs land in
// the correct per-task log file instead of being misfiled under empty string.
func (g *RoleGenerator) GetOrCreateRole(ctx context.Context, taskID string, stepID string, roleID string, taskDescription string, capability string) (*config.Role, error) {
	// 1. Semantic Reuse Check
	hash := g.computeHash(taskDescription, capability)
	for _, r := range g.registry.Roles {
		if r.Metadata["hash"] == hash {
			g.logger.Log(EventRoleSynthesisSuppressed, taskID, stepID, map[string]any{
				"reason":           "HashConflict",
				"existing_role_id": r.ID,
				"hash":             hash,
			})
			return r, nil
		}
	}

	// 2. Defensive Cleanup
	g.CleanupExpiredRoles()

	// 3. Generation
	role, err := g.GenerateRole(ctx, taskID, stepID, roleID, taskDescription, hash)
	if err != nil {
		return nil, err
	}

	return role, nil
}

func (g *RoleGenerator) computeHash(desc, cap string) string {
	h := sha256.New()
	h.Write([]byte(desc + cap))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// slugifyID converts an LLM-generated human-readable name into a compact,
// registry-key/filesystem-safe identifier (lowercase ASCII alphanumerics
// with runs of anything else collapsed to a single underscore, no leading
// or trailing underscore). Used by GenerateRoleObjects when the caller
// didn't supply a role ID -- see the FIX comment at its call site for why.
// Returns "" if name has no usable alphanumeric content, so the caller can
// detect that and fall back to a different ID source.
func slugifyID(name string) string {
	var b strings.Builder
	lastUnderscore := true // suppress a leading underscore
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "_")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// extractJSONObject extracts a JSON object from model output that may be
// wrapped in a markdown code fence, preceded by free-form analysis/thinking
// text, or both. See the FIX (2026-06-27, F17) comment at the call site.
func extractJSONObject(text string) string {
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	} else if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}

	dq := string(rune(34))
	bs := string(rune(92))

	start := strings.Index(text, "{")
	if start == -1 {
		return text
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		c := text[i : i+1]
		if inString {
			if escaped {
				escaped = false
			} else if c == bs {
				escaped = true
			} else if c == dq {
				inString = false
			}
			continue
		}
		switch c {
		case dq:
			inString = true
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}

	return text[start:]
}

func (g *RoleGenerator) CleanupExpiredRoles() {
	g.registry.Mu.RLock()
	// Keep last 15 dynamic roles to prevent rapid rotation
	var dynamicRoles []*config.Role
	for _, r := range g.registry.Roles {
		if _, ok := r.Metadata["generated_at"]; ok {
			dynamicRoles = append(dynamicRoles, r)
		}
	}
	g.registry.Mu.RUnlock()

	if len(dynamicRoles) > 15 {
		// Sort by generated_at to find the oldest
		sort.Slice(dynamicRoles, func(i, j int) bool {
			ti, _ := time.Parse(time.RFC3339, dynamicRoles[i].Metadata["generated_at"])
			tj, _ := time.Parse(time.RFC3339, dynamicRoles[j].Metadata["generated_at"])
			return ti.Before(tj)
		})

		oldest := dynamicRoles[0]
		ops := []config.PatchOp{
			{Op: config.OpRemove, Path: "/roles/" + oldest.ID},
		}
		if err := g.registry.CommitOps(ops, "role_generator", ""); err != nil {
			g.logger.Log("EventRoleGenerationWarning", "", "", map[string]any{
				"role_id": oldest.ID,
				"warning": "Failed to prune expired role via WAL",
				"error":   err.Error(),
			})
		}
	}
}

// GenerateRole attempts to generate a missing role and persists it to the global registry.
func (g *RoleGenerator) GenerateRole(ctx context.Context, taskID string, stepID string, roleID string, taskDescription string, hash string) (*config.Role, error) {
	role, skill, err := g.GenerateRoleObjects(ctx, taskID, stepID, roleID, taskDescription)
	if err != nil {
		return nil, err
	}

	if role.Metadata == nil {
		role.Metadata = make(map[string]string)
	}
	role.Metadata["hash"] = hash
	role.Metadata["generated_at"] = time.Now().Format(time.RFC3339)

	// Priority 7: Use CommitOps for atomic increment with audit log
	roleJSON, _ := json.Marshal(role)
	var ops []config.PatchOp
	ops = append(ops, config.PatchOp{Op: config.OpAdd, Path: "/roles/" + role.ID, Value: json.RawMessage(roleJSON)})

	if skill != nil {
		skillJSON, _ := json.Marshal(skill)
		ops = append(ops, config.PatchOp{Op: config.OpAdd, Path: "/skills/" + skill.ID, Value: json.RawMessage(skillJSON)})
	}

	if err := g.registry.CommitOps(ops, "role_generator", taskID); err != nil {
		g.logger.Log("EventRoleGenerationWarning", taskID, stepID, map[string]any{
			"role_id": role.ID,
			"warning": "Generated role but failed to persist via WAL",
			"error":   err.Error(),
		})
	}

	// Persist the skill to the external directory
	persisted := false
	if skill != nil {
		skillsDir := g.registry.GetConfigPath()
		skillsDir = filepath.Join(config.ConfigDir(skillsDir), "workspace/skills/")
		if mkErr := os.MkdirAll(skillsDir, 0755); mkErr == nil {
			skillData, err := json.MarshalIndent(skill, "", "  ")
			if err == nil {
				if wErr := os.WriteFile(filepath.Join(skillsDir, skill.ID+".json"), skillData, 0644); wErr == nil {
					persisted = true
				} else {
					g.logger.Log("EventRoleGenerationWarning", taskID, stepID, map[string]any{
						"role_id": role.ID, "warning": "skill file write failed", "error": wErr.Error(),
					})
				}
			}
		}
	}

	g.logger.Log("EventRoleGenerationCompleted", taskID, stepID, map[string]any{
		"role_id":    role.ID,
		"capability": role.BaseCapability,
		"skill_id":   skill.ID,
		"persisted":  persisted,
	})

	return role, nil
}

// GenerateRoleObjects creates the Role and Skill objects by calling the LLM,
// without adding them to the registry or persisting them.
func (g *RoleGenerator) GenerateRoleObjects(ctx context.Context, taskID string, stepID string, roleID string, taskDescription string) (*config.Role, *config.Skill, error) {
	g.logger.Log("EventRoleGenerationStarted", taskID, stepID, map[string]any{
		"role_id":          roleID,
		"task_description": taskDescription,
	})

	providerCfg := g.registry.GetProvider(g.registry.DefaultProviderName()) // audit H1: locked accessors
	if providerCfg == nil {
		return nil, nil, fmt.Errorf("default provider not configured")
	}
	provider, err := providers.Get(providerCfg, g.registry.ExternalRuntimes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get default provider: %w", err)
	}

	cookbookSource := g.registry.ResolveCookbookSource(providerCfg.Model, providerCfg.Family)
	if cookbookSource == "" {
		return nil, nil, fmt.Errorf("no cookbook source configured for model %s", providerCfg.Model)
	}

	// FIX (2026-07-25, defense-in-depth for the panic documented in the
	// 2026-07-24 addendum): g.resourceLoader can be nil when a RoleGenerator
	// is constructed via NewSpawner(..., nil) (several existing call sites do
	// this, e.g. core/session_provider_test.go). Calling FetchCookbook on a
	// nil *ResourceLoader panics inside it (nil pointer deref on l.Mu). Fail
	// with a clear, catchable error instead of crashing the whole process --
	// an unrecovered panic in any goroutine (this can be reached from
	// DirectedEngine.run's background goroutine) takes down the entire
	// orchestrator, not just the one task.
	if g.resourceLoader == nil {
		return nil, nil, fmt.Errorf("role generation requires a cookbook resource loader, but none was configured (resourceLoader is nil)")
	}

	cookbookContent, err := g.resourceLoader.FetchCookbook(ctx, cookbookSource, taskDescription)
	if err != nil {
		g.logger.Log("EventRoleGenerationFailed", taskID, stepID, map[string]any{
			"role_id": roleID,
			"error":   fmt.Sprintf("Failed to fetch cookbook from %s: %v", cookbookSource, err),
		})
		return nil, nil, fmt.Errorf("failed to fetch cookbook: %w", err)
	}
	systemPrompt := `You are an expert AI Systems Architect. Your task is to dynamically generate a new AI Role configuration.
You have access to a library of engineering principles (Cookbook). 
Your objective is to:
1. Analyze the requested Task and Role.
2. Search the provided Cookbook for relevant engineering patterns, best practices, or constraints.
3. Synthesize these findings into a detailed and strict System Prompt for the new Role.
4. Ensure the Role's System Prompt explicitly incorporates the synthesized engineering principles.

You MUST return the result EXACTLY as a valid JSON object, with no markdown formatting, no code blocks, and no extra text.

The JSON MUST have this schema:
{
  "name": "Human readable name for the role",
  "capability": "The primary capability this role performs",
  "system_prompt": "A highly detailed system prompt. It MUST include a section titled 'Engineering Standards' derived from the Cookbook, defining specific rules and constraints for the role."
}`

	userPrompt := fmt.Sprintf(`Task Description: %s
Requested Role ID: %s

Cookbook Knowledge Base:
%s

Generate the required JSON for this role. If the Cookbook is a complex resource (e.g., Jupyter Notebook), intelligently extract only the relevant engineering principles, patterns, and best practices. Do not include raw code or irrelevant metadata. Synthesize them into the 'system_prompt' field.`, taskDescription, roleID, cookbookContent)

	g.logger.Log("EventRoleGenerationLLMCall", taskID, stepID, map[string]any{
		"role_id":  roleID,
		"provider": providerCfg.Provider,
	})

	resp, err := provider.Complete(ctx, providers.CompleteRequest{
		System:    systemPrompt,
		User:      userPrompt,
		Model:     providerCfg.Model,
		MaxTokens: 4096,
	})

	if err != nil {
		return nil, nil, fmt.Errorf("LLM provider error during role generation: %w", err)
	}

	text := extractJSONObject(strings.TrimSpace(resp.Text))

	var generated struct {
		Name         string `json:"name"`
		Capability   string `json:"capability"`
		SystemPrompt string `json:"system_prompt"`
	}

	if err := json.Unmarshal([]byte(text), &generated); err != nil {
		// Attempt truncation repair if LLM output was cut by max_tokens
		if repaired, ok := jsonrepair.Repair(text); ok {
			if err2 := json.Unmarshal([]byte(repaired), &generated); err2 == nil {
				g.logger.Log("EventRoleGenerationRepaired", taskID, stepID, map[string]any{
					"role_id":      roleID,
					"original_len": len(text),
					"repaired_len": len(repaired),
				})
				goto REPAIRED_OK
			}
		}

		g.logger.Log("EventRoleGenerationFailed", taskID, stepID, map[string]any{
			"role_id":    roleID,
			"error":      "Failed to parse LLM output as JSON",
			"output":     text,
			"raw_output": resp.Text,
		})
		return nil, nil, fmt.Errorf("failed to parse generated role JSON: %w", err)
	}
REPAIRED_OK:

	// FIX (2026-08-1x): the ephemeral/dynamic "let the system pick a role
	// from the task description alone" flow (see builtin_interceptors.go's
	// LogInterceptor) legitimately calls this function with roleID == "".
	// Previously that empty string was used AS-IS for both newRole.ID and
	// the skill_base_<roleID> naming scheme, producing a real, persisted
	// ghost role (ID "") and a malformed skill (ID "skill_base_", truncated
	// "Auto-generated base skill for " description) -- one such orphan was
	// found still loaded live and committed to disk in the 2026-08-10/11
	// addendum's session (workspace/skills/skill_base_.json, removed).
	// Derive a real, human-traceable ID from the LLM's own generated name
	// when the caller didn't supply one, falling back to a short hash-based
	// ID only if even that is empty/unusable.
	effectiveRoleID := roleID
	if effectiveRoleID == "" {
		effectiveRoleID = slugifyID(generated.Name)
		if effectiveRoleID == "" {
			h := sha256.Sum256([]byte(taskDescription + generated.Capability))
			effectiveRoleID = fmt.Sprintf("role_%x", h[:6])
		}
	}

	skillID := fmt.Sprintf("skill_base_%s", effectiveRoleID)

	// 1. Create the base skill
	newSkill := &config.Skill{
		ID:            skillID,
		Name:          generated.Name + " Base Skill",
		Capability:    generated.Capability,
		Description:   fmt.Sprintf("Auto-generated base skill for %s", effectiveRoleID),
		TokenEstimate: 500, // heuristic
		Implementations: map[string]config.SkillImplementation{
			"default": {
				SystemPrompt:  generated.SystemPrompt,
				PromptVersion: "1.0",
			},
		},
	}
	newSkill.InitFilter()

	// 2. Create the role
	newRole := &config.Role{
		ID:                  effectiveRoleID,
		Name:                generated.Name,
		BaseCapability:      generated.Capability,
		BoundSkills:         []string{skillID},
		BoundMCPBindings:    []config.MCPBinding{},
		AllowDynamicSkills:  true,
		AllowDynamicMCPs:    true,
		MaxAdditionalSkills: 5,
		Metadata:            make(map[string]string),
	}

	return newRole, newSkill, nil
}
