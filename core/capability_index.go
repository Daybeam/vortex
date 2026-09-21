package core

import (
	"strings"
	"sync"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/filters"
)

// CapabilityIndex provides a fast-fail lookup for capability tags.
// It uses Cuckoo filters to quickly determine if a given intent/tag
// might be supported by the current system configuration.
type CapabilityIndex struct {
	Mu     sync.RWMutex
	Filter filters.ToolFilter
	Tags   map[string]bool // Set of all known capability tags
}

func NewCapabilityIndex(reg *config.Registry) *CapabilityIndex {
	idx := &CapabilityIndex{
		Tags: make(map[string]bool),
	}
	idx.Rebuild(reg)
	return idx
}

// Rebuild reconstructs the index from the current registry state.
func (idx *CapabilityIndex) Rebuild(reg *config.Registry) {
	idx.Mu.Lock()
	defer idx.Mu.Unlock()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	// 1. Collect all tags
	newTags := make(map[string]bool)

	// Helper to add tokens as tags
	addTokens := func(s string) {
		if s == "" {
			return
		}
		// Separators: space, comma, bang, etc. (Dot is NOT a separator to support research.paper)
		tokens := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return r == ' ' || r == ',' || r == '!' || r == '?' || r == ';' || r == ':' || r == '(' || r == ')'
		})
		for _, t := range tokens {
			if len(t) >= 3 {
				newTags[t] = true
			}
		}
	}

	// Roles
	for _, r := range reg.Roles {
		addTokens(r.BaseCapability)
	}

	// Skills
	for _, s := range reg.Skills {
		addTokens(s.Capability)
	}

	// MCPs
	for _, m := range reg.MCPs {
		for _, p := range m.Provides {
			addTokens(p)
		}
	}

	// Dynamic MCPs
	for _, m := range reg.DynamicMCPs {
		for _, tool := range m.AvailableTools {
			addTokens(tool)
		}
	}

	idx.Tags = newTags

	// 2. Build Cuckoo Filter
	expected := uint(len(newTags))
	if expected == 0 {
		expected = 100
	}
	idx.Filter = filters.NewCuckooToolFilter(expected)
	for tag := range newTags {
		idx.Filter.Add(tag)
	}
}

// Maybe returns true if the tag might be supported.
func (idx *CapabilityIndex) Maybe(tag string) bool {
	idx.Mu.RLock()
	defer idx.Mu.RUnlock()

	if idx.Filter == nil {
		return true // Fail open if no filter
	}
	return idx.Filter.Test(strings.ToLower(tag))
}

// ListKnown returns all tags in the index.
func (idx *CapabilityIndex) ListKnown() []string {
	idx.Mu.RLock()
	defer idx.Mu.RUnlock()

	res := make([]string, 0, len(idx.Tags))
	for t := range idx.Tags {
		res = append(res, t)
	}
	return res
}
