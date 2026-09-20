package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
)

// FilterRetriever uses the Cuckoo-filter based CapabilityIndex for fast-fail exact matching.
type FilterRetriever struct {
	Index    *CapabilityIndex
	Registry *config.Registry
}

func (r *FilterRetriever) Recall(ctx context.Context, query string, tags []string) []DiscoveryCandidate {
	var candidates []DiscoveryCandidate
	r.Registry.Mu.RLock()
	defer r.Registry.Mu.RUnlock()

	// Check tags against Registry
	for _, tag := range tags {
		// Fast-fail check
		if !r.Index.Maybe(tag) {
			continue
		}

		// Exact match search in Roles
		for id, role := range r.Registry.Roles {
			if strings.EqualFold(role.BaseCapability, tag) {
				candidates = append(candidates, DiscoveryCandidate{
					ID:          id,
					Type:        CandidateRole,
					Name:        role.Name,
					Description: role.Purpose,
					Confidence:  1.0,
					Source:      "filter",
				})
			}
		}

		// Exact match search in MCP Provides
		for id, mcp := range r.Registry.MCPs {
			for _, p := range mcp.Provides {
				if strings.EqualFold(p, tag) {
					candidates = append(candidates, DiscoveryCandidate{
						ID:          id,
						Type:        CandidateMCP,
						Name:        id,
						Description: fmt.Sprintf("Provides: %s", strings.Join(mcp.Provides, ", ")),
						Confidence:  0.9,
						Source:      "filter",
					})
				}
			}
		}
	}

	return candidates
}

// KeywordRetriever performs substring/fuzzy matching over registry fields.
type KeywordRetriever struct {
	Registry *config.Registry
}

func (r *KeywordRetriever) Recall(ctx context.Context, query string, tags []string) []DiscoveryCandidate {
	var candidates []DiscoveryCandidate
	lowerQuery := strings.ToLower(query)
	if lowerQuery == "" {
		return nil
	}

	r.Registry.Mu.RLock()
	defer r.Registry.Mu.RUnlock()

	// Search Roles
	for id, role := range r.Registry.Roles {
		if strings.Contains(strings.ToLower(role.Name), lowerQuery) ||
			strings.Contains(strings.ToLower(role.Purpose), lowerQuery) {
			candidates = append(candidates, DiscoveryCandidate{
				ID:          id,
				Type:        CandidateRole,
				Name:        role.Name,
				Description: role.Purpose,
				Confidence:  0.8,
				Source:      "keyword",
			})
		}
	}

	// Search MCPs
	for id, mcp := range r.Registry.MCPs {
		toolsJoined := strings.Join(mcp.AvailableTools, " ")
		if strings.Contains(strings.ToLower(id), lowerQuery) ||
			strings.Contains(strings.ToLower(toolsJoined), lowerQuery) {
			candidates = append(candidates, DiscoveryCandidate{
				ID:          id,
				Type:        CandidateMCP,
				Name:        id,
				Description: fmt.Sprintf("Tools: %s", toolsJoined),
				Confidence:  0.7,
				Source:      "keyword",
			})
		}
	}

	// Search SOPs
	for id, sop := range r.Registry.SOPs {
		if strings.Contains(strings.ToLower(sop.Description), lowerQuery) {
			candidates = append(candidates, DiscoveryCandidate{
				ID:          id,
				Type:        CandidateSOP,
				Name:        id,
				Description: sop.Description,
				Confidence:  0.75,
				Source:      "keyword",
			})
		}
	}

	// Search RoleGroups
	for id, g := range r.Registry.RoleGroups {
		if strings.Contains(strings.ToLower(g.Name), lowerQuery) ||
			strings.Contains(strings.ToLower(g.Description), lowerQuery) {
			candidates = append(candidates, DiscoveryCandidate{
				ID:          id,
				Type:        CandidateGroup,
				Name:        g.Name,
				Description: g.Description,
				Confidence:  0.85,
				Source:      "keyword",
			})
		}
	}

	return candidates
}

// SemanticRetriever leverages the CapabilityGraph and LLM-detected tags.
type SemanticRetriever struct {
	Graph    *CapabilityGraph
	Registry *config.Registry
}

func (r *SemanticRetriever) Recall(ctx context.Context, query string, tags []string) []DiscoveryCandidate {
	var candidates []DiscoveryCandidate

	// 1. Expand tags via Graph
	expanded := make(map[string]bool)
	for _, t := range tags {
		for _, related := range r.Graph.GetRelated(t) {
			expanded[related] = true
		}
	}

	r.Registry.Mu.RLock()
	defer r.Registry.Mu.RUnlock()

	// 2. Find items matching expanded tags
	for tag := range expanded {
		// Match against Roles
		for id, role := range r.Registry.Roles {
			if strings.Contains(strings.ToLower(role.BaseCapability), tag) {
				candidates = append(candidates, DiscoveryCandidate{
					ID:          id,
					Type:        CandidateRole,
					Name:        role.Name,
					Description: role.Purpose,
					Confidence:  0.7, // Semantic expansion has lower base confidence
					Source:      "semantic",
				})
			}
		}

		// Match against Skills
		for id, skill := range r.Registry.Skills {
			if strings.EqualFold(skill.Capability, tag) {
				candidates = append(candidates, DiscoveryCandidate{
					ID:          id,
					Type:        CandidateSkill,
					Name:        skill.Name,
					Description: skill.Description,
					Confidence:  0.65,
					Source:      "semantic",
				})
			}
		}
	}

	return candidates
}
