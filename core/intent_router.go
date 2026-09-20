package core

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// EmbeddingClient defines the interface for generating text embeddings.
type EmbeddingClient interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// IntentRouter handles semantic routing of user input to SOPs with concurrency racing,
// caching, config hot-reloading, and graceful degradation (fallback to keyword matching).
type IntentRouter struct {
	mu          sync.RWMutex
	embeddings  map[string][]float32 // SOP ID -> embedding vector
	sopVersions map[string]string    // SOP ID -> version/hash tracking for hot-reload
	embedClient EmbeddingClient
}

type SOPRouteResult struct {
	SOP        *schemas.SOP
	Confidence float64
	MatchMode  string // "semantic_embedding" or "keyword_fallback"
}

func NewIntentRouter(embedClient EmbeddingClient) *IntentRouter {
	return &IntentRouter{
		embeddings:  make(map[string][]float32),
		sopVersions: make(map[string]string),
		embedClient: embedClient,
	}
}

// RefreshEmbeddings updates SOP vector cache incrementally on reload.
// Network I/O (embedding generation) is performed outside the lock to avoid
// blocking concurrent Route callers.
func (r *IntentRouter) RefreshEmbeddings(ctx context.Context, reg *config.Registry) {
	if r.embedClient == nil || reg == nil {
		return
	}

	reg.Mu.RLock()
	sopsCopy := make(map[string]*schemas.SOP, len(reg.SOPs))
	for k, v := range reg.SOPs {
		sopsCopy[k] = v
	}
	reg.Mu.RUnlock()

	// Step 1: Under read lock, identify which SOPs need re-embedding.
	type pendingEmbed struct {
		id         string
		versionKey string
		text       string
	}
	var pending []pendingEmbed

	r.mu.RLock()
	for id, sop := range sopsCopy {
		if sop == nil {
			continue
		}
		versionKey := sop.Version + "_" + strconv.Itoa(len(sop.Description))
		if r.sopVersions[id] == versionKey && r.embeddings[id] != nil {
			continue // Unchanged, skip re-embedding
		}

		// Build rich embedding text
		text := "[SOP:" + id + "] " + sop.Description + " "
		for _, trigger := range sop.Triggers {
			for _, kw := range trigger.Keywords {
				text += kw + " "
			}
		}
		for _, t := range sop.TypicalTasks {
			text += t + " "
		}
		for _, tag := range sop.Tags {
			text += tag + " "
		}
		pending = append(pending, pendingEmbed{id: id, versionKey: versionKey, text: text})
	}
	r.mu.RUnlock()

	// Step 2: Compute embeddings outside any lock (network I/O).
	type embedResult struct {
		id         string
		versionKey string
		vec        []float32
	}
	var results []embedResult

	for _, p := range pending {
		embedCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if vec, err := r.embedClient.Embed(embedCtx, p.text); err == nil && len(vec) > 0 {
			results = append(results, embedResult{id: p.id, versionKey: p.versionKey, vec: vec})
		}
		cancel()
	}

	// Step 3: Batch-update maps under a short write lock.
	if len(results) > 0 {
		r.mu.Lock()
		for _, res := range results {
			r.embeddings[res.id] = res.vec
			r.sopVersions[res.id] = res.versionKey
		}
		r.mu.Unlock()
	}
}

// Route matches user input against available SOPs using concurrency racing (embedding vs keyword)
// and handles graceful degradation when embedding service is unavailable or confidence is low.
func (r *IntentRouter) Route(ctx context.Context, reg *config.Registry, userInput string, maxResults int) ([]SOPRouteResult, error) {
	if reg == nil || userInput == "" {
		return nil, errors.New("invalid registry or empty user input")
	}

	type raceResult struct {
		results []SOPRouteResult
		err     error
	}

	embedChan := make(chan raceResult, 1)
	keywordChan := make(chan raceResult, 1)

	// Race 1: Semantic Embedding Matching
	go func() {
		r.mu.RLock()
		hasClient := r.embedClient != nil
		hasEmbeddings := len(r.embeddings) > 0
		r.mu.RUnlock()

		if !hasClient || !hasEmbeddings {
			embedChan <- raceResult{err: errors.New("embedding client or cache not ready")}
			return
		}

		embedCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond) // Fast timeout for race
		defer cancel()

		userVec, err := r.embedClient.Embed(embedCtx, userInput)
		if err != nil || len(userVec) == 0 {
			embedChan <- raceResult{err: errors.New("failed to embed user input")}
			return
		}

		reg.Mu.RLock()
		sopsCopy := make(map[string]*schemas.SOP, len(reg.SOPs))
		for k, v := range reg.SOPs {
			sopsCopy[k] = v
		}
		reg.Mu.RUnlock()

		r.mu.RLock()
		type scoredSOP struct {
			sop        *schemas.SOP
			confidence float64
		}
		var scored []scoredSOP

		for id, vec := range r.embeddings {
			if sop, exists := sopsCopy[id]; exists && sop != nil {
				sim := cosineSimilarity(userVec, vec)
				if sim >= 0.45 { // Confidence threshold
					scored = append(scored, scoredSOP{sop: sop, confidence: sim})
				}
			}
		}
		r.mu.RUnlock()

		if len(scored) == 0 {
			embedChan <- raceResult{err: errors.New("no semantic matches above confidence threshold")}
			return
		}

		// Sort by confidence descending (audit: was O(N²) bubble sort → sort.Slice)
		sort.Slice(scored, func(i, j int) bool {
			return scored[i].confidence > scored[j].confidence
		})

		var res []SOPRouteResult
		for i, s := range scored {
			if i >= maxResults {
				break
			}
			res = append(res, SOPRouteResult{
				SOP:        s.sop,
				Confidence: s.confidence,
				MatchMode:  "semantic_embedding",
			})
		}
		embedChan <- raceResult{results: res}
	}()

	// Race 2: Keyword Fallback (Instant & Reliable)
	go func() {
		candidates := MatchSOPCandidates(reg, userInput, maxResults)
		if len(candidates) == 0 {
			keywordChan <- raceResult{err: errors.New("no keyword matches")}
			return
		}
		var res []SOPRouteResult
		for _, c := range candidates {
			res = append(res, SOPRouteResult{
				SOP:        c.SOP,
				Confidence: float64(c.Hits) / 10.0, // Normalized pseudo-confidence
				MatchMode:  "keyword_fallback",
			})
		}
		keywordChan <- raceResult{results: res}
	}()

	// Concurrency select race logic
	select {
	case embRes := <-embedChan:
		if embRes.err == nil && len(embRes.results) > 0 {
			return embRes.results, nil
		}
		// Fallback to keyword if embedding failed
		select {
		case kwRes := <-keywordChan:
			if kwRes.err == nil {
				return kwRes.results, nil
			}
			return nil, embRes.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case kwRes := <-keywordChan:
		// If keyword finished first, give a brief window for embedding to attempt superior match
		select {
		case embRes := <-embedChan:
			if embRes.err == nil && len(embRes.results) > 0 {
				return embRes.results, nil
			}
		case <-time.After(250 * time.Millisecond):
		}
		if kwRes.err == nil {
			return kwRes.results, nil
		}
		select {
		case embRes := <-embedChan:
			if embRes.err == nil {
				return embRes.results, nil
			}
			return nil, kwRes.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
