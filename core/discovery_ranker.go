package core

import (
	"context"
	"sort"
)

// SimpleRanker merges candidates from multiple sources and picks the highest confidence for each item.
type SimpleRanker struct {
	ConfidenceThreshold float64
}

func NewSimpleRanker(threshold float64) *SimpleRanker {
	return &SimpleRanker{ConfidenceThreshold: threshold}
}

func (r *SimpleRanker) Rank(ctx context.Context, query string, candidates []DiscoveryCandidate) []DiscoveryCandidate {
	// 1. Merge duplicates
	merged := make(map[string]DiscoveryCandidate)
	for _, c := range candidates {
		key := string(c.Type) + ":" + c.ID
		if existing, ok := merged[key]; ok {
			// Keep highest confidence and merge sources for audit
			if c.Confidence > existing.Confidence {
				c.Source = existing.Source + "," + c.Source
				merged[key] = c
			} else {
				existing.Source = existing.Source + "," + c.Source
				merged[key] = existing
			}
		} else {
			merged[key] = c
		}
	}

	// 2. Filter by threshold
	var result []DiscoveryCandidate
	for _, c := range merged {
		if c.Confidence >= r.ConfidenceThreshold {
			result = append(result, c)
		}
	}

	// 3. Sort by confidence descending, then by Name
	sort.Slice(result, func(i, j int) bool {
		if result[i].Confidence != result[j].Confidence {
			return result[i].Confidence > result[j].Confidence
		}
		return result[i].Name < result[j].Name
	})

	return result
}
