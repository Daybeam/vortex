// Package bootmetrics records process-startup phase timings so a slow boot can
// be attributed to a phase without a profiler. Marks are cheap, safe for
// concurrent use, exposed through /health and logged once when the server
// starts listening.
package bootmetrics

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Phase is one startup mark, timed relative to package initialization.
type Phase struct {
	Name    string `json:"name"`
	AtMs    int64  `json:"atMs"`
	DeltaMs int64  `json:"deltaMs"`
}

var (
	mu     sync.Mutex
	origin = time.Now()
	prev   = origin
	phases []Phase
)

// Mark records the time elapsed since the previous mark under name.
func Mark(name string) {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	phases = append(phases, Phase{
		Name:    name,
		AtMs:    now.Sub(origin).Milliseconds(),
		DeltaMs: now.Sub(prev).Milliseconds(),
	})
	prev = now
}

// SinceMs returns milliseconds elapsed since process start.
func SinceMs() int64 {
	return time.Since(origin).Milliseconds()
}

// Snapshot returns a copy of the recorded phases.
func Snapshot() []Phase {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Phase, len(phases))
	copy(out, phases)
	return out
}

// Summary renders every phase on one line, e.g.
// "total=13.80s config=0.12s db=0.35s ...".
func Summary() string {
	mu.Lock()
	defer mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "total=%.2fs", time.Since(origin).Seconds())
	for _, phase := range phases {
		fmt.Fprintf(&sb, " %s=%.2fs", phase.Name, float64(phase.DeltaMs)/1000)
	}
	return sb.String()
}
