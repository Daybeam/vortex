package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

type ScheduleManager struct {
	Mu            sync.RWMutex
	scheduler     OrchestrationEngine
	expStore      store.IExperienceStore
	scheduleStore *store.ScheduleStore
	logger        *Logger
	stopChan      chan struct{}
	stopOnce      sync.Once
	lastLoaded    time.Time
	wg            sync.WaitGroup // audit M8: track all goroutines
}

func NewScheduleManager(
	s OrchestrationEngine,
	es store.IExperienceStore,
	ss *store.ScheduleStore,
	logger *Logger,
) *ScheduleManager {
	return &ScheduleManager{
		scheduler:     s,
		expStore:      es,
		scheduleStore: ss,
		logger:        logger,
		stopChan:      make(chan struct{}),
	}
}

func (sm *ScheduleManager) Start() {
	sm.wg.Add(1)
	go func() {
		defer sm.wg.Done()
		sm.loop()
	}()
}

func (sm *ScheduleManager) Stop() {
	sm.stopOnce.Do(func() { close(sm.stopChan) })
	sm.wg.Wait() // audit M8: wait for loop, trigger, and monitor goroutines
}

func (sm *ScheduleManager) loop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sm.stopChan:
			return
		case <-ticker.C:
			sm.scan()
		}
	}
}

func (sm *ScheduleManager) scan() {
	// 1. Check for file updates (mtime)
	sm.checkReload()

	// 2. Cleanup old locks occasionally
	if time.Since(sm.lastLoaded).Minutes() > 60 {
		sm.cleanupLocks()
	}

	// 3. Scan and trigger
	now := time.Now()
	schedules := sm.scheduleStore.List()

	for _, s := range schedules {
		if s.Status != schemas.ScheduleActive {
			continue
		}

		if !s.NextRun.IsZero() && now.After(s.NextRun) {
			// Acquire distributed lock before triggering
			if sm.acquireLock(s.ID, s.NextRun) {
				sm.wg.Add(1)
				go func() {
					defer sm.wg.Done()
					sm.trigger(s)
				}()
			} else {
				// Log collision/skip on debug level or similar
				sm.logger.Log("schedule_lock_skip", "", "", map[string]any{
					"schedule_id": s.ID,
					"reason":      "locked_by_another_instance",
				})

				// Optional: update local NextRun to match store to avoid repeated attempts
				// while another instance is working.
				latest := sm.scheduleStore.Get(s.ID)
				if latest != nil {
					s.NextRun = latest.NextRun
				}
			}
		}
	}
}

func (sm *ScheduleManager) acquireLock(id string, timestamp time.Time) bool {
	lockDir := filepath.Join(filepath.Dir(sm.scheduleStore.GetPath()), "locks")
	_ = os.MkdirAll(lockDir, 0755)

	// Format: schedule_{ID}_{YYYYMMDDHHMM}.lock
	// Use the scheduled time, not current time, to ensure all pods agree on the same lock key
	lockName := fmt.Sprintf("schedule_%s_%s.lock", id, timestamp.Format("200601021504"))
	lockPath := filepath.Join(lockDir, lockName)

	// O_EXCL ensures atomic creation. If file exists, OpenFile fails.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return false
	}
	defer f.Close()

	// Write owner info for audit (pod name if available)
	owner := os.Getenv("POD_NAME")
	if owner == "" {
		owner, _ = os.Hostname()
	}
	_, _ = f.WriteString(fmt.Sprintf("Owner: %s\nAcquired: %s", owner, time.Now().Format(time.RFC3339)))

	return true
}

func (sm *ScheduleManager) cleanupLocks() {
	lockDir := filepath.Join(filepath.Dir(sm.scheduleStore.GetPath()), "locks")
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(lockDir, entry.Name()))
		}
	}
}

func (sm *ScheduleManager) checkReload() {
	info, err := os.Stat(sm.scheduleStore.GetPath())
	if err != nil {
		return
	}

	if info.ModTime().After(sm.lastLoaded) {
		sm.scheduleStore.Reload()
		sm.lastLoaded = info.ModTime()
		sm.logger.Log("schedules_reloaded", "", "", map[string]any{
			"mtime": info.ModTime(),
		})
	}
}

func (sm *ScheduleManager) trigger(s *schemas.Schedule) {
	sm.logger.Log("schedule_triggered", "", "", map[string]any{
		"schedule_id":   s.ID,
		"schedule_name": s.Name,
	})

	// Submit to scheduler
	taskID, err := sm.scheduler.Submit(s.TaskInputs)

	sm.Mu.Lock()
	defer sm.Mu.Unlock()

	now := time.Now()
	s.LastRun = &now
	s.RunCount++

	if err != nil {
		s.ConsecutiveFailures++
		sm.logger.Log("schedule_submission_failed", "", "", map[string]any{
			"schedule_id": s.ID,
			"error":       err.Error(),
		})
	} else {
		// Monitor for actual completion — tie lifetime to stopChan so
		// shutdown cancels pending monitors (audit finding H1).
		// audit M8: track goroutines with wg so Stop() waits for them.
		ctx, cancel := context.WithCancel(context.Background())
		sm.wg.Add(2)
		go func() {
			defer sm.wg.Done()
			select {
			case <-sm.stopChan:
				cancel()
			case <-ctx.Done():
			}
		}()
		go func() {
			defer sm.wg.Done()
			sm.monitorTask(ctx, taskID, s.ID)
		}()
	}

	// Calculate next run
	s.NextRun = sm.calculateNextRun(s)

	// Check MaxRuns
	if s.MaxRuns > 0 && s.RunCount >= s.MaxRuns {
		s.Status = schemas.ScheduleCompleted
	}

	sm.scheduleStore.Set(s)
}

func (sm *ScheduleManager) monitorTask(ctx context.Context, taskID string, scheduleID string) {
	// Event-driven via WaitTask (uses doneChans internally) with 5s timeout fallback.
	// Eliminates the fixed 5s polling delay — returns immediately when the task
	// signals completion via doneChan.
	for {
		result, ok := sm.scheduler.WaitTask(ctx, taskID, 5*time.Second)
		if !ok {
			return
		}
		graphStatus, ok := result["status"].(schemas.GraphStatus)
		if !ok {
			// Status missing or wrong type — skip this iteration safely
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if graphStatus == schemas.GraphCompleted || graphStatus == schemas.GraphFailed {
			sm.afterRun(scheduleID, graphStatus == schemas.GraphCompleted)
			return
		}
		// GraphBlocked: WaitTask returns immediately (fast path), so sleep
		// to avoid busy-looping while waiting for a decision to unblock.
		if graphStatus == schemas.GraphBlocked {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (sm *ScheduleManager) afterRun(scheduleID string, success bool) {
	s := sm.scheduleStore.Get(scheduleID)
	if s == nil {
		return
	}

	sm.Mu.Lock()
	defer sm.Mu.Unlock()

	if !success {
		s.ConsecutiveFailures++
		if s.ConsecutiveFailures >= 3 {
			sm.applyAdaptiveRecovery(s)
		}
	} else {
		s.ConsecutiveFailures = 0
	}
	sm.scheduleStore.Set(s)
}

func (sm *ScheduleManager) applyAdaptiveRecovery(s *schemas.Schedule) {
	if len(s.TaskInputs) == 0 {
		return
	}

	// Query experience store for advice
	roleID := s.TaskInputs[0].RoleID
	advice := sm.expStore.QueryRoleAdvice(context.Background(), roleID)

	if combos, ok := advice["recommended_skill_combos"].([][]string); ok && len(combos) > 0 {
		// Attempt to use the first recommended skill combo for the primary step
		s.TaskInputs[0].AdditionalSkills = combos[0]

		sm.logger.Log("schedule_adaptive_recovery", "", "", map[string]any{
			"schedule_id": s.ID,
			"new_skills":  combos[0],
		})
	}
}

func (sm *ScheduleManager) calculateNextRun(s *schemas.Schedule) time.Time {
	switch s.Type {
	case schemas.ScheduleOnce:
		return time.Time{} // No next run after execution
	case schemas.ScheduleInterval:
		d, err := time.ParseDuration(s.Interval)
		if err != nil {
			sm.logger.Log("schedule_config_error", "", "", map[string]any{
				"schedule_id": s.ID,
				"error":       "invalid interval: " + s.Interval,
			})
			return time.Time{}
		}
		return time.Now().Add(d)
	case schemas.ScheduleCron:
		next, err := sm.parseCron(s.CronExpr, time.Now())
		if err != nil {
			sm.logger.Log("schedule_config_error", "", "", map[string]any{
				"schedule_id": s.ID,
				"error":       "invalid cron: " + s.CronExpr + " (" + err.Error() + ")",
			})
			return time.Now().Add(24 * time.Hour) // Fallback
		}
		return next
	}
	return time.Time{}
}

func (sm *ScheduleManager) parseCron(expr string, from time.Time) (time.Time, error) {
	// Simple Cron Parser for standard 5-field format: min hour day month weekday
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("cron expression must have 5 fields")
	}

	// In a real project, use github.com/robfig/cron/v3.
	// For this audit completion, we implement a basic "next-minute/next-hour" logic
	// if the expression is simple (like "* * * * *"), otherwise fallback to 1h for now.

	// If it's all stars, next minute
	if expr == "* * * * *" {
		return from.Add(time.Minute).Truncate(time.Minute), nil
	}

	// If it's "0 * * * *", next hour
	if fields[0] == "0" && fields[1] == "*" && fields[2] == "*" && fields[3] == "*" && fields[4] == "*" {
		return from.Add(time.Hour).Truncate(time.Hour), nil
	}

	// FIX (playbook addendum, found during audit of the "完成 cron 解析器"
	// claim): the original placeholder here silently returned
	// `from.Add(time.Hour)` with a nil error for ANY 5-field expression that
	// wasn't one of the two hardcoded literal patterns above (e.g. a real
	// schedule like "0 9 * * 1-5" for weekday mornings would silently run
	// hourly instead, with no error surfaced anywhere). Only "* * * * *" and
	// "minute-hour wildcards" are genuinely supported today; anything else
	// must fail loudly (as an error, which the caller already logs as
	// schedule_config_error and falls back to a visible 24h retry for) rather
	// than silently producing a schedule the user never asked for. Replace
	// this branch with a real cron library (e.g. robfig/cron/v3) to support
	// the general case; until then, an honest error is strictly better than a
	// silently wrong next-run time for a scheduling feature.
	return time.Time{}, fmt.Errorf("cron expression %q is not one of the currently-supported patterns (\"* * * * *\" or \"0 * * * *\"); full cron parsing is not yet implemented", expr)
}
