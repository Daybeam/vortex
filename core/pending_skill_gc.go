package core

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
)

// PendingSkillGC periodically sweeps Registry.PendingSkills and removes entries
// whose StagedAt + TTL has expired. For each expired entry it:
//  1. Removes the skill directory from disk (os.RemoveAll) — no orphan residue.
//  2. Deletes the entry from PendingSkills — freed from memory.
//
// Promoted skills (moved to Registry.Skills via promote_skill) are no longer in
// PendingSkills, so they are never swept. This is the safety property: once
// promoted, the TTL timer no longer applies.
//
// TTL source: Registry.System.StagingRetentionHours (default 168h = 7 days).
// Sweep interval: 1 hour (hardcoded — non-hot path, matches AssetManager).
//
// See docs/plan/SKILL_MARKETPLACE_INTEGRATION_DESIGN.md TODO-5.
type PendingSkillGC struct {
	registry *config.Registry
	stopChan chan struct{}
	stopOnce sync.Once
}

// NewPendingSkillGC creates a GC sweeper for the given registry.
func NewPendingSkillGC(reg *config.Registry) *PendingSkillGC {
	return &PendingSkillGC{
		registry: reg,
		stopChan: make(chan struct{}),
	}
}

// Start launches the periodic sweep goroutine.
func (g *PendingSkillGC) Start() {
	go g.loop()
}

// Stop signals the sweep goroutine to exit. Idempotent.
func (g *PendingSkillGC) Stop() {
	g.stopOnce.Do(func() {
		close(g.stopChan)
	})
}

// loop is the periodic sweep goroutine.
func (g *PendingSkillGC) loop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[pending-skill-gc] goroutine panic: %v\n%s", r, debug.Stack())
		}
	}()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopChan:
			return
		case <-ticker.C:
			g.sweep()
		}
	}
}

// sweep removes expired pending skills from both disk and memory.
// Returns the number of expired entries removed.
func (g *PendingSkillGC) sweep() int {
	ttl := time.Duration(g.registry.System.StagingRetentionHours) * time.Hour
	if ttl <= 0 {
		ttl = 168 * time.Hour // default 7 days
	}
	now := time.Now()

	g.registry.Mu.Lock()
	defer g.registry.Mu.Unlock()

	removed := 0
	for id, skill := range g.registry.PendingSkills {
		if skill.StagedAt.IsZero() {
			continue // no timestamp — skip (shouldn't happen for staged skills)
		}
		if now.Sub(skill.StagedAt) < ttl {
			continue // not expired yet
		}

		// 1. Remove skill directory from disk
		if skill.SkillDir != "" {
			if err := os.RemoveAll(skill.SkillDir); err != nil {
				log.Printf("[pending-skill-gc] failed to remove %s: %v", skill.SkillDir, err)
				// Still remove from memory — disk cleanup is best-effort
			}
		}

		// 2. Remove from PendingSkills
		delete(g.registry.PendingSkills, id)
		removed++

		log.Printf("[pending-skill-gc] expired pending skill %q (dir=%s)", id, skill.SkillDir)
	}

	if removed > 0 {
		fmt.Fprintf(os.Stderr, "[pending-skill-gc] removed %d expired pending skills\n", removed)
	}

	return removed
}

// SweepOnce performs a single sweep without starting the goroutine.
// Useful for testing or on-demand cleanup.
func (g *PendingSkillGC) SweepOnce() int {
	return g.sweep()
}
