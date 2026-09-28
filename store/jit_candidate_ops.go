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
// build SampleCount until it reaches the retrieval threshold (minSamples=5),
// at which point the pattern becomes visible to QueryJITCandidates.
//
// This fixes the L5 double-breakage identified in
// docs/RSI_AUTONOMY_LEVELS_ASSESSMENT.md: Break 1 (map divergence) and
// Break 2 (threshold unreachable — now reachable via accumulation).
func (es *ExperienceStore) UpsertTaskPatternFromReplay(candidateID, sequenceKey, sourceText string) {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	now := time.Now()

	// Look for existing pattern with same SequenceKey (accumulation)
	for id, p := range es.TaskPatterns {
		if p.SequenceKey == sequenceKey && sequenceKey != "" {
			p.SampleCount++
			p.AvgConfidence = (p.AvgConfidence*float64(p.SampleCount-1) + 0.5) / float64(p.SampleCount)
			p.LastSeen = now
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
		AvgConfidence: 0.5,
		LastSeen:      now,
	}
}
