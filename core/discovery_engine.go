package core

import (
	"context"
	"sync"
)

// CandidateType identifies the kind of item discovered.
type CandidateType string

const (
	CandidateRole       CandidateType = "role"
	CandidateSkill      CandidateType = "skill"
	CandidateGroup      CandidateType = "group"
	CandidateSOP        CandidateType = "sop"
	CandidateMCP        CandidateType = "mcp"
	CandidateCapability CandidateType = "capability"
)

// DiscoveryCandidate represents a single matched item in the discovery process.
type DiscoveryCandidate struct {
	ID          string         `json:"id"`
	Type        CandidateType  `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Confidence  float64        `json:"confidence"` // 0.0 to 1.0
	Source      string         `json:"source"`     // e.g., "filter", "keyword", "semantic"
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Retriever defines the interface for an atomic discovery component.
type Retriever interface {
	// Recall returns a set of candidates based on the query and pre-extracted tags.
	Recall(ctx context.Context, query string, tags []string) []DiscoveryCandidate
}

// Ranker defines the interface for merging and scoring candidates.
type Ranker interface {
	// Rank merges candidates from multiple retrievers and applies final weighting.
	Rank(ctx context.Context, query string, candidates []DiscoveryCandidate) []DiscoveryCandidate
}

// DiscoveryPipeline orchestrates the discovery process from Recall to Rank.
type DiscoveryPipeline struct {
	Retrievers []Retriever
	Ranker     Ranker
}

func NewDiscoveryPipeline(retrievers []Retriever, ranker Ranker) *DiscoveryPipeline {
	return &DiscoveryPipeline{
		Retrievers: retrievers,
		Ranker:     ranker,
	}
}

// Execute runs the full discovery pipeline.
func (p *DiscoveryPipeline) Execute(ctx context.Context, query string, tags []string) []DiscoveryCandidate {
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		candidates []DiscoveryCandidate
	)

	// Phase 1: Parallel Recall
	for _, r := range p.Retrievers {
		wg.Add(1)
		go func(ret Retriever) {
			defer wg.Add(-1)
			results := ret.Recall(ctx, query, tags)
			mu.Lock()
			candidates = append(candidates, results...)
			mu.Unlock()
		}(r)
	}
	wg.Wait()

	// Phase 2: Ranking & Merging
	return p.Ranker.Rank(ctx, query, candidates)
}
