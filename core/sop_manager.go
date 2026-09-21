package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// SOPManager handles CRUD operations and governance for Standard Operating Procedures.
type SOPManager struct {
	registry *config.Registry
	mu       sync.RWMutex
	sopsDir  string
}

func NewSOPManager(reg *config.Registry, sopsDir string) *SOPManager {
	return &SOPManager{
		registry: reg,
		sopsDir:  sopsDir,
	}
}

// SOPOverview provides a summary of an SOP for listing.
type SOPOverview struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	Author         string `json:"author"`
	MutableByAgent bool   `json:"mutable_by_agent"`
	Description    string `json:"description"`
	StepCount      int    `json:"step_count"`
}

func (m *SOPManager) ListSOPs() []SOPOverview {
	m.registry.Mu.RLock()
	defer m.registry.Mu.RUnlock()

	var list []SOPOverview
	for id, sop := range m.registry.SOPs {
		list = append(list, SOPOverview{
			ID:             id,
			Version:        sop.Version,
			Author:         sop.Author,
			MutableByAgent: sop.MutableByAgent,
			Description:    sop.Description,
			StepCount:      len(sop.Steps),
		})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].ID < list[j].ID
	})

	return list
}

func (m *SOPManager) GetSOP(id string) (*schemas.SOP, error) {
	m.registry.Mu.RLock()
	defer m.registry.Mu.RUnlock()

	sop, ok := m.registry.SOPs[id]
	if !ok {
		return nil, fmt.Errorf("sop %q not found", id)
	}
	return sop, nil
}

// SaveSOP persists an SOP to disk and hot-reloads it.
// It enforces the immutability guard for human-authored SOPs.
func (m *SOPManager) SaveSOP(ctx context.Context, sop schemas.SOP, isAgent bool, override bool) error {
	if sop.ID == "" {
		return fmt.Errorf("sop ID is required")
	}

	// 1. Immutability Guard
	m.registry.Mu.RLock()
	existing, exists := m.registry.SOPs[sop.ID]
	m.registry.Mu.RUnlock()

	if exists {
		// Rule: Agent cannot overwrite human-authored immutable SOP
		if isAgent && !existing.MutableByAgent && existing.Author == "human_expert" && !override {
			return fmt.Errorf("SOP %q is immutable and human-authored; agents cannot overwrite it. Please submit a proposal instead", sop.ID)
		}
	}

	// 2. Validation
	if len(sop.Steps) == 0 {
		return fmt.Errorf("SOP must have at least one step")
	}
	if err := validateSOPDAG(sop.Steps); err != nil {
		return fmt.Errorf("SOP validation failed: %w", err)
	}

	// 3. Persistence
	data, err := json.MarshalIndent(sop, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal SOP: %w", err)
	}

	// Sanitize ID to prevent path traversal (matches the established pattern
	// in tools/subsystems.go's update_sop action).
	safeID := filepath.Base(sop.ID)
	path := filepath.Join(m.sopsDir, safeID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create SOPs directory: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write SOP file: %w", err)
	}

	// 4. Hot Reload
	return m.registry.Load()
}

// ProposeSOPPatch saves an optimization proposal for an existing SOP.
func (m *SOPManager) ProposeSOPPatch(ctx context.Context, sopID string, patch string, traceID string, author string) (string, error) {
	m.registry.Mu.RLock()
	_, exists := m.registry.SOPs[sopID]
	m.registry.Mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("cannot propose patch for non-existent SOP %q", sopID)
	}

	proposalID := fmt.Sprintf("prop_%s_%s", sopID, strings.ReplaceAll(traceID, "-", "")[:8])
	if traceID == "" {
		proposalID = fmt.Sprintf("prop_%s_%d", sopID, os.Getpid())
	}

	proposal := map[string]any{
		"sop_id":         sopID,
		"proposal_id":    proposalID,
		"patch":          patch,
		"evidence_trace": traceID,
		"proposed_by":    author,
		"status":         "pending_review",
		"created_at":     time.Now().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(proposal, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal proposal: %w", err)
	}

	propPath := filepath.Join(m.sopsDir, "proposals", proposalID+".json")
	if err := os.MkdirAll(filepath.Dir(propPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create proposals directory: %w", err)
	}

	if err := os.WriteFile(propPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write proposal file: %w", err)
	}

	return proposalID, nil
}

// validateSOPDAG checks that SOP step dependencies form a valid acyclic graph.
// It verifies that all DependsOn references point to existing steps and that
// the dependency graph contains no cycles.
func validateSOPDAG(steps map[string]schemas.SOPStep) error {
	// 1. Validate all DependsOn references exist
	for id, step := range steps {
		for _, dep := range step.DependsOn {
			if _, ok := steps[dep]; !ok {
				return fmt.Errorf("step %q depends on unknown step %q", id, dep)
			}
		}
	}

	// 2. Cycle detection via DFS with 3-color marking
	const (
		white = 0 // unvisited
		gray  = 1 // in progress (on current DFS path)
		black = 2 // fully processed
	)
	color := make(map[string]int, len(steps))

	var dfs func(id string) error
	dfs = func(id string) error {
		if color[id] == gray {
			return fmt.Errorf("cycle detected involving step %q", id)
		}
		if color[id] == black {
			return nil
		}
		color[id] = gray
		for _, dep := range steps[id].DependsOn {
			if err := dfs(dep); err != nil {
				return err
			}
		}
		color[id] = black
		return nil
	}

	// Visit every step to catch cycles in disconnected components
	for id := range steps {
		if color[id] == white {
			if err := dfs(id); err != nil {
				return err
			}
		}
	}
	return nil
}
