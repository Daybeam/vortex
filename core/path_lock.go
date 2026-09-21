package core

import (
	"path/filepath"
	"sync"
)

// PathLockManager manages fine-grained read/write locks per file path.
// Zero external dependencies — pure sync.RWMutex. Different paths run
// fully parallel; only same-path steps serialize.
// See docs/completed/2026-09-13/PATH_LEVEL_MUTEX_CONCURRENCY_DESIGN.md
type PathLockManager struct {
	mu    sync.Mutex
	locks map[string]*sync.RWMutex
}

func NewPathLockManager() *PathLockManager {
	return &PathLockManager{
		locks: make(map[string]*sync.RWMutex),
	}
}

// Acquire obtains a lock for the given path. write=true for exclusive
// write lock, write=false for shared read lock. Returns an unlock func
// suitable for defer.
func (m *PathLockManager) Acquire(path string, write bool) func() {
	cleanPath := filepath.Clean(path)

	m.mu.Lock()
	rw, exists := m.locks[cleanPath]
	if !exists {
		rw = &sync.RWMutex{}
		m.locks[cleanPath] = rw
	}
	m.mu.Unlock()

	if write {
		rw.Lock()
		return func() { rw.Unlock() }
	}
	rw.RLock()
	return func() { rw.RUnlock() }
}

// Release removes the lock entry for a path, freeing memory.
// Call only when no goroutine is holding or waiting on the lock for this path.
// (audit: locks map was unbounded — every unique path created a permanent entry)
func (m *PathLockManager) Release(path string) {
	cleanPath := filepath.Clean(path)
	m.mu.Lock()
	delete(m.locks, cleanPath)
	m.mu.Unlock()
}
