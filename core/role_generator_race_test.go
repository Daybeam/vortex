package core

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestGetOrCreateRole_ConcurrentReadWithRegistryWrite_NoRace is the regression
// test for audit H1: GetOrCreateRole iterated g.registry.Roles WITHOUT holding
// g.registry.Mu, while CommitOps/ApplyPatch (reached via GenerateRole in the
// same file) mutates Roles under Mu.Lock. Two concurrent GetOrCreateRole calls
// — or one GetOrCreateRole reader racing a writer — is a concurrent map
// read+write, which is a fatal runtime panic.
//
// Before the fix this test fails under `go test -race` (the race detector
// reports a data race on registry.Roles). After the fix the iteration is
// wrapped in Mu.RLock/RUnlock and the race is gone.
//
// The writer here mutates Roles under Lock directly, mirroring what
// CommitOps->ApplyPatch does on the write side. The readers call
// GetOrCreateRole with a non-matching hash so they execute the full iteration
// (the racy read) before falling through to GenerateRole, which returns an
// error on a nil loader — the error is ignored because the race we are
// pinning lives in the iteration, not the generation.
func TestGetOrCreateRole_ConcurrentReadWithRegistryWrite_NoRace(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "main",
		Providers: map[string]*config.ProviderConfig{
			"main": {Provider: "openai", Model: "gpt-4o", APIKeyEnv: "X_NO_EXIST"},
		},
		Roles: map[string]*config.Role{
			"existing": {ID: "existing", Metadata: map[string]string{"hash": "fixedhash"}},
		},
	}

	logger := mustNewLogger(t, t.TempDir(), nil)
	defer logger.Close()
	gen := NewRoleGenerator(reg, nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	// Writer: mutate Roles under Lock, exactly as CommitOps->ApplyPatch does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			reg.Mu.Lock()
			reg.Roles[fmt.Sprintf("w%d", i)] = &config.Role{ID: fmt.Sprintf("w%d", i)}
			if i > 0 && i%2 == 0 {
				delete(reg.Roles, fmt.Sprintf("w%d", i-1))
			}
			reg.Mu.Unlock()
		}
	}()

	// Readers: each GetOrCreateRole iterates all of Roles (the racy read).
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = gen.GetOrCreateRole(ctx, "taskID", "stepID", "missing", "desc", "cap")
			}
		}()
	}

	wg.Wait()
}
