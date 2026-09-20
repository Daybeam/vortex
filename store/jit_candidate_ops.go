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
