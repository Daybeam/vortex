package core

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPathLockManager_DifferentPathsParallel(t *testing.T) {
	m := NewPathLockManager()

	var wg sync.WaitGroup
	done := make(chan bool, 2)
	start := time.Now()

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(t.TempDir(), "file_"+string(rune('a'+i)))
			unlock := m.Acquire(path, true)
			defer unlock()
			time.Sleep(50 * time.Millisecond)
			done <- true
		}(i)
	}

	wg.Wait()
	close(done)

	elapsed := time.Since(start)
	if elapsed > 150*time.Millisecond {
		t.Errorf("different paths should run in parallel, took %v", elapsed)
	}
}

func TestPathLockManager_SamePathSerializes(t *testing.T) {
	m := NewPathLockManager()
	path := "/test/shared/file.txt"

	var wg sync.WaitGroup
	order := make([]int, 0, 2)
	var orderMu sync.Mutex

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock := m.Acquire(path, true)
			defer unlock()
			orderMu.Lock()
			order = append(order, i)
			orderMu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}(i)
	}

	wg.Wait()

	if len(order) != 2 {
		t.Fatalf("expected 2 writes, got %d", len(order))
	}
}

func TestPathLockManager_ReadLockShared(t *testing.T) {
	m := NewPathLockManager()
	path := "/test/shared/read.txt"

	var wg sync.WaitGroup
	done := make(chan bool, 2)
	start := time.Now()

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := m.Acquire(path, false)
			defer unlock()
			time.Sleep(50 * time.Millisecond)
			done <- true
		}()
	}

	wg.Wait()
	close(done)

	elapsed := time.Since(start)
	if elapsed > 150*time.Millisecond {
		t.Errorf("read locks should be shared, took %v", elapsed)
	}
}

func TestPathLockManager_WriteBlocksRead(t *testing.T) {
	m := NewPathLockManager()
	path := "/test/conflict.txt"

	writeAcquired := make(chan struct{})
	writeDone := make(chan struct{})
	readAttempted := make(chan struct{})
	readDone := make(chan struct{})

	go func() {
		unlock := m.Acquire(path, true)
		close(writeAcquired)
		defer unlock()
		time.Sleep(100 * time.Millisecond)
		close(writeDone)
	}()

	<-writeAcquired

	go func() {
		close(readAttempted)
		unlock := m.Acquire(path, false)
		defer unlock()
		close(readDone)
	}()

	<-readAttempted
	select {
	case <-readDone:
		t.Error("read should not complete while write lock is held")
	case <-time.After(50 * time.Millisecond):
	}

	<-writeDone
	<-readDone
}

func TestPathLockManager_PathNormalization(t *testing.T) {
	m := NewPathLockManager()

	unlock1 := m.Acquire("/test/../test/./file.txt", true)

	done := make(chan struct{})
	go func() {
		unlock2 := m.Acquire("/test/file.txt", true)
		defer unlock2()
		close(done)
	}()

	select {
	case <-done:
		t.Error("same path after Clean should be blocked by write lock")
	case <-time.After(30 * time.Millisecond):
	}

	unlock1()
	<-done
}

func TestPathLockManager_UnlockFunc(t *testing.T) {
	m := NewPathLockManager()
	path := "/test/unlock.txt"

	unlock := m.Acquire(path, true)
	unlock()

	done := make(chan struct{})
	go func() {
		unlock2 := m.Acquire(path, true)
		defer unlock2()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Error("second acquire should succeed after unlock")
	}
}
