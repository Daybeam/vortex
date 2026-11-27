package chat

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daybeam/vortex/store"
)

// UserProfileIndexer asynchronously extracts user preferences from chat
// messages and persists them to MemoryBankStore.UserProfile. It replaces
// the synchronous FactCache.ExtractAndInject path with an incremental,
// batch-oriented worker.
//
// Trigger model (spec §3):
//   - Incremental: flush immediately when N new user messages accumulate
//   - Timer fallback: flush on interval if any messages are pending
//   - Zero pending → timer skips (no work to do)
//
// Extraction reuses FactCache's factPathRe / factPreferRe / mergeUnique
// to avoid divergent regex logic. Only user-role messages are processed.
//
// Lifecycle: Start launches a ticker goroutine; Stop cancels the context
// and waits for any in-flight flush to complete. The indexer is safe for
// concurrent OnUserMessage calls.
type UserProfileIndexer struct {
	store     *store.MemoryBankStore
	threshold int
	interval  time.Duration

	msgCounter atomic.Int64
	pendingMu  sync.Mutex
	pending    []ChatMessage

	flushMu sync.Mutex
	flushWg sync.WaitGroup

	ctx     context.Context
	cancel  context.CancelFunc
	stopped atomic.Bool
}

// DefaultUserProfileIndexerThreshold is the number of new user messages
// that triggers an immediate flush. Spec §3 recommends 10.
const DefaultUserProfileIndexerThreshold = 10

// DefaultUserProfileIndexerInterval is the fallback flush interval.
// Spec §3 recommends 1h.
const DefaultUserProfileIndexerInterval = time.Hour

// NewUserProfileIndexer creates an indexer with the given store and
// configuration. Zero values fall back to defaults.
func NewUserProfileIndexer(mbStore *store.MemoryBankStore, threshold int, interval time.Duration) *UserProfileIndexer {
	if threshold <= 0 {
		threshold = DefaultUserProfileIndexerThreshold
	}
	if interval <= 0 {
		interval = DefaultUserProfileIndexerInterval
	}
	return &UserProfileIndexer{
		store:     mbStore,
		threshold: threshold,
		interval:  interval,
	}
}

// OnUserMessage records a user message for incremental processing.
// When the pending count reaches the threshold, flush is triggered
// immediately (non-blocking — flush runs in its own goroutine).
func (u *UserProfileIndexer) OnUserMessage(msg ChatMessage) {
	if u == nil || u.store == nil {
		return
	}
	if msg.Role != "user" {
		return
	}
	u.pendingMu.Lock()
	u.pending = append(u.pending, msg)
	u.pendingMu.Unlock()

	if u.msgCounter.Add(1) >= int64(u.threshold) {
		u.flushWg.Add(1)
		go func() {
			defer u.flushWg.Done()
			u.flush()
		}()
	}
}

// Start launches the timer-fallback goroutine. The goroutine exits when
// ctx is cancelled or Stop is called.
func (u *UserProfileIndexer) Start(ctx context.Context) {
	if u == nil {
		return
	}
	u.ctx, u.cancel = context.WithCancel(ctx)
	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()

	for {
		select {
		case <-u.ctx.Done():
			return
		case <-ticker.C:
			if u.msgCounter.Load() == 0 {
				continue
			}
			u.flushWg.Add(1)
			func() {
				defer u.flushWg.Done()
				u.flush()
			}()
		}
	}
}

// Stop cancels the indexer and waits for any in-flight flush to complete.
// Safe to call multiple times.
func (u *UserProfileIndexer) Stop() {
	if u == nil {
		return
	}
	if !u.stopped.CompareAndSwap(false, true) {
		return
	}
	if u.cancel != nil {
		u.cancel()
	}
	u.flushWg.Wait()
}

// flush extracts preferences from all pending user messages, merges them
// into UserProfile.Preferences, and persists. The flushMu serializes
// concurrent flush calls (incremental + timer).
func (u *UserProfileIndexer) flush() {
	u.flushMu.Lock()
	defer u.flushMu.Unlock()

	u.pendingMu.Lock()
	msgs := u.pending
	u.pending = nil
	u.pendingMu.Unlock()

	if len(msgs) == 0 {
		u.msgCounter.Store(0)
		return
	}

	facts := extractPreferences(msgs)
	if len(facts) == 0 {
		u.msgCounter.Store(0)
		return
	}

	if u.store != nil {
		if mb, err := u.store.Load(); err == nil {
			merged := mergeUnique(mb.UserProfile.Preferences, facts)
			up := mb.UserProfile
			up.Preferences = merged
			if err := u.store.SaveUserProfile(up); err != nil {
				log.Printf("WARN: user_profile_indexer: failed to save user profile: %v", err)
			}
		} else {
			log.Printf("WARN: user_profile_indexer: failed to load memory bank: %v", err)
		}
	}

	u.msgCounter.Store(0)
}

// extractPreferences runs the same regex logic as FactCache.ExtractAndInject
// but only on the provided messages (not full history). Returns deduplicated
// preference facts.
func extractPreferences(msgs []ChatMessage) []string {
	seen := make(map[string]bool)
	var facts []string

	for _, msg := range msgs {
		if msg.Role != "user" {
			continue
		}
		for _, match := range factPathRe.FindAllString(msg.Content, -1) {
			if !seen[match] {
				seen[match] = true
				facts = append(facts, "path: "+match)
			}
		}
		for _, match := range factPreferRe.FindAllStringSubmatch(msg.Content, -1) {
			fact := "prefers " + match[1]
			if !seen[fact] {
				seen[fact] = true
				facts = append(facts, fact)
			}
		}
	}
	return facts
}
