package store

import (
	"context"
	"time"
)

func (es *ExperienceStore) AddJITCandidate(ctx context.Context, c *JITCandidate) error {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	if c.LastSeen.IsZero() {
		c.LastSeen = time.Now()
	}
	if c.Status == "" {
		c.Status = "pending"
	}

	es.JITCandidates[c.ID] = c
	return nil
}

func (es *ExperienceStore) GetJITCandidates() map[string]*JITCandidate {
	es.Mu.RLock()
	defer es.Mu.RUnlock()
	return es.JITCandidates
}

func (es *ExperienceStore) AddPromotionAuditLog(log PromotionAuditLog) {
	es.Mu.Lock()
	defer es.Mu.Unlock()
	es.PromotionAuditLogs = append(es.PromotionAuditLogs, log)
	if max := es.getMaxPromotionAuditLogs(); len(es.PromotionAuditLogs) > max {
		es.PromotionAuditLogs = es.PromotionAuditLogs[len(es.PromotionAuditLogs)-max:]
	}
}

// UpsertTaskPatternFromReplay writes a TaskPattern derived from a replay
// scheduler mutation. Unlike AddJITCandidate (which writes to the parallel
// JITCandidates map that QueryJITCandidates never reads), this writes to
// TaskPatterns — the map that QueryJITCandidates → QuerySimilarPatterns
// actually scans.
//
// Accumulation semantics: if a TaskPattern with the same SequenceKey already
// exists, SampleCount is incremented and AvgConfidence is updated as a
// running average. This lets repeated replay cycles (every 6h) naturally
// build SampleCount until it reaches the retrieval threshold (minSamples=5).
//
// verified controls the confidence value fed into the running average:
//   - verified=true  → 0.8 (cross-family verified mutation)
//   - verified=false → 0.5 (unverified/pending mutation)
// After enough verified cycles, AvgConfidence converges toward 0.8 > 0.7 gate.
//
// sourceText is the original task text, used as SourceText for BM25 matching
// in QuerySimilarPatterns. This must be the real task description (e.g.
// "build a widget"), NOT a generic string like "rewrite mutation from replay".
//
// This fixes the L5 triple-breakage identified in
// docs/RSI_AUTONOMY_LEVELS_ASSESSMENT.md: Break 1 (map divergence),
// Break 2 (SampleCount accumulation), Break 3 (AvgConfidence gate),
// Break 4 (BM25 SourceText mismatch).
func (es *ExperienceStore) UpsertTaskPatternFromReplay(candidateID, sequenceKey, sourceText string, verified bool) {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	now := time.Now()
	newConf := 0.5
	if verified {
		newConf = 0.8
	}

	// Look for existing pattern with same SequenceKey (accumulation)
	// audit M-6: use sequenceKeyIndex for O(1) lookup instead of O(N) scan.
	// Rebuild if nil (test setups that bypass NewExperienceStore).
	if sequenceKey != "" {
		if es.sequenceKeyIndex == nil {
			es.rebuildSequenceKeyIndexLocked()
		}
		if id, ok := es.sequenceKeyIndex[sequenceKey]; ok {
			p := es.TaskPatterns[id]
			p.SampleCount++
			p.AvgConfidence = (p.AvgConfidence*float64(p.SampleCount-1) + newConf) / float64(p.SampleCount)
			p.LastSeen = now
			// Update SourceText if the new one is more descriptive
			if sourceText != "" && len(sourceText) > len(p.SourceText) {
				p.SourceText = sourceText
			}
			es.TaskPatterns[id] = p // write back modified value
			return
		}
	}

	// No existing pattern — create new one
	es.TaskPatterns[candidateID] = TaskPattern{
		ID:            candidateID,
		SequenceKey:   sequenceKey,
		SourceText:    sourceText,
		SampleCount:   1,
		AvgConfidence: newConf,
		LastSeen:      now,
	}
	// audit M-6: maintain sequenceKeyIndex
	if sequenceKey != "" {
		es.sequenceKeyIndex[sequenceKey] = candidateID
	}
}
