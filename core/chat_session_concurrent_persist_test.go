package core

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestC6_ChatSession_ConcurrentPersistAppend is a regression test for
// audit C-6: ChatSessionStore.Persist must deep-copy the messages map under
// lock so that post-unlock iteration (JSON marshaling) does not race with
// concurrent AppendUserMessage/AppendMessage map writes.
//
// Before the fix, Persist used `data.Messages = s.Messages` which copies only
// the map header (same backing map), so post-unlock iteration raced with
// concurrent map writes (panic: concurrent map iteration and map write).
// After the fix, the map is deep-copied under lock.
//
// Reproduction: run concurrent Persist + AppendMessage and verify no panic.
// Run with `go test -race` to detect the data race.
func TestC6_ChatSession_ConcurrentPersistAppend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chat_concurrent")
	st := NewChatSessionStore(dir, nil)

	s := st.GetOrCreate("concurrent-session")

	var wg sync.WaitGroup
	const numGoroutines = 20

	// Concurrent appenders.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				msgID := "msg-" + strconv.Itoa(gid) + "-" + strconv.Itoa(j)
				s.AppendUserMessage(msgID, "content", "")
			}
		}(i)
	}

	// Concurrent persisters.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = st.Persist(s)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success: no panic or deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: concurrent Persist/Append deadlocked")
	}
}
