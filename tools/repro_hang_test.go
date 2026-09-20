package tools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestReproHang(t *testing.T) {
	// 1. Setup minimal registry
	reg := &config.Registry{
		SOPs: make(map[string]*schemas.SOP),
	}
	reg.SOPs["test"] = &schemas.SOP{
		ID: "test",
		Triggers: []schemas.SOPTrigger{
			{Keywords: []string{"test", "hang", "repro"}},
		},
	}

	// 2. Setup minimal ExperienceStore
	es, _ := store.NewExperienceStore(t.TempDir(), nil, nil, nil, nil)
	// Add some patterns to make it work a bit
	for i := 0; i < 100; i++ {
		// Internal upsert bypasses lock for test setup if we are careful,
		// but let's just use a direct tool-like call if possible.
		// Actually ExperienceStore is exported.
		es.Mu.Lock()
		es.TaskPatterns[fmt.Sprintf("p%d", i)] = store.TaskPattern{
			ID:            fmt.Sprintf("p%d", i),
			SourceText:    "test hang reproduction",
			TaskType:      "test+capability",
			SampleCount:   10,
			AvgConfidence: 0.9,
		}
		es.Mu.Unlock()
	}

	taskText := "test hang reproduction"

	start := time.Now()
	// Simulate the code path in tools.go
	if candidates := core.MatchSOPCandidates(reg, taskText, 3); len(candidates) > 0 {
		taskText += core.FormatSOPHints(candidates)
	}

	jitCandidates := es.QueryJITCandidates(context.Background(), taskText, 5, 0.7, 3)

	duration := time.Since(start)
	t.Logf("Execution took %v, found %d JIT candidates", duration, len(jitCandidates))

	if duration > 1*time.Second {
		t.Errorf("Execution too slow: %v", duration)
	}
}
