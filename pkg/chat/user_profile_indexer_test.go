package chat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/store"
)

// Tests for UserProfileIndexer: incremental trigger, timer fallback,
// flush logic, and lifecycle.
// Spec: docs/completed/2026-11-27/USER_PROFILE_INCREMENTAL_INDEXER.md §2, §3

func TestUserProfileIndexer_IncrementalTrigger_FlushesAtThreshold(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	indexer := NewUserProfileIndexer(mbStore, 3, time.Hour)
	go indexer.Start(context.Background())
	defer indexer.Stop()

	for i := 0; i < 3; i++ {
		indexer.OnUserMessage(ChatMessage{
			Role:    "user",
			Content: "I prefer Go",
		})
	}

	// Wait for async flush to complete
	indexer.flushWg.Wait()

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "prefers Go") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("preference 'prefers Go' not found in UserProfile after threshold flush; got %v", mb.UserProfile.Preferences)
	}
}

func TestUserProfileIndexer_BelowThreshold_NoFlush(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	indexer := NewUserProfileIndexer(mbStore, 10, time.Hour)

	indexer.OnUserMessage(ChatMessage{
		Role:    "user",
		Content: "I prefer Rust",
	})

	// Give some time for any unexpected flush
	time.Sleep(50 * time.Millisecond)

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "prefers Rust") {
			t.Fatalf("preference should not be flushed below threshold; got %v", mb.UserProfile.Preferences)
		}
	}
}

func TestUserProfileIndexer_TimerFallback_FlushesPending(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	// Use a very short interval for testing
	indexer := NewUserProfileIndexer(mbStore, 100, 50*time.Millisecond)
	go indexer.Start(context.Background())
	defer indexer.Stop()

	indexer.OnUserMessage(ChatMessage{
		Role:    "user",
		Content: "I prefer Python",
	})

	// Wait for timer fallback to fire
	time.Sleep(200 * time.Millisecond)

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "prefers Python") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("preference 'prefers Python' not flushed by timer fallback; got %v", mb.UserProfile.Preferences)
	}
}

func TestUserProfileIndexer_TimerFallback_SkipsWhenNoPending(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	// Pre-populate with existing data
	up := store.UserProfile{Preferences: []string{"prefers existing"}}
	if err := mbStore.SaveUserProfile(up); err != nil {
		t.Fatalf("SaveUserProfile: %v", err)
	}

	indexer := NewUserProfileIndexer(mbStore, 100, 50*time.Millisecond)
	go indexer.Start(context.Background())
	defer indexer.Stop()

	// Wait for at least 2 timer ticks with no pending messages
	time.Sleep(150 * time.Millisecond)

	// Profile should be unchanged — no flush occurred
	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(mb.UserProfile.Preferences) != 1 || mb.UserProfile.Preferences[0] != "prefers existing" {
		t.Fatalf("profile should be unchanged when no pending messages; got %v", mb.UserProfile.Preferences)
	}
}

func TestUserProfileIndexer_IgnoresAssistantMessages(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	indexer := NewUserProfileIndexer(mbStore, 1, time.Hour)
	go indexer.Start(context.Background())
	defer indexer.Stop()

	// Assistant message should be ignored
	indexer.OnUserMessage(ChatMessage{
		Role:    "assistant",
		Content: "I prefer Java",
	})

	indexer.flushWg.Wait()

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "prefers Java") {
			t.Fatalf("assistant messages must not be processed; got %v", mb.UserProfile.Preferences)
		}
	}
}

func TestUserProfileIndexer_MergeUnique_DoesNotDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	// Pre-populate with existing preference
	up := store.UserProfile{Preferences: []string{"prefers Go"}}
	if err := mbStore.SaveUserProfile(up); err != nil {
		t.Fatalf("SaveUserProfile: %v", err)
	}

	indexer := NewUserProfileIndexer(mbStore, 1, time.Hour)
	go indexer.Start(context.Background())
	defer indexer.Stop()

	// Send the same preference again
	indexer.OnUserMessage(ChatMessage{
		Role:    "user",
		Content: "I prefer Go",
	})

	indexer.flushWg.Wait()

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	count := 0
	for _, p := range mb.UserProfile.Preferences {
		if p == "prefers Go" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("mergeUnique should prevent duplicates; got %d occurrences of 'prefers Go' in %v", count, mb.UserProfile.Preferences)
	}
}

func TestUserProfileIndexer_StopWaitsForInFlightFlush(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	indexer := NewUserProfileIndexer(mbStore, 1, time.Hour)
	go indexer.Start(context.Background())

	// Trigger a flush
	indexer.OnUserMessage(ChatMessage{
		Role:    "user",
		Content: "I prefer TypeScript",
	})

	// Stop should wait for the in-flight flush to complete
	indexer.Stop()

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "prefers TypeScript") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Stop should wait for in-flight flush; preference not found in %v", mb.UserProfile.Preferences)
	}
}

func TestUserProfileIndexer_NilStore_NoOp(t *testing.T) {
	indexer := NewUserProfileIndexer(nil, 1, time.Hour)

	// Should not panic
	indexer.OnUserMessage(ChatMessage{
		Role:    "user",
		Content: "I prefer anything",
	})

	indexer.Stop()
}

func TestUserProfileIndexer_ExtractsPathFacts(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	indexer := NewUserProfileIndexer(mbStore, 1, time.Hour)
	go indexer.Start(context.Background())
	defer indexer.Stop()

	indexer.OnUserMessage(ChatMessage{
		Role:    "user",
		Content: "Check the file at /home/user/project/main.go",
	})

	indexer.flushWg.Wait()

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "/home/user/project/main.go") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("path fact not extracted; got %v", mb.UserProfile.Preferences)
	}
}

func TestLoadSettledPreferences_ReadsFromMemoryBank(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	up := store.UserProfile{Preferences: []string{"prefers Go", "path: /home/user/project"}}
	if err := mbStore.SaveUserProfile(up); err != nil {
		t.Fatalf("SaveUserProfile: %v", err)
	}

	result := loadSettledPreferences(mbStore)
	if !strings.Contains(result, "prefers Go") {
		t.Fatalf("expected 'prefers Go' in settled preferences, got: %s", result)
	}
	if !strings.Contains(result, "/home/user/project") {
		t.Fatalf("expected path in settled preferences, got: %s", result)
	}
}

func TestLoadSettledPreferences_EmptyReturnsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	result := loadSettledPreferences(mbStore)
	if result != "" {
		t.Fatalf("expected empty string for empty profile, got: %s", result)
	}
}

func TestLoadSettledPreferences_NilStoreReturnsEmpty(t *testing.T) {
	result := loadSettledPreferences(nil)
	if result != "" {
		t.Fatalf("expected empty string for nil store, got: %s", result)
	}
}

func TestRollingWindowMemory_MemoryBankReadsSettledPreferences(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	up := store.UserProfile{Preferences: []string{"prefers Go"}}
	if err := mbStore.SaveUserProfile(up); err != nil {
		t.Fatalf("SaveUserProfile: %v", err)
	}

	mem := NewRollingWindowMemory(10)
	mem.MemoryBank = mbStore

	history := []ChatMessage{
		{ID: "1", Role: "user", Content: "Hello"},
	}

	got := mem.Process(context.Background(), "session1", history)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 messages (facts + history), got %d", len(got))
	}
	if got[0].Role != "system" {
		t.Fatalf("first message should be system facts, got role %s", got[0].Role)
	}
	if !strings.Contains(got[0].Content, "prefers Go") {
		t.Fatalf("system facts should contain settled preference 'prefers Go', got: %s", got[0].Content)
	}
}

// Ensure concurrent OnUserMessage calls are safe
func TestUserProfileIndexer_ConcurrentOnUserMessage(t *testing.T) {
	tmpDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(tmpDir, nil)

	indexer := NewUserProfileIndexer(mbStore, 50, time.Hour)
	go indexer.Start(context.Background())
	defer indexer.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			indexer.OnUserMessage(ChatMessage{
				Role:    "user",
				Content: "I prefer concurrency",
			})
		}()
	}
	wg.Wait()
	indexer.flushWg.Wait()

	mb, err := mbStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "prefers concurrency") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("concurrent OnUserMessage should still flush preferences; got %v", mb.UserProfile.Preferences)
	}
}
