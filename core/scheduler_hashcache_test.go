package core

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestPersistGraph_HashCacheConcurrentSafe is a regression test for the
// hashCache concurrent-map-access crash introduced when SHA-256 caching was
// added to persistGraph. persistGraph is invoked from parallel step
// goroutines (executeStep runs up to 10 steps of one graph concurrently),
// so the shared hashCache map MUST be guarded. Before the fix this test
// fails under `go test -race` with "concurrent map read and map write".
func TestPersistGraph_HashCacheConcurrentSafe(t *testing.T) {
	reg := &config.Registry{
		Roles:  make(map[string]*config.Role),
		Skills: make(map[string]*config.Skill),
		MCPs:   make(map[string]*config.MCPDef),
	}
	ts := &mockTaskStore{}

	tasksDir, _ := os.MkdirTemp("", "hashcache_race")
	logger, _ := NewLogger(tasksDir, nil)
	defer logger.Close()
	defer os.RemoveAll(tasksDir)

	engine := NewDirectedEngine(reg, ts, nil, nil, logger, nil, tasksDir, tasksDir, nil)
	defer engine.Stop()

	// Create a real artifact on disk so the SHA-256 branch executes.
	artifactPath := filepath.Join(tasksDir, "artifact.bin")
	if err := os.WriteFile(artifactPath, []byte("regression-payload"), 0644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	graph := &schemas.TaskGraph{
		TaskID: "race-task",
		Steps:  map[string]*schemas.Step{"s1": {ID: "s1"}},
		OutputFiles: []schemas.OutputFile{
			{StepID: "s1", Path: artifactPath, Format: "bin", IsPrimary: true},
		},
	}

	// Hammer persistGraph from many goroutines — the cache is shared across
	// all of them. Under -race the unsynchronized map access is a fatal
	// report; without the lock this also crashes at runtime once the
	// goroutines overlap.
	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			engine.persistGraph(graph)
		}()
	}
	wg.Wait()

	// Sanity: the hash was populated and is stable.
	engine.hashCacheMu.Lock()
	got, ok := engine.hashCache[artifactPath]
	engine.hashCacheMu.Unlock()
	if !ok || got == "" {
		t.Fatalf("expected cached SHA-256 for %s, got ok=%v hash=%q", artifactPath, ok, got)
	}
}
