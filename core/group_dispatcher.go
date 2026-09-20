package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/google/uuid"
)

// GroupDispatcher expands a RoleGroup into a TaskGraph and submits it to the
// DirectedEngine.  It is the bridge between the high-level "group_id" concept
// and the low-level []StepInput DAG that Submit() understands.
type GroupDispatcher struct {
	engine   *DirectedEngine
	registry *config.Registry
}

// NewGroupDispatcher constructs a GroupDispatcher wired to the given engine and registry.
func NewGroupDispatcher(engine *DirectedEngine, registry *config.Registry) *GroupDispatcher {
	return &GroupDispatcher{engine: engine, registry: registry}
}

// DispatchGroup expands the named RoleGroup into a TaskGraph and submits it.
// - groupID: must match a registered RoleGroup ID.
// - task: the human-language task description provided by the caller.
// - contextRefs: optional key→value references passed through to all steps.
// - roles/skills: optional session-scoped IR to augment the group's execution.
// Returns the TaskGraph ID on success.
func (gd *GroupDispatcher) DispatchGroup(
	groupID string,
	task string,
	contextRefs map[string]string,
	sessionRoles []*config.Role,
	sessionSkills []*config.Skill,
	sessionProviders []*config.ProviderConfig,
	mainProviderID string,
) (string, error) {
	gd.registry.Mu.RLock()
	group := gd.registry.RoleGroups[groupID]
	gd.registry.Mu.RUnlock()

	if group == nil {
		return "", fmt.Errorf("role group %q not found", groupID)
	}
	if len(group.Members) == 0 && group.Policy != config.GroupPolicySOP {
		return "", fmt.Errorf("role group %q has no members", groupID)
	}

	inputs, err := gd.expandGroup(group, task, contextRefs)
	if err != nil {
		return "", fmt.Errorf("expanding group %q: %w", groupID, err)
	}

	return gd.engine.SubmitWithSessionIR(inputs, sessionRoles, sessionSkills, sessionProviders, mainProviderID, "", "", 0, 0)
}

// expandGroup converts a RoleGroup + task string into a concrete []StepInput
// DAG according to the group's Policy.
func (gd *GroupDispatcher) expandGroup(
	group *config.RoleGroup,
	task string,
	contextRefs map[string]string,
) ([]schemas.StepInput, error) {
	switch group.Policy {
	case config.GroupPolicySequential:
		return gd.expandSequential(group, task, contextRefs)
	case config.GroupPolicyParallel:
		return gd.expandParallel(group, task, contextRefs)
	case config.GroupPolicyVoting:
		return gd.expandVoting(group, task, contextRefs)
	case config.GroupPolicyChainOfThought:
		return gd.expandChainOfThought(group, task, contextRefs)
	case config.GroupPolicySOP:
		return gd.expandSOP(group, task, contextRefs)
	default:
		// Fallback to sequential for unrecognised/empty policy
		return gd.expandSequential(group, task, contextRefs)
	}
}

// ─── Policy implementations ───────────────────────────────────────────────

// expandSequential wires steps so that step[i] depends on step[i-1].
func (gd *GroupDispatcher) expandSequential(
	group *config.RoleGroup,
	task string,
	contextRefs map[string]string,
) ([]schemas.StepInput, error) {
	inputs := make([]schemas.StepInput, 0, len(group.Members))
	var prevID string

	for i, m := range group.Members {
		stepID := groupStepID(group.ID, i)
		stepTask := resolveTemplate(m.TaskTemplate, task)

		inp := schemas.StepInput{
			ID:               stepID,
			RoleID:           m.RoleID,
			Task:             stepTask,
			ContextRefs:      cloneContextRefs(contextRefs),
			ProviderOverride: m.ProviderOverride,
			GroupID:          group.ID,
		}
		if prevID != "" {
			inp.DependsOn = []string{prevID}
		}
		prevID = stepID
		inputs = append(inputs, inp)
	}

	// Optional aggregator step (only useful when there is more than one member)
	if group.AggregatorRoleID != "" && len(group.Members) > 1 {
		aggID := group.ID + "_agg_" + shortID()
		inputs = append(inputs, schemas.StepInput{
			ID:          aggID,
			RoleID:      group.AggregatorRoleID,
			Task:        fmt.Sprintf("Synthesise and summarise all prior results for the following task: %s", task),
			DependsOn:   []string{prevID},
			ContextRefs: cloneContextRefs(contextRefs),
			GroupID:     group.ID,
		})
	}
	return inputs, nil
}

// expandParallel wires all member steps without dependencies (fully concurrent).
func (gd *GroupDispatcher) expandParallel(
	group *config.RoleGroup,
	task string,
	contextRefs map[string]string,
) ([]schemas.StepInput, error) {
	memberIDs := make([]string, 0, len(group.Members))
	inputs := make([]schemas.StepInput, 0, len(group.Members)+1)

	for i, m := range group.Members {
		stepID := groupStepID(group.ID, i)
		memberIDs = append(memberIDs, stepID)
		inputs = append(inputs, schemas.StepInput{
			ID:               stepID,
			RoleID:           m.RoleID,
			Task:             resolveTemplate(m.TaskTemplate, task),
			ContextRefs:      cloneContextRefs(contextRefs),
			ProviderOverride: m.ProviderOverride,
			GroupID:          group.ID,
		})
	}

	// Optional aggregator that depends on ALL parallel steps
	if group.AggregatorRoleID != "" {
		aggID := group.ID + "_agg_" + shortID()
		inputs = append(inputs, schemas.StepInput{
			ID:          aggID,
			RoleID:      group.AggregatorRoleID,
			Task:        fmt.Sprintf("Merge, deduplicate, and summarise all parallel results for: %s", task),
			DependsOn:   memberIDs,
			ContextRefs: cloneContextRefs(contextRefs),
			GroupID:     group.ID,
		})
	}
	return inputs, nil
}

// expandVoting runs all members in parallel and appends a weighted-voting
// aggregator that picks the highest-confidence answer.
func (gd *GroupDispatcher) expandVoting(
	group *config.RoleGroup,
	task string,
	contextRefs map[string]string,
) ([]schemas.StepInput, error) {
	memberIDs := make([]string, 0, len(group.Members))
	inputs := make([]schemas.StepInput, 0, len(group.Members)+1)

	// Build weight map description for the aggregator
	var weightDesc strings.Builder

	for i, m := range group.Members {
		stepID := groupStepID(group.ID, i)
		memberIDs = append(memberIDs, stepID)

		w := m.Weight
		if w == 0 {
			w = 1.0
		}
		weightDesc.WriteString(fmt.Sprintf("- step %s (role: %s, weight: %.2f)\n", stepID, m.RoleID, w))

		inputs = append(inputs, schemas.StepInput{
			ID:               stepID,
			RoleID:           m.RoleID,
			Task:             resolveTemplate(m.TaskTemplate, task),
			ContextRefs:      cloneContextRefs(contextRefs),
			ProviderOverride: m.ProviderOverride,
			GroupID:          group.ID,
		})
	}

	// The aggregator's role: choose or synthesise best answer using weights
	aggregatorRoleID := group.AggregatorRoleID
	if aggregatorRoleID == "" {
		// Fallback: use the first member's role to aggregate
		if len(group.Members) > 0 {
			aggregatorRoleID = group.Members[0].RoleID
		}
	}
	aggID := group.ID + "_vote_" + shortID()
	inputs = append(inputs, schemas.StepInput{
		ID:     aggID,
		RoleID: aggregatorRoleID,
		Task: fmt.Sprintf(
			"You are a voting aggregator. Review the outputs from the following parallel agents and select or synthesise the best answer (highest confidence, considering weights):\n%s\nOriginal task: %s",
			weightDesc.String(), task,
		),
		DependsOn:   memberIDs,
		ContextRefs: cloneContextRefs(contextRefs),
		GroupID:     group.ID,
	})
	return inputs, nil
}

// expandChainOfThought runs members sequentially and automatically threads
// each step's output ref into the next step's ContextRefs.
func (gd *GroupDispatcher) expandChainOfThought(
	group *config.RoleGroup,
	task string,
	contextRefs map[string]string,
) ([]schemas.StepInput, error) {
	inputs := make([]schemas.StepInput, 0, len(group.Members))
	var prevID string

	for i, m := range group.Members {
		stepID := groupStepID(group.ID, i)

		// Build step task: append chain instruction after the first step
		stepTask := resolveTemplate(m.TaskTemplate, task)
		if prevID != "" {
			stepTask = fmt.Sprintf(
				"%s\n\n[Chain context: build upon the output of step %s; its result is available in context_refs[\"prev_step_output\"]]",
				stepTask, prevID,
			)
		}

		// Thread previous step's output ref into this step's context
		refs := cloneContextRefs(contextRefs)
		if prevID != "" {
			refs["prev_step_output"] = prevID
		}

		inputs = append(inputs, schemas.StepInput{
			ID:               stepID,
			RoleID:           m.RoleID,
			Task:             stepTask,
			DependsOn:        dependsOnSlice(prevID),
			ContextRefs:      refs,
			ProviderOverride: m.ProviderOverride,
			GroupID:          group.ID,
		})
		prevID = stepID
	}

	// Optional aggregator
	if group.AggregatorRoleID != "" && len(group.Members) > 1 {
		aggID := group.ID + "_agg_" + shortID()
		refs := cloneContextRefs(contextRefs)
		refs["chain_output"] = prevID
		inputs = append(inputs, schemas.StepInput{
			ID:          aggID,
			RoleID:      group.AggregatorRoleID,
			Task:        fmt.Sprintf("Produce the final, polished answer for: %s", task),
			DependsOn:   []string{prevID},
			ContextRefs: refs,
			GroupID:     group.ID,
		})
	}
	return inputs, nil
}

// expandSOP delegates to an existing registered SOP by building a single
// synthetic step with the SOP hint injected — the scheduler's SOP hint
// injection in tools/tools.go will pick it up automatically.
func (gd *GroupDispatcher) expandSOP(
	group *config.RoleGroup,
	task string,
	contextRefs map[string]string,
) ([]schemas.StepInput, error) {
	if group.SOPRef == "" {
		return nil, fmt.Errorf("group %q uses 'sop' policy but has no sop_ref", group.ID)
	}

	gd.registry.Mu.RLock()
	sop := gd.registry.SOPs[group.SOPRef]
	gd.registry.Mu.RUnlock()

	if sop == nil {
		return nil, fmt.Errorf("sop_ref %q not found in registry", group.SOPRef)
	}

	// Build StepInputs from the SOP's steps
	inputs := make([]schemas.StepInput, 0, len(sop.Steps))
	for _, s := range sop.Steps {
		stepTask := s.Task
		if stepTask == "" {
			stepTask = task
		}
		inp := schemas.StepInput{
			ID:               s.ID,
			RoleID:           s.Role,
			Task:             stepTask,
			DependsOn:        s.DependsOn,
			AdditionalSkills: s.Skills,
			AdditionalMCPs:   s.MCPs,
			ContextRefs:      cloneContextRefs(contextRefs),
			ProviderOverride: s.Provider,
			ExitCriteria:     s.ExitCriteria, // PROPAGATED (ADDED 2026-09-09)
			GroupID:          group.ID,
		}
		inputs = append(inputs, inp)
	}
	return inputs, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────

func groupStepID(groupID string, idx int) string {
	return fmt.Sprintf("%s_m%d_%s", groupID, idx, shortID())
}

func shortID() string {
	return uuid.New().String()[:6]
}

// resolveTemplate replaces {{input}} in template with the actual task.
// If template is empty it returns task verbatim.
func resolveTemplate(template, task string) string {
	if template == "" {
		return task
	}
	return strings.ReplaceAll(template, "{{input}}", task)
}

func cloneContextRefs(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func dependsOnSlice(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}
