package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// AntiPatternPrecedent captures a structural development pitfall — trigger
// conditions, symptom, resolution, and domain tags — so it can be surfaced
// to subagents as a protected, zero-compression warning block. ADDED
// (2026-08-27) as part of the ExperienceStore precedent injection pipeline.
type AntiPatternPrecedent struct {
	ID               string    `json:"id"`
	AntiPattern      string    `json:"anti_pattern"`
	CorrectPattern   string    `json:"correct_pattern"`
	TriggerCondition string    `json:"trigger_condition"`
	Symptom          string    `json:"symptom"`
	Category         string    `json:"category"`
	Confidence       float64   `json:"confidence"`
	Tags             []string  `json:"tags,omitempty"`
	SourceText       string    `json:"source_text,omitempty"`
	Embedding        []float32 `json:"embedding,omitempty"`
	EmbeddingModel   string    `json:"embedding_model,omitempty"`
	LastSeen         time.Time `json:"last_seen"`
	CreatedAt        time.Time `json:"created_at"`
}

// embedderResolver lazily resolves the current IEmbeddingClient from the
// parent ExperienceStore, enabling embedding injection after store init.
type embedderResolver func() IEmbeddingClient

// AntiPatternStore persists anti-pattern precedents and exposes query
// capabilities by keyword/tag. Embedding is resolved lazily via embedder
// so the parent ExperienceStore can inject its client after init.
type AntiPatternStore struct {
	embedder   embedderResolver
	backend    IAntiPatternBackend
	Mu         sync.RWMutex
	precedents []AntiPatternPrecedent
}

func NewAntiPatternStore(embedder embedderResolver, backend IAntiPatternBackend) *AntiPatternStore {
	aps := &AntiPatternStore{
		embedder: embedder,
		backend:  backend,
	}
	aps.Reload()
	return aps
}

func (s *AntiPatternStore) Reload() {
	if s.backend == nil {
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	list, err := s.backend.LoadAll(context.Background())
	if err == nil {
		s.precedents = list
	}
}

func (s *AntiPatternStore) Upsert(ctx context.Context, p AntiPatternPrecedent) {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.LastSeen.IsZero() {
		p.LastSeen = time.Now().UTC()
	}
	if p.ID == "" {
		p.ID = "prec_" + strings.ReplaceAll(p.AntiPattern, " ", "_")
	}

	if s.embedder != nil {
		cli := s.embedder()
		if cli != nil && p.SourceText != "" && len(p.Embedding) == 0 {
			vec, model, _ := cli.EmbedWithModel(ctx, p.SourceText)
			p.Embedding = vec
			p.EmbeddingModel = model
		}
	}

	for i, existing := range s.precedents {
		if existing.ID == p.ID {
			s.precedents[i] = p
			if s.backend != nil {
				s.backend.Upsert(ctx, p)
			}
			return
		}
	}
	s.precedents = append(s.precedents, p)
	if s.backend != nil {
		s.backend.Upsert(ctx, p)
	}
}

// QueryByCategory returns all precedents matching the given category
// (case-insensitive), ordered by descending confidence.
func (s *AntiPatternStore) QueryByCategory(category string) []AntiPatternPrecedent {
	s.Mu.RLock()
	defer s.Mu.RUnlock()

	var out []AntiPatternPrecedent
	catLower := strings.ToLower(category)
	for _, p := range s.precedents {
		if strings.ToLower(p.Category) == catLower {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Confidence > out[j].Confidence
	})
	return out
}

// QueryByKeyword returns precedents whose Symptom, AntiPattern, or
// CorrectPattern contains the given keyword (case-insensitive).
func (s *AntiPatternStore) QueryByKeyword(keyword string, limit int) []AntiPatternPrecedent {
	s.Mu.RLock()
	defer s.Mu.RUnlock()

	lower := strings.ToLower(keyword)
	var out []AntiPatternPrecedent
	for _, p := range s.precedents {
		if strings.Contains(strings.ToLower(p.AntiPattern), lower) ||
			strings.Contains(strings.ToLower(p.Symptom), lower) ||
			strings.Contains(strings.ToLower(p.CorrectPattern), lower) {
			out = append(out, p)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Confidence > out[j].Confidence
	})
	return out
}

// TopConfidence returns the highest-confidence precedents up to 'limit'.
func (s *AntiPatternStore) TopConfidence(limit int) []AntiPatternPrecedent {
	s.Mu.RLock()
	defer s.Mu.RUnlock()

	snapshot := make([]AntiPatternPrecedent, len(s.precedents))
	copy(snapshot, s.precedents)
	sort.Slice(snapshot, func(i, j int) bool {
		return snapshot[i].Confidence > snapshot[j].Confidence
	})
	if limit > 0 && len(snapshot) > limit {
		snapshot = snapshot[:limit]
	}
	return snapshot
}

// PrecedentCount returns the total number of stored precedents.
func (s *AntiPatternStore) PrecedentCount() int {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return len(s.precedents)
}

// AutoDepositDecisionRecovery creates a new AntiPatternPrecedent from a
// successful decision recovery and persists it. ADDED (2026-09-08) for
// Decision-Driven Experience Evolution.
func (s *AntiPatternStore) AutoDepositDecisionRecovery(ctx context.Context, intent, failureMode, trigger, symptom, recovery string) {
	p := AntiPatternPrecedent{
		AntiPattern:      failureMode,
		CorrectPattern:   recovery,
		TriggerCondition: trigger,
		Symptom:          symptom,
		Category:         "auto_recovery",
		Confidence:       0.8, // Initial confidence for auto-deposited patterns
		Tags:             []string{"auto_evolution", intent},
		SourceText:       fmt.Sprintf("%s: %s", failureMode, symptom),
		LastSeen:         time.Now().UTC(),
		CreatedAt:        time.Now().UTC(),
	}
	s.Upsert(ctx, p)
}

// Compile-time check.
var _ = schemas.ContentBlock{}
